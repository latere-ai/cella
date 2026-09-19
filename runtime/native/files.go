// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package native

import (
	"archive/tar"
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"slices"
	"strings"

	driver "latere.ai/x/cella/runtime"
)

// MaxArchiveBytes limits the sum of uncompressed file bytes accepted per import.
const MaxArchiveBytes int64 = 256 << 20

func workspacePath(p string) (string, error) {
	if strings.Contains(p, "\x00") || !path.IsAbs(p) {
		return "", driver.ErrInvalid
	}
	if slices.Contains(strings.Split(p, "/"), "..") {
		return "", driver.ErrInvalid
	}
	p = path.Clean(p)
	if p == driver.DefaultWorkdir {
		return ".", nil
	}
	if !strings.HasPrefix(p, driver.DefaultWorkdir+"/") {
		return "", driver.ErrInvalid
	}
	return strings.TrimPrefix(p, driver.DefaultWorkdir+"/"), nil
}
func (d *Driver) workspace(ctx context.Context, id string) (*os.Root, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	if _, err := d.load(id); err != nil {
		return nil, err
	}
	return os.OpenRoot(filepath.Join(d.dir(id), "workspace"))
}

// ImportTar imports regular files and directories below dest. Links, special
// files, absolute archive names and traversal are refused. Each file is replaced
// only after its complete payload has arrived; earlier complete files survive errors.
func (d *Driver) ImportTar(ctx context.Context, id, dest string, src io.Reader) error {
	rel, err := workspacePath(dest)
	if err != nil {
		return err
	}
	base, err := d.workspace(ctx, id)
	if err != nil {
		return err
	}
	defer func() { _ = base.Close() }()
	if err = base.MkdirAll(rel, 0700); err != nil {
		return err
	}
	root, err := base.OpenRoot(rel)
	if err != nil {
		return err
	}
	defer func() { _ = root.Close() }()
	tr := tar.NewReader(src)
	var total int64
	for {
		if err = ctx.Err(); err != nil {
			return err
		}
		h, err := tr.Next()
		if errors.Is(err, io.EOF) {
			return nil
		}
		if err != nil {
			return err
		}
		name := strings.TrimSuffix(h.Name, "/")
		if !fs.ValidPath(name) || strings.Contains(name, "\\") || strings.Contains(name, "\x00") {
			return fmt.Errorf("%w: archive path", driver.ErrInvalid)
		}
		switch h.Typeflag {
		case tar.TypeDir:
			if err = root.MkdirAll(name, 0755); err != nil {
				return err
			}
		case tar.TypeReg:
			if h.Size < 0 || h.Size > MaxArchiveBytes-total {
				return fmt.Errorf("%w: archive size", driver.ErrInvalid)
			}
			total += h.Size
			if err = root.MkdirAll(path.Dir(name), 0700); err != nil {
				return err
			}
			if err = importFile(ctx, root, name, os.FileMode(h.Mode)&0777, tr); err != nil {
				return err
			}
		default:
			return fmt.Errorf("%w: archive entry type", driver.ErrInvalid)
		}
	}
}
func importFile(ctx context.Context, root *os.Root, name string, mode os.FileMode, src io.Reader) error {
	// A random exclusive temporary file also avoids following destination symlinks.
	temp := ".cella-import-" + rand.Text()
	f, err := root.OpenFile(temp, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if err != nil {
		return err
	}
	defer func() { _ = root.Remove(temp) }()
	_, err = io.Copy(f, &contextReader{ctx: ctx, r: src})
	if err == nil {
		err = f.Chmod(mode)
	}
	err = errors.Join(err, f.Close())
	if err != nil {
		return err
	}
	return root.Rename(temp, name)
}

type contextReader struct {
	ctx context.Context
	r   io.Reader
}

func (r *contextReader) Read(p []byte) (int, error) {
	if err := r.ctx.Err(); err != nil {
		return 0, err
	}
	return r.r.Read(p)
}

// ExportTar writes workspace-relative names, preserving file permissions. Links
// and special files are refused rather than followed or emitted.
func (d *Driver) ExportTar(ctx context.Context, id string, paths []string, dst io.Writer) error {
	root, err := d.workspace(ctx, id)
	if err != nil {
		return err
	}
	defer func() { _ = root.Close() }()
	tw := tar.NewWriter(dst)
	if len(paths) == 0 {
		paths = []string{driver.DefaultWorkdir}
	}
	for _, p := range paths {
		rel, err := workspacePath(p)
		if err != nil {
			return err
		}
		err = fs.WalkDir(root.FS(), rel, func(name string, entry fs.DirEntry, walkErr error) error {
			if walkErr != nil {
				return walkErr
			}
			if err := ctx.Err(); err != nil {
				return err
			}
			info, err := entry.Info()
			if err != nil {
				return err
			}
			if !info.IsDir() && !info.Mode().IsRegular() {
				return fmt.Errorf("%w: file type", driver.ErrInvalid)
			}
			h, err := tar.FileInfoHeader(info, "")
			if err != nil {
				return err
			}
			h.Name = name
			if err = tw.WriteHeader(h); err != nil {
				return err
			}
			if info.IsDir() {
				return nil
			}
			f, err := root.Open(name)
			if err != nil {
				return err
			}
			_, err = io.Copy(tw, &contextReader{ctx: ctx, r: f})
			return errors.Join(err, f.Close())
		})
		if err != nil {
			return err
		}
	}
	return tw.Close()
}
