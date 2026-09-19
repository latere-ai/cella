// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package podman

import (
	"archive/tar"
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"path"
	"strings"

	driver "latere.ai/x/cella/runtime"
)

// MaxArchiveBytes limits the sum of file bytes accepted per import, the same
// bound the native driver applies.
const MaxArchiveBytes int64 = 256 << 20

// ImportTar extracts an archive below dest inside the sandbox's workspace. The
// archive is read here before any byte reaches the engine: podman extracts an
// absolute name, a traversal and a link as written, so this is where they are
// refused. It works while the sandbox is Stopped, which is the Files
// capability.
func (d *Driver) ImportTar(ctx context.Context, id, dest string, src io.Reader) error {
	root, err := d.workspacePath(ctx, id)
	if err != nil {
		return err
	}
	if _, err := relative(root, dest); err != nil {
		return err
	}
	pr, pw := io.Pipe()
	var produce error
	done := make(chan struct{})
	go func() {
		defer close(done)
		produce = reframeImport(ctx, src, pw)
		_ = pw.CloseWithError(produce)
	}()
	defer func() {
		_ = pr.Close()
		<-done
	}()
	req, err := http.NewRequestWithContext(ctx, http.MethodPut, d.client().compatURL("/containers/"+containerName(id)+"/archive?"+query("path", path.Clean(dest))), pr)
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/x-tar")
	resp, err := d.client().hc.Do(req)
	if err != nil {
		// The reader failing is why the request failed; report that instead.
		<-done
		if produce != nil {
			return produce
		}
		return fmt.Errorf("podman: importing into %s: %w", id, err)
	}
	defer func() { _ = resp.Body.Close() }()
	serr := statusErr(resp)
	<-done
	if produce != nil {
		return produce
	}
	if serr != nil {
		if notFound(serr) {
			return driver.ErrNotFound
		}
		return fmt.Errorf("podman: importing into %s: %w", id, serr)
	}
	_, _ = io.Copy(io.Discard, resp.Body)
	return nil
}

// reframeImport copies the caller's archive entry by entry, refusing anything
// but a regular file or a directory and anything whose name leaves the
// destination.
func reframeImport(ctx context.Context, src io.Reader, dst io.Writer) error {
	tr := tar.NewReader(src)
	tw := tar.NewWriter(dst)
	var total int64
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		h, err := tr.Next()
		if errors.Is(err, io.EOF) {
			return tw.Close()
		}
		if err != nil {
			return fmt.Errorf("%w: reading the archive: %w", driver.ErrInvalid, err)
		}
		name := strings.TrimSuffix(h.Name, "/")
		if !fs.ValidPath(name) || strings.Contains(name, "\\") || strings.Contains(name, "\x00") {
			return fmt.Errorf("%w: archive path %q", driver.ErrInvalid, h.Name)
		}
		switch h.Typeflag {
		case tar.TypeDir:
			if err := tw.WriteHeader(&tar.Header{Name: name + "/", Mode: h.Mode, Typeflag: tar.TypeDir, ModTime: h.ModTime}); err != nil {
				return err
			}
		case tar.TypeReg:
			if h.Size < 0 || h.Size > MaxArchiveBytes-total {
				return fmt.Errorf("%w: archive size", driver.ErrInvalid)
			}
			total += h.Size
			if err := tw.WriteHeader(&tar.Header{Name: name, Mode: h.Mode, Size: h.Size, Typeflag: tar.TypeReg, ModTime: h.ModTime}); err != nil {
				return err
			}
			if _, err := io.CopyN(tw, tr, h.Size); err != nil {
				return err
			}
		default:
			return fmt.Errorf("%w: archive entry type %q for %q", driver.ErrInvalid, string(h.Typeflag), h.Name)
		}
	}
}

// ExportTar writes the named paths as one archive with names relative to the
// workspace. Podman names its entries after the base of the path asked for, so
// every entry is renamed here, several paths become one archive, and a
// directory loses the trailing slash the contract's readers do not expect.
func (d *Driver) ExportTar(ctx context.Context, id string, paths []string, dst io.Writer) error {
	root, err := d.workspacePath(ctx, id)
	if err != nil {
		return err
	}
	if len(paths) == 0 {
		paths = []string{root}
	}
	rels := make([]string, 0, len(paths))
	for _, p := range paths {
		rel, err := relative(root, p)
		if err != nil {
			return err
		}
		rels = append(rels, rel)
	}
	tw := tar.NewWriter(dst)
	for i, p := range paths {
		if err := d.exportOne(ctx, id, path.Clean(p), rels[i], tw); err != nil {
			return err
		}
	}
	return tw.Close()
}

// exportOne streams one path out of the container and into the open archive.
func (d *Driver) exportOne(ctx context.Context, id, abs, rel string, tw *tar.Writer) error {
	resp, err := d.client().do(ctx, http.MethodGet, d.client().compatURL("/containers/"+containerName(id)+"/archive?"+query("path", abs)), nil)
	if err != nil {
		return fmt.Errorf("podman: exporting %s from %s: %w", abs, id, err)
	}
	defer func() { _ = resp.Body.Close() }()
	if serr := statusErr(resp); serr != nil {
		if notFound(serr) {
			return fmt.Errorf("%w: %s", driver.ErrNotFound, abs)
		}
		return fmt.Errorf("podman: exporting %s from %s: %w", abs, id, serr)
	}
	tr := tar.NewReader(resp.Body)
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		h, err := tr.Next()
		if errors.Is(err, io.EOF) {
			return nil
		}
		if err != nil {
			return err
		}
		name := exportName(rel, h.Name)
		if name == "" {
			continue
		}
		switch h.Typeflag {
		case tar.TypeDir:
			if err := tw.WriteHeader(&tar.Header{Name: name, Mode: h.Mode, Typeflag: tar.TypeDir, ModTime: h.ModTime}); err != nil {
				return err
			}
		case tar.TypeReg:
			if err := tw.WriteHeader(&tar.Header{Name: name, Mode: h.Mode, Size: h.Size, Typeflag: tar.TypeReg, ModTime: h.ModTime}); err != nil {
				return err
			}
			if _, err := io.CopyN(tw, tr, h.Size); err != nil {
				return err
			}
		default:
			return fmt.Errorf("%w: %s is a %q, which an export does not carry", driver.ErrInvalid, name, string(h.Typeflag))
		}
	}
}

// exportName maps one entry of podman's archive onto its name relative to the
// workspace. Podman prefixes every entry with the base of the path asked for,
// so that prefix is dropped and the path's own name relative to the workspace
// is put back. The workspace's own directory entry maps to nothing.
func exportName(rel, entry string) string {
	entry = strings.TrimSuffix(entry, "/")
	_, rest, _ := strings.Cut(entry, "/")
	if rel == "." {
		return rest
	}
	if rest == "" {
		return rel
	}
	return rel + "/" + rest
}

// workspacePath reads where the sandbox's files live, which is what every path
// in a transfer is relative to.
func (d *Driver) workspacePath(ctx context.Context, id string) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}
	if !validID.MatchString(id) {
		return "", driver.ErrInvalid
	}
	vi, err := d.inspectVolume(ctx, workspaceVolume(id))
	if err != nil {
		return "", err
	}
	return identityOf(vi.Labels).workspacePath, nil
}
