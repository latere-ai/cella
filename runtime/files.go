// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package runtime

import (
	"context"
	"errors"
	"io"
	"io/fs"
	"time"
)

// ErrTooLarge is a body past the bound the caller set on a write. The file the
// write named keeps the content it had.
var ErrTooLarge = errors.New("the body is larger than the write allows")

// DefaultFileMode is the mode a write takes when the request names none.
const DefaultFileMode fs.FileMode = 0o644

// FileInfo is one entry of a sandbox's workspace. Name is the base name
// exactly as the filesystem holds it, which may contain any byte but NUL and
// the separator, and Path is where it is, absolute and inside the workspace.
type FileInfo struct {
	Name    string      `json:"name"`
	Path    string      `json:"path"`
	Size    int64       `json:"size"`
	Mode    fs.FileMode `json:"mode"`
	ModTime time.Time   `json:"modTime"`
	IsDir   bool        `json:"isDir"`
}

// WriteRequest is one file's body, the mode it lands with, and the bound past
// which the write is refused. A zero Mode is DefaultFileMode and a zero
// MaxBytes is no bound.
type WriteRequest struct {
	Path     string
	Mode     fs.FileMode
	MaxBytes int64
	Body     io.Reader
}

// FileStore is implemented by a driver if and only if it declares
// Capabilities.Files; runtimetest checks both directions. It is the per-file
// half of the capability, beside the whole-tree ExportTar and ImportTar of
// Driver.
//
// Every path is absolute and inside the sandbox's managed workspace. A path
// that leaves it, by a component of "..", by an absolute name outside it, or
// through a symbolic link that resolves outside it, is ErrInvalid, and so is
// a Remove or a Move of the workspace root itself. A path that names nothing
// is ErrNotFound. A body past WriteRequest.MaxBytes is ErrTooLarge and the
// file the write named is unchanged.
//
// A driver whose transport needs the sandbox running answers ErrNotRunning
// while it is Stopped. One whose transport reaches the workspace without it
// answers as it does while Running. Nothing else is a legal answer to a
// stopped sandbox, which is what lets the API turn one into 409 and read the
// rest as failures.
type FileStore interface {
	// Stat describes one file or directory.
	Stat(ctx context.Context, id, path string) (FileInfo, error)

	// ReadDir returns the immediate entries of a directory, sorted by name.
	// A path that is not a directory is ErrInvalid. It is not called List
	// because Driver.List is the substrate's own listing and one type
	// implements both.
	ReadDir(ctx context.Context, id, path string) ([]FileInfo, error)

	// Open streams one file and describes it. The caller closes the reader,
	// which ends the transfer wherever it is.
	Open(ctx context.Context, id, path string) (io.ReadCloser, FileInfo, error)

	// Write stores one file, creating the missing parents, and returns the
	// bytes it wrote. The body is staged and moved onto the name, so a body
	// that ends early or passes the bound leaves the previous file whole.
	Write(ctx context.Context, id string, req WriteRequest) (int64, error)

	// Mkdir creates a directory and the missing parents. An existing
	// directory is not an error; an existing file is ErrInvalid.
	Mkdir(ctx context.Context, id, path string) error

	// Remove deletes a file or a directory tree. A path that names nothing is
	// not an error: the caller asked for it to be gone.
	Remove(ctx context.Context, id, path string) error

	// Move renames from onto the exact path to, creating to's missing
	// parents. A destination that is an existing directory is ErrInvalid: a
	// move never nests the source inside it.
	Move(ctx context.Context, id, from, to string) error
}
