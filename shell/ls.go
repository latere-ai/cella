// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package shell

import (
	"cmp"
	"fmt"
	"io/fs"
	"path"
	"slices"
	"strconv"
	"strings"
	"time"
)

type lsInv struct {
	args     []string
	operands []int

	all, almostAll  bool
	long, recursive bool
	human           bool
	byTime, bySize  bool
	unsorted        bool
	reverse         bool
	dirsAsFiles     bool
	classify, slash bool
	noGroup         bool
	numeric         bool
}

var lsSpec = optSpec{prog: "ls", short: "1aAdFGhlnpRrStU", code: 2, long: map[string]bool{
	"all": false, "almost-all": false, "directory": false, "classify": false, "no-group": false,
	"human-readable": false, "numeric-uid-gid": false, "recursive": false, "reverse": false,
	"color": false,
}}

func parseLs(args []string) (invocation, error) {
	// --color=WHEN is read here, as grep reads it. Output never reaches a
	// terminal, so auto is never; always, and --color alone, which means
	// always, are not taken.
	read := make([]string, 0, len(args))
	dashes := false
	for _, a := range args {
		switch {
		case dashes:
		case a == "--":
			dashes = true
		case a == "--color":
			return nil, &usageError{prog: "ls", msg: "colored output is not available", code: 2}
		case strings.HasPrefix(a, "--color="):
			switch v := strings.TrimPrefix(a, "--color="); v {
			case "never", "no", "none", "auto", "tty", "if-tty":
				a = "--color"
			default:
				return nil, &usageError{prog: "ls", msg: "colored output is not available", code: 2}
			}
		}
		read = append(read, a)
	}
	ps, err := lsSpec.parse(read)
	if err != nil {
		return nil, err
	}
	inv := &lsInv{args: args, operands: ps.operands}
	for _, o := range ps.opts {
		switch o.name {
		case "a", "all":
			inv.all, inv.almostAll = true, false
		case "A", "almost-all":
			inv.almostAll, inv.all = true, false
		case "d", "directory":
			inv.dirsAsFiles = true
		case "F", "classify":
			inv.classify = true
		case "p":
			inv.slash = true
		case "G", "no-group":
			inv.noGroup = true
		case "h", "human-readable":
			inv.human = true
		case "l":
			inv.long = true
		case "n", "numeric-uid-gid":
			inv.long, inv.numeric = true, true
		case "R", "recursive":
			inv.recursive = true
		case "r", "reverse":
			inv.reverse = true
		case "S":
			inv.bySize, inv.byTime, inv.unsorted = true, false, false
		case "t":
			inv.byTime, inv.bySize, inv.unsorted = true, false, false
		case "U":
			inv.unsorted, inv.byTime, inv.bySize = true, false, false
		}
	}
	return inv, nil
}

func (l *lsInv) paths() []int { return l.operands }

func (l *lsInv) cwd() bool { return len(l.operands) == 0 }

// lsEntry is one name ls prints: as the operand gave it or as the
// directory holds it.
type lsEntry struct {
	shown string
	abs   string
	info  fs.FileInfo
}

// listing is a group of entries printed together: the file operands, or
// one directory's entries.
type listing struct {
	entries []lsEntry
	// total prints the blocks the entries take first, as a directory's
	// listing does and the file operands' does not.
	total bool
}

func (l *lsInv) run(p *proc) int {
	code := 0
	names := operandStrings(l.args, l.operands)
	if len(names) == 0 {
		names = []string{"."}
	}
	var fileOps, dirs []lsEntry
	for _, name := range names {
		abs := p.abs(name)
		info, err := p.s.files.stat(p.ctx, abs)
		if err != nil {
			p.errorf("cannot access %s: %s", quote(name), errorText(err))
			code = 2
			continue
		}
		e := lsEntry{shown: name, abs: abs, info: info}
		if info.IsDir() && !l.dirsAsFiles {
			dirs = append(dirs, e)
		} else {
			fileOps = append(fileOps, e)
		}
	}
	byteOrder := p.byteOrder()
	l.sort(fileOps, byteOrder)
	l.sort(dirs, byteOrder)

	var out strings.Builder
	printed := false
	if len(fileOps) > 0 {
		l.print(p, &out, listing{entries: fileOps})
		printed = true
	}
	headers := l.recursive || len(names) > 1
	for _, d := range dirs {
		code = max(code, l.listDir(p, &out, d, headers, &printed, byteOrder, true))
	}
	p.write([]byte(out.String()))
	return code
}

// listDir prints one directory's entries and, under -R, its
// subdirectories' after them, and answers ls's status: 2 when a directory
// the line named could not be read, 1 when one under it could not.
func (l *lsInv) listDir(p *proc, out *strings.Builder, d lsEntry, header bool, printed *bool, byteOrder bool, named bool) int {
	infos, err := p.s.files.readDir(p.ctx, d.abs)
	if err != nil {
		p.errorf("cannot open directory %s: %s", quote(d.shown), errorText(err))
		if named {
			return 2
		}
		return 1
	}
	var entries []lsEntry
	if l.all {
		entries = append(entries, lsEntry{shown: ".", abs: d.abs, info: d.info})
		parent := path.Dir(d.abs)
		pinfo, err := p.s.files.stat(p.ctx, parent)
		if err != nil {
			// The parent of a served folder is the machine's; it is shown
			// as a directory like the folder itself.
			pinfo = NewFileInfo("..", 0, fs.ModeDir|0o755, d.info.ModTime())
		}
		entries = append(entries, lsEntry{shown: "..", abs: parent, info: pinfo})
	}
	for _, info := range infos {
		if strings.HasPrefix(info.Name(), ".") && !l.all && !l.almostAll {
			continue
		}
		entries = append(entries, lsEntry{shown: info.Name(), abs: path.Join(d.abs, info.Name()), info: info})
	}
	l.sort(entries, byteOrder)
	if *printed {
		out.WriteByte('\n')
	}
	if header {
		out.WriteString(d.shown + ":\n")
	}
	*printed = true
	l.print(p, out, listing{entries: entries, total: true})
	if !l.recursive {
		return 0
	}
	code := 0
	for _, e := range entries {
		if !e.info.IsDir() || e.shown == "." || e.shown == ".." {
			continue
		}
		sub := lsEntry{shown: joinShown(d.shown, e.shown), abs: e.abs, info: e.info}
		code = max(code, l.listDir(p, out, sub, true, printed, byteOrder, false))
	}
	return code
}

// sort orders entries by name, newest first under -t, largest first under
// -S, as the directory holds them under -U, and the other way under -r.
func (l *lsInv) sort(entries []lsEntry, byteOrder bool) {
	if l.unsorted {
		return
	}
	slices.SortStableFunc(entries, func(a, b lsEntry) int {
		var c int
		switch {
		case l.byTime:
			c = b.info.ModTime().Compare(a.info.ModTime())
		case l.bySize:
			c = cmp.Compare(lsSize(b.info), lsSize(a.info))
		}
		if c == 0 {
			c = compareNames(a.shown, b.shown, byteOrder)
		}
		if l.reverse {
			return -c
		}
		return c
	})
}

// lsSize is the size ls shows: a directory is one block.
func lsSize(info fs.FileInfo) int64 {
	if info.IsDir() && info.Size() == 0 {
		return blockSize
	}
	return info.Size()
}

// blockSize is the block a file system of a sandbox image allocates.
const blockSize = 4096

// blocks is the space a file takes, in KiB, as ls -l totals it.
func blocks(info fs.FileInfo) int64 {
	return (lsSize(info) + blockSize - 1) / blockSize * (blockSize / 1024)
}

func (l *lsInv) suffix(info fs.FileInfo) string {
	if (l.classify || l.slash) && info.IsDir() {
		return "/"
	}
	return ""
}

func (l *lsInv) print(p *proc, out *strings.Builder, g listing) {
	if !l.long {
		for _, e := range g.entries {
			out.WriteString(e.shown + l.suffix(e.info) + "\n")
		}
		return
	}
	owner := p.s.owner
	if l.numeric {
		owner = "0"
	}
	type row struct {
		mode, links, size, date, name string
	}
	rows := make([]row, len(g.entries))
	var total int64
	linkW, sizeW := 0, 0
	for i, e := range g.entries {
		total += blocks(e.info)
		r := row{
			mode:  modeString(e.info),
			links: strconv.Itoa(l.links(p, e)),
			size:  l.sizeText(lsSize(e.info)),
			date:  lsDate(e.info.ModTime(), p.s.now),
			name:  e.shown + l.suffix(e.info),
		}
		linkW, sizeW = max(linkW, len(r.links)), max(sizeW, len(r.size))
		rows[i] = r
	}
	if g.total {
		if l.human {
			fmt.Fprintf(out, "total %s\n", humanSize(total*1024))
		} else {
			fmt.Fprintf(out, "total %d\n", total)
		}
	}
	for _, r := range rows {
		group := " " + owner
		if l.noGroup {
			group = ""
		}
		fmt.Fprintf(out, "%s %*s %s%s %*s %s %s\n", r.mode, linkW, r.links, owner, group, sizeW, r.size, r.date, r.name)
	}
}

// links is a file's link count: one for a file; for a directory, its
// own entry, its parent's, and each subdirectory's.
func (l *lsInv) links(p *proc, e lsEntry) int {
	if !e.info.IsDir() {
		return 1
	}
	n := 2
	infos, err := p.s.files.readDir(p.ctx, e.abs)
	if err != nil {
		return n
	}
	for _, i := range infos {
		if i.IsDir() {
			n++
		}
	}
	return n
}

func (l *lsInv) sizeText(n int64) string {
	if l.human {
		return humanSize(n)
	}
	return strconv.FormatInt(n, 10)
}

// humanSize is a size as ls -h shows it: rounded up, to one decimal below
// ten of its unit.
func humanSize(n int64) string {
	if n < 1024 {
		return strconv.FormatInt(n, 10)
	}
	units := "KMGTPE"
	v := float64(n)
	u := -1
	for v >= 1024 && u < len(units)-1 {
		v /= 1024
		u++
	}
	if v < 10 {
		tenths := ceilTenths(v)
		if tenths < 100 {
			return fmt.Sprintf("%d.%d%c", tenths/10, tenths%10, units[u])
		}
		v = 10
	}
	whole := int64(v)
	if float64(whole) < v {
		whole++
	}
	if whole >= 1024 && u < len(units)-1 {
		return fmt.Sprintf("1.0%c", units[u+1])
	}
	return fmt.Sprintf("%d%c", whole, units[u])
}

// ceilTenths is v in tenths, rounded up.
func ceilTenths(v float64) int64 {
	t := v * 10
	whole := int64(t)
	if float64(whole) < t-1e-9 {
		whole++
	}
	return whole
}

func modeString(info fs.FileInfo) string {
	m := info.Mode()
	b := []byte("----------")
	if m.IsDir() {
		b[0] = 'd'
	}
	const rwx = "rwxrwxrwx"
	for i := range 9 {
		if m.Perm()&(1<<(8-i)) != 0 {
			b[i+1] = rwx[i]
		}
	}
	return string(b)
}

// lsDate is a time as ls -l shows it: month, day and time for one in the
// last six months, the year in the place of the time otherwise.
func lsDate(t, now time.Time) string {
	t = t.UTC()
	const sixMonths = 31556952 * time.Second / 2
	if t.After(now) || now.Sub(t) > sixMonths {
		return t.Format("Jan _2  2006")
	}
	return t.Format("Jan _2 15:04")
}
