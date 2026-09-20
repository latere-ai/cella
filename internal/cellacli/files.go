// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package cellacli

import (
	"archive/tar"
	"context"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"strings"

	"latere.ai/x/cella/internal/cellaclient"
)

// reference is one side of a transfer: a path inside a sandbox, or a path on
// the caller's own disk.
type reference struct {
	// ref is the sandbox, empty for a local path.
	ref string
	// path is the file or directory.
	path string
}

// remote reports whether this side is inside a sandbox.
func (r reference) remote() bool { return r.ref != "" }

// parseReference reads `<ref>:<path>` as a path inside a sandbox and
// anything else as a local one. A Windows drive letter is not a concern
// here: the sandbox reference of design 008 is a name or a prefixed id, and
// both are longer than one character.
func parseReference(arg string) reference {
	ref, p, ok := strings.Cut(arg, ":")
	if !ok || len(ref) < 2 || !strings.HasPrefix(p, "/") {
		return reference{path: arg}
	}
	return reference{ref: ref, path: p}
}

// copyFiles is `cella cp`: a tree in either direction, streamed as tar with
// no temporary file between.
func copyFiles(ctx context.Context, c *invocation, args []string) error {
	fs := c.flags("cella cp <ref>:<src> <dest> | cella cp <src> <ref>:<dest>")
	rest, err := parse(fs, args)
	if err != nil {
		return err
	}
	if len(rest) != 2 {
		return usagef("cp needs a source and a destination")
	}
	source, destination := parseReference(rest[0]), parseReference(rest[1])
	client, err := c.client()
	if err != nil {
		return err
	}
	switch {
	case source.remote() && destination.remote():
		return usagef("cp copies between a sandbox and this machine, not between two sandboxes")
	case source.remote():
		return c.download(ctx, client, source, destination.path)
	case destination.remote():
		return c.upload(ctx, client, source.path, destination)
	default:
		return usagef("one side of cp names a sandbox as <ref>:<path>")
	}
}

// download streams the archive of one path and extracts it under the
// destination directory.
func (c *invocation) download(ctx context.Context, client *cellaclient.Client, source reference, destination string) error {
	stream, err := client.ExportTar(ctx, source.ref, []string{source.path})
	if err != nil {
		return err
	}
	defer func() { _ = stream.Close() }()
	written, err := extract(stream, destination)
	if err != nil {
		return err
	}
	if err = stream.Err(); err != nil {
		return err
	}
	if c.json {
		return writeValue(c.Stdout, map[string]any{"files": written, "path": destination})
	}
	_, err = fmt.Fprintf(c.Stdout, "%d file(s) written under %s\n", written, destination)
	return err
}

// upload streams a local path as an archive extracted below the
// destination.
func (c *invocation) upload(ctx context.Context, client *cellaclient.Client, source string, destination reference) error {
	info, err := os.Stat(source)
	if err != nil {
		return usageError{err}
	}
	reader, writer := io.Pipe()
	go func() { _ = writer.CloseWithError(archive(source, info, writer)) }()
	defer func() { _ = reader.Close() }()
	if err = client.ImportTar(ctx, destination.ref, destination.path, reader); err != nil {
		return err
	}
	if c.json {
		return writeValue(c.Stdout, map[string]any{"path": destination.path})
	}
	_, err = fmt.Fprintf(c.Stdout, "%s written under %s:%s\n", source, destination.ref, destination.path)
	return err
}

// archive writes one file or one tree as a tar stream, with the names
// relative to the source's own parent, which is what extracting below a
// destination directory means.
func archive(source string, info os.FileInfo, w io.Writer) error {
	tw := tar.NewWriter(w)
	base := filepath.Dir(source)
	walk := func(p string, entry os.FileInfo) error {
		name, err := filepath.Rel(base, p)
		if err != nil {
			return err
		}
		header, err := tar.FileInfoHeader(entry, "")
		if err != nil {
			return err
		}
		header.Name = filepath.ToSlash(name)
		if entry.IsDir() {
			header.Name += "/"
		}
		if err = tw.WriteHeader(header); err != nil {
			return err
		}
		if entry.IsDir() || !entry.Mode().IsRegular() {
			return nil
		}
		f, err := os.Open(p)
		if err != nil {
			return err
		}
		defer func() { _ = f.Close() }()
		_, err = io.Copy(tw, f)
		return err
	}
	if !info.IsDir() {
		if err := walk(source, info); err != nil {
			return err
		}
		return tw.Close()
	}
	err := filepath.WalkDir(source, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		entry, err := d.Info()
		if err != nil {
			return err
		}
		return walk(p, entry)
	})
	if err != nil {
		return err
	}
	return tw.Close()
}

// extract writes an archive under a directory, refusing a name that would
// leave it. The count is how many regular files were written.
func extract(r io.Reader, destination string) (int, error) {
	if err := os.MkdirAll(destination, 0o755); err != nil {
		return 0, err
	}
	root, err := filepath.Abs(destination)
	if err != nil {
		return 0, err
	}
	tr := tar.NewReader(r)
	written := 0
	for {
		header, err := tr.Next()
		if err == io.EOF {
			return written, nil
		}
		if err != nil {
			return written, err
		}
		target := filepath.Join(root, filepath.FromSlash(path.Clean("/"+header.Name)))
		if !strings.HasPrefix(target, root) {
			return written, fmt.Errorf("the archive names %q, which is outside %s", header.Name, destination)
		}
		switch header.Typeflag {
		case tar.TypeDir:
			if err = os.MkdirAll(target, fs.FileMode(header.Mode).Perm()|0o700); err != nil {
				return written, err
			}
		case tar.TypeReg:
			if err = os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
				return written, err
			}
			f, err := os.OpenFile(target, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, fs.FileMode(header.Mode).Perm())
			if err != nil {
				return written, err
			}
			if _, err = io.Copy(f, tr); err != nil {
				_ = f.Close()
				return written, err
			}
			if err = f.Close(); err != nil {
				return written, err
			}
			written++
		}
	}
}

// files is the granular half of design 008's file routes, one subcommand per
// route.
func files(ctx context.Context, c *invocation, args []string) error {
	fs := c.flags("cella files ls|stat|get|put|mkdir|rm|mv <ref>:<path> [...]")
	mode := fs.String("mode", "", "the mode a written file takes, octal text")
	rest, err := parse(fs, args)
	if err != nil {
		return err
	}
	if len(rest) == 0 {
		return usagef("files takes one of ls, stat, get, put, mkdir, rm, mv")
	}
	client, err := c.client()
	if err != nil {
		return err
	}
	sub, rest := rest[0], rest[1:]
	switch sub {
	case "ls":
		return c.fileList(ctx, client, rest)
	case "stat":
		return c.fileStat(ctx, client, rest)
	case "get":
		return c.fileGet(ctx, client, rest)
	case "put":
		return c.filePut(ctx, client, rest, *mode)
	case "mkdir":
		return c.fileMkdir(ctx, client, rest)
	case "rm":
		return c.fileRemove(ctx, client, rest)
	case "mv":
		return c.fileMove(ctx, client, rest)
	default:
		return usagef("files takes one of ls, stat, get, put, mkdir, rm, mv, and %q is none of them", sub)
	}
}

// inside reads one `<ref>:<path>` argument, which every file subcommand
// takes at least one of.
func inside(args []string, want int, verb string) ([]reference, error) {
	if len(args) != want {
		return nil, usagef("files %s needs %d argument(s) of the form <ref>:<path>", verb, want)
	}
	out := make([]reference, 0, want)
	for _, arg := range args {
		r := parseReference(arg)
		if !r.remote() {
			return nil, usagef("%q names no sandbox; the form is <ref>:/workspace/...", arg)
		}
		out = append(out, r)
	}
	return out, nil
}

func (c *invocation) fileList(ctx context.Context, client *cellaclient.Client, args []string) error {
	refs, err := inside(args, 1, "ls")
	if err != nil {
		return err
	}
	entries, raw, err := client.FileList(ctx, refs[0].ref, refs[0].path)
	if err != nil {
		return err
	}
	if c.json {
		return writeRaw(c.Stdout, raw)
	}
	return writeEntries(c.Stdout, entries)
}

func (c *invocation) fileStat(ctx context.Context, client *cellaclient.Client, args []string) error {
	refs, err := inside(args, 1, "stat")
	if err != nil {
		return err
	}
	entry, raw, err := client.FileStat(ctx, refs[0].ref, refs[0].path)
	if err != nil {
		return err
	}
	if c.json {
		return writeRaw(c.Stdout, raw)
	}
	return writeEntries(c.Stdout, []cellaclient.FileEntry{entry})
}

// fileGet writes one file to a destination on disk, or to standard output
// where none is named.
func (c *invocation) fileGet(ctx context.Context, client *cellaclient.Client, args []string) error {
	if len(args) == 0 || len(args) > 2 {
		return usagef("files get needs <ref>:<path> and an optional destination")
	}
	refs, err := inside(args[:1], 1, "get")
	if err != nil {
		return err
	}
	body, err := client.FileGet(ctx, refs[0].ref, refs[0].path)
	if err != nil {
		return err
	}
	defer func() { _ = body.Close() }()
	destination := c.Stdout
	name := "-"
	if len(args) == 2 && args[1] != "-" {
		name = args[1]
		f, err := os.Create(name)
		if err != nil {
			return usageError{err}
		}
		defer func() { _ = f.Close() }()
		destination = f
	}
	written, err := io.Copy(destination, body)
	if err != nil {
		return err
	}
	if c.json && name != "-" {
		return writeValue(c.Stdout, map[string]any{"path": name, "bytes": written})
	}
	return nil
}

// filePut writes one file from disk or from standard input.
func (c *invocation) filePut(ctx context.Context, client *cellaclient.Client, args []string, mode string) error {
	if len(args) != 2 {
		return usagef("files put needs a source and <ref>:<path>")
	}
	destination := parseReference(args[1])
	if !destination.remote() {
		return usagef("%q names no sandbox; the form is <ref>:/workspace/...", args[1])
	}
	var body io.Reader = c.Stdin
	if args[0] != "-" {
		f, err := os.Open(args[0])
		if err != nil {
			return usageError{err}
		}
		defer func() { _ = f.Close() }()
		body = f
	}
	if err := client.FilePut(ctx, destination.ref, destination.path, mode, body); err != nil {
		return err
	}
	return c.wrote(destination, "written")
}

func (c *invocation) fileMkdir(ctx context.Context, client *cellaclient.Client, args []string) error {
	refs, err := inside(args, 1, "mkdir")
	if err != nil {
		return err
	}
	if err = client.FileMkdir(ctx, refs[0].ref, refs[0].path); err != nil {
		return err
	}
	return c.wrote(refs[0], "made")
}

func (c *invocation) fileRemove(ctx context.Context, client *cellaclient.Client, args []string) error {
	refs, err := inside(args, 1, "rm")
	if err != nil {
		return err
	}
	if err = client.FileRemove(ctx, refs[0].ref, refs[0].path); err != nil {
		return err
	}
	return c.wrote(refs[0], "removed")
}

func (c *invocation) fileMove(ctx context.Context, client *cellaclient.Client, args []string) error {
	refs, err := inside(args, 2, "mv")
	if err != nil {
		return err
	}
	if refs[0].ref != refs[1].ref {
		return usagef("a move has both ends in one sandbox")
	}
	if err = client.FileMove(ctx, refs[0].ref, refs[0].path, refs[1].path); err != nil {
		return err
	}
	return c.wrote(refs[1], "moved")
}

// wrote reports a route that answers nothing, which design 008 answers as
// 204: the empty object under --json and one line otherwise.
func (c *invocation) wrote(r reference, done string) error {
	if c.json {
		return writeValue(c.Stdout, map[string]any{"path": r.path})
	}
	_, err := fmt.Fprintf(c.Stdout, "%s:%s %s\n", r.ref, r.path, done)
	return err
}
