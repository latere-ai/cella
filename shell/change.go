// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package shell

import (
	"errors"
	"io/fs"
	"path"
	"strings"
	"syscall"
)

// changeOps is an invocation whose operands are all paths it changes.
type changeOps struct {
	args     []string
	operands []int
}

func (c changeOps) paths() []int { return c.operands }

func (changeOps) cwd() bool { return false }

func (c changeOps) names() []string { return operandStrings(c.args, c.operands) }

// mkdir

type mkdirInv struct {
	changeOps
	parents, verbose bool
}

var mkdirSpec = optSpec{prog: "mkdir", short: "m:pv", code: 1, long: map[string]bool{
	"mode": true, "parents": false, "verbose": false,
}}

func parseMkdir(args []string) (invocation, error) {
	ps, err := mkdirSpec.parse(args)
	if err != nil {
		return nil, err
	}
	if len(ps.operands) == 0 {
		return nil, &usageError{prog: "mkdir", msg: "missing operand", code: 1}
	}
	inv := &mkdirInv{args: args, operands: ps.operands}
	for _, o := range ps.opts {
		switch o.name {
		case "p", "parents":
			inv.parents = true
		case "v", "verbose":
			inv.verbose = true
		}
	}
	return inv, nil
}

func (m *mkdirInv) run(p *proc) int {
	code := 0
	for _, name := range m.names() {
		abs := p.abs(name)
		if !m.parents {
			if err := p.s.files.mkdir(p.ctx, abs); err != nil {
				p.errorf("cannot create directory %s: %s", quoteLocale(name), errorText(err))
				code = 1
				continue
			}
			m.made(p, name)
			continue
		}
		if !m.makeAll(p, name, abs, true) {
			code = 1
		}
	}
	return code
}

// makeAll makes a directory and every parent it lacks, as mkdir -p does.
// A file in a parent's place is not a directory; in the place of the
// directory asked for, it exists.
func (m *mkdirInv) makeAll(p *proc, name, abs string, last bool) bool {
	if info, err := p.s.files.stat(p.ctx, abs); err == nil {
		if info.IsDir() {
			return true
		}
		why := syscall.ENOTDIR
		if last {
			why = syscall.EEXIST
		}
		p.errorf("cannot create directory %s: %s", quoteLocale(name), errorText(why))
		return false
	}
	parentName := path.Dir(strings.TrimSuffix(name, "/"))
	if parentAbs := path.Dir(abs); parentAbs != abs && p.s.files.serves(parentAbs) {
		if !m.makeAll(p, parentName, parentAbs, false) {
			return false
		}
	}
	if err := p.s.files.mkdir(p.ctx, abs); err != nil {
		p.errorf("cannot create directory %s: %s", quoteLocale(name), errorText(err))
		return false
	}
	m.made(p, name)
	return true
}

func (m *mkdirInv) made(p *proc, name string) {
	if m.verbose {
		p.printf("mkdir: created directory %s\n", quote(name))
	}
}

// rm

type rmInv struct {
	changeOps
	force, recursive, dirs, verbose bool
}

var rmSpec = optSpec{prog: "rm", short: "dfrRv", code: 1, long: map[string]bool{
	"dir": false, "force": false, "recursive": false, "verbose": false,
}}

func parseRm(args []string) (invocation, error) {
	ps, err := rmSpec.parse(args)
	if err != nil {
		return nil, err
	}
	inv := &rmInv{args: args, operands: ps.operands}
	for _, o := range ps.opts {
		switch o.name {
		case "f", "force":
			inv.force = true
		case "r", "R", "recursive":
			inv.recursive = true
		case "d", "dir":
			inv.dirs = true
		case "v", "verbose":
			inv.verbose = true
		}
	}
	if len(ps.operands) == 0 && !inv.force {
		return nil, &usageError{prog: "rm", msg: "missing operand", code: 1}
	}
	return inv, nil
}

func (r *rmInv) run(p *proc) int {
	code := 0
	for _, name := range r.names() {
		if base := path.Base(strings.TrimRight(name, "/")); base == "." || base == ".." {
			p.errorf("refusing to remove '.' or '..' directory: skipping %s", quote(name))
			code = 1
			continue
		}
		abs := p.abs(name)
		info, err := p.s.files.stat(p.ctx, abs)
		if err != nil {
			if !r.force || !errors.Is(err, fs.ErrNotExist) {
				p.errorf("cannot remove %s: %s", quote(name), errorText(err))
				code = 1
			}
			continue
		}
		if info.IsDir() {
			if !r.recursive && !r.dirs {
				p.errorf("cannot remove %s: %s", quote(name), errorText(syscall.EISDIR))
				code = 1
				continue
			}
			// A directory that holds a served folder cannot go: the
			// folder is mounted there. Nothing under it is removed.
			if p.s.files.holdsMount(abs) {
				p.errorf("cannot remove %s: %s", quote(name), errorText(syscall.EBUSY))
				code = 1
				continue
			}
			if !r.recursive {
				if err := p.s.files.rmdir(p.ctx, abs); err != nil {
					p.errorf("cannot remove %s: %s", quote(name), errorText(err))
					code = 1
				} else if r.verbose {
					p.printf("removed directory %s\n", quote(name))
				}
				continue
			}
		}
		if !r.removeTree(p, name, abs, info) {
			code = 1
		}
	}
	return code
}

// removeTree removes a file, or a directory and what is under it, first
// what is under it; false when anything stayed.
func (r *rmInv) removeTree(p *proc, name, abs string, info fs.FileInfo) bool {
	if !info.IsDir() {
		if err := p.s.files.remove(p.ctx, abs); err != nil {
			p.errorf("cannot remove %s: %s", quote(name), errorText(err))
			return false
		}
		if r.verbose {
			p.printf("removed %s\n", quote(name))
		}
		return true
	}
	entries, err := p.s.files.readDir(p.ctx, abs)
	if err != nil {
		p.errorf("cannot remove %s: %s", quote(name), errorText(err))
		return false
	}
	sortInfos(entries, p.byteOrder())
	ok := true
	for _, e := range entries {
		if !r.removeTree(p, joinShown(name, e.Name()), path.Join(abs, e.Name()), e) {
			ok = false
		}
	}
	if !ok {
		// GNU rm leaves a directory it could not empty without a word.
		return false
	}
	if err := p.s.files.rmdir(p.ctx, abs); err != nil {
		p.errorf("cannot remove %s: %s", quote(name), errorText(err))
		return false
	}
	if r.verbose {
		p.printf("removed directory %s\n", quote(name))
	}
	return true
}

// joinShown is a name under a directory as a program prints it: the
// directory's name as given, then the entry.
func joinShown(dir, name string) string {
	if strings.HasSuffix(dir, "/") {
		return dir + name
	}
	return dir + "/" + name
}

// mv and cp

// transfer is mv or cp: sources and a target, which when it is a
// directory receives each source under its own name.
type transfer struct {
	changeOps
	noClobber, verbose bool
	recursive          bool // cp only
}

var (
	mvSpec = optSpec{prog: "mv", short: "fnv", code: 1, long: map[string]bool{
		"force": false, "no-clobber": false, "verbose": false,
	}}
	cpSpec = optSpec{prog: "cp", short: "afnpRrv", code: 1, long: map[string]bool{
		"archive": false, "force": false, "no-clobber": false, "recursive": false, "verbose": false,
	}}
)

type (
	mvInv struct{ transfer }
	cpInv struct{ transfer }
)

func parseTransfer(spec optSpec, args []string) (transfer, error) {
	ps, err := spec.parse(args)
	if err != nil {
		return transfer{}, err
	}
	t := transfer{args: args, operands: ps.operands}
	for _, o := range ps.opts {
		switch o.name {
		case "n", "no-clobber":
			t.noClobber = true
		case "f", "force":
			t.noClobber = false
		case "v", "verbose":
			t.verbose = true
		case "r", "R", "a", "recursive", "archive":
			t.recursive = true
		}
	}
	switch len(ps.operands) {
	case 0:
		return t, &usageError{prog: spec.prog, msg: "missing file operand", code: 1}
	case 1:
		return t, &usageError{prog: spec.prog, msg: "missing destination file operand after " + quote(args[ps.operands[0]]), code: 1}
	}
	return t, nil
}

func parseMv(args []string) (invocation, error) {
	t, err := parseTransfer(mvSpec, args)
	if err != nil {
		return nil, err
	}
	return &mvInv{t}, nil
}

func parseCp(args []string) (invocation, error) {
	t, err := parseTransfer(cpSpec, args)
	if err != nil {
		return nil, err
	}
	return &cpInv{t}, nil
}

// pairs are the transfer's sources, each with the path it goes to, or
// false when the target cannot take them.
func (t *transfer) pairs(p *proc) (sources []string, dests []string, ok bool) {
	names := t.names()
	target := names[len(names)-1]
	sources = names[:len(names)-1]
	targetDir, _ := p.s.files.isDir(p.ctx, p.abs(target))
	if len(sources) > 1 && !targetDir {
		p.errorf("target %s: %s", quote(target), errorText(syscall.ENOTDIR))
		return nil, nil, false
	}
	for _, s := range sources {
		if targetDir {
			dests = append(dests, joinShown(target, path.Base(strings.TrimRight(s, "/"))))
		} else {
			dests = append(dests, target)
		}
	}
	return sources, dests, true
}

func (m *mvInv) run(p *proc) int {
	sources, dests, ok := m.pairs(p)
	if !ok {
		return 1
	}
	code := 0
	for i, src := range sources {
		if !m.move(p, src, dests[i]) {
			code = 1
		}
	}
	return code
}

func (m *mvInv) move(p *proc, src, dst string) bool {
	from, to := p.abs(src), p.abs(dst)
	info, err := p.s.files.stat(p.ctx, from)
	if err != nil {
		p.errorf("cannot stat %s: %s", quote(src), errorText(err))
		return false
	}
	if from == to {
		p.errorf("%s and %s are the same file", quote(src), quote(dst))
		return false
	}
	if info.IsDir() && within(to, from) {
		p.errorf("cannot move %s to a subdirectory of itself, %s", quote(src), quote(dst))
		return false
	}
	failed := func(err error) bool {
		p.errorf("cannot move %s to %s: %s", quote(src), quote(dst), errorText(err))
		return false
	}
	if p.s.files.holdsMount(from) {
		return failed(syscall.EBUSY)
	}
	if fm, tm := p.s.files.mountOf(from), p.s.files.mountOf(to); fm.Dir != tm.Dir {
		return m.moveAcross(p, src, dst, info, tm)
	}
	if existing, err := p.s.files.stat(p.ctx, to); err == nil {
		switch {
		case m.noClobber:
			p.errorf("not replacing %s", quote(dst))
			return false
		case info.IsDir() && !existing.IsDir():
			p.errorf("cannot overwrite non-directory %s with directory %s", quote(dst), quote(src))
			return false
		case !info.IsDir() && existing.IsDir():
			p.errorf("cannot overwrite directory %s with non-directory %s", quote(dst), quote(src))
			return false
		}
	}
	if info.IsDir() {
		if err := p.s.files.renameTree(p.ctx, from, to); err != nil {
			return failed(err)
		}
	} else if err := p.s.files.rename(p.ctx, from, to); err != nil {
		return failed(err)
	}
	if m.verbose {
		p.printf("renamed %s -> %s\n", quote(src), quote(dst))
	}
	return true
}

// moveAcross moves a file or a directory to another mount, as mv moves one
// to another device: it copies it, then removes the original.
func (m *mvInv) moveAcross(p *proc, src, dst string, info fs.FileInfo, target Mount) bool {
	to := p.abs(dst)
	parent, err := p.s.files.stat(p.ctx, path.Dir(to))
	switch {
	case err != nil:
		p.errorf("cannot move %s to %s: %s", quote(src), quote(dst), errorText(err))
		return false
	case !parent.IsDir():
		p.errorf("cannot move %s to %s: %s", quote(src), quote(dst), errorText(syscall.ENOTDIR))
		return false
	case target.ReadOnly:
		p.errorf("inter-device move failed: %s to %s; unable to remove target: %s", quote(src), quote(dst), errorText(syscall.EROFS))
		return false
	}
	cp := &cpInv{transfer{recursive: true}}
	if !cp.copy(p, src, dst, info) {
		return false
	}
	rm := &rmInv{recursive: true}
	if !rm.removeTree(p, src, p.abs(src), info) {
		return false
	}
	if m.verbose {
		p.printf("renamed %s -> %s\n", quote(src), quote(dst))
	}
	return true
}

func (c *cpInv) run(p *proc) int {
	if c.noClobber {
		p.errorf("warning: behavior of -n is non-portable and may change in future; use --update=none instead")
	}
	sources, dests, ok := c.pairs(p)
	if !ok {
		return 1
	}
	code := 0
	for i, src := range sources {
		info, err := p.s.files.stat(p.ctx, p.abs(src))
		if err != nil {
			p.errorf("cannot stat %s: %s", quote(src), errorText(err))
			code = 1
			continue
		}
		if info.IsDir() && !c.recursive {
			p.errorf("-r not specified; omitting directory %s", quote(src))
			code = 1
			continue
		}
		if !c.copy(p, src, dests[i], info) {
			code = 1
		}
	}
	return code
}

// copy copies a file, or a directory and what is under it.
func (c *cpInv) copy(p *proc, src, dst string, info fs.FileInfo) bool {
	from, to := p.abs(src), p.abs(dst)
	if from == to {
		p.errorf("%s and %s are the same file", quote(src), quote(dst))
		return false
	}
	existing, statErr := p.s.files.stat(p.ctx, to)
	if !info.IsDir() {
		if statErr == nil {
			switch {
			case existing.IsDir():
				p.errorf("cannot overwrite directory %s with non-directory", quote(dst))
				return false
			case c.noClobber:
				return true
			}
		}
		data, err := p.s.files.read(p.ctx, from)
		if err != nil {
			p.errorf("cannot open %s for reading: %s", quote(src), errorText(err))
			return false
		}
		if err := p.s.files.write(p.ctx, to, data); err != nil {
			p.errorf("cannot create regular file %s: %s", quote(dst), errorText(err))
			return false
		}
		if c.verbose {
			p.printf("%s -> %s\n", quote(src), quote(dst))
		}
		return true
	}
	if within(to, from) {
		p.errorf("cannot copy a directory, %s, into itself, %s", quote(src), quote(dst))
		return false
	}
	switch {
	case statErr == nil && !existing.IsDir():
		p.errorf("cannot overwrite non-directory %s with directory %s", quote(dst), quote(src))
		return false
	case statErr != nil:
		if err := p.s.files.mkdir(p.ctx, to); err != nil {
			p.errorf("cannot create directory %s: %s", quote(dst), errorText(err))
			return false
		}
		if c.verbose {
			p.printf("%s -> %s\n", quote(src), quote(dst))
		}
	}
	entries, err := p.s.files.readDir(p.ctx, from)
	if err != nil {
		p.errorf("cannot access %s: %s", quote(src), errorText(err))
		return false
	}
	sortInfos(entries, p.byteOrder())
	ok := true
	for _, e := range entries {
		if !c.copy(p, joinShown(src, e.Name()), joinShown(dst, e.Name()), e) {
			ok = false
		}
	}
	return ok
}

// touch

type touchInv struct {
	changeOps
	noCreate bool
}

var touchSpec = optSpec{prog: "touch", short: "acm", code: 1, long: map[string]bool{
	"no-create": false,
}}

func parseTouch(args []string) (invocation, error) {
	ps, err := touchSpec.parse(args)
	if err != nil {
		return nil, err
	}
	if len(ps.operands) == 0 {
		return nil, &usageError{prog: "touch", msg: "missing file operand", code: 1}
	}
	inv := &touchInv{args: args, operands: ps.operands}
	for _, o := range ps.opts {
		if o.name == "c" || o.name == "no-create" {
			inv.noCreate = true
		}
	}
	return inv, nil
}

// run makes each file that is missing, empty. A file that is there is
// left as it is: the file system keeps the time of its last write.
func (t *touchInv) run(p *proc) int {
	code := 0
	for _, name := range t.names() {
		abs := p.abs(name)
		_, err := p.s.files.stat(p.ctx, abs)
		switch {
		case err == nil:
			if p.s.files.readOnly(abs) {
				p.errorf("cannot touch %s: %s", quote(name), errorText(syscall.EROFS))
				code = 1
			}
		case !errors.Is(err, fs.ErrNotExist):
			p.errorf("cannot touch %s: %s", quote(name), errorText(err))
			code = 1
		case t.noCreate:
		default:
			if err := p.s.files.write(p.ctx, abs, nil); err != nil {
				p.errorf("cannot touch %s: %s", quote(name), errorText(err))
				code = 1
			}
		}
	}
	return code
}
