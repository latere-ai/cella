// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package native

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	driver "latere.ai/x/cella/runtime"
)

// TestFileStoreRefusesWhatTheContractDoesNot covers the answers the
// conformance suite cannot reach through a well-behaved caller: a sandbox
// that is not there, a parent that is a file, a body that is nil, and a
// context that ended before the call.
func TestFileStoreRefusesWhatTheContractDoesNot(t *testing.T) {
	ctx := context.Background()
	d, _ := fresh(t)
	create(t, d, "one")

	t.Run("NoSuchSandbox", func(t *testing.T) {
		for name, err := range map[string]error{
			"Stat":    errOf(d.Stat(ctx, "absent", "/workspace/f")),
			"ReadDir": errOf(d.ReadDir(ctx, "absent", "/workspace")),
			"Open":    openErr(d, "absent", "/workspace/f"),
			"Write":   errOf(d.Write(ctx, "absent", driver.WriteRequest{Path: "/workspace/f"})),
			"Mkdir":   d.Mkdir(ctx, "absent", "/workspace/f"),
			"Remove":  d.Remove(ctx, "absent", "/workspace/f"),
			"Move":    d.Move(ctx, "absent", "/workspace/f", "/workspace/g"),
		} {
			if !errors.Is(err, driver.ErrNotFound) {
				t.Errorf("%s on a sandbox that is not there: %v", name, err)
			}
		}
	})

	t.Run("AParentThatIsAFile", func(t *testing.T) {
		if _, err := d.Write(ctx, "one", driver.WriteRequest{Path: "/workspace/file", Body: strings.NewReader("x")}); err != nil {
			t.Fatal(err)
		}
		for name, err := range map[string]error{
			"Write": errOf(d.Write(ctx, "one", driver.WriteRequest{Path: "/workspace/file/below", Body: strings.NewReader("x")})),
			"Mkdir": d.Mkdir(ctx, "one", "/workspace/file/below"),
			"Move":  d.Move(ctx, "one", "/workspace/file", "/workspace/file/below"),
		} {
			if !errors.Is(err, driver.ErrInvalid) {
				t.Errorf("%s below a file: %v", name, err)
			}
		}
	})

	t.Run("ABodyThatIsNotThere", func(t *testing.T) {
		n, err := d.Write(ctx, "one", driver.WriteRequest{Path: "/workspace/empty"})
		if err != nil || n != 0 {
			t.Fatalf("a write with no body: %d bytes, %v", n, err)
		}
		info, err := d.Stat(ctx, "one", "/workspace/empty")
		if err != nil || info.Size != 0 || info.Mode.Perm() != driver.DefaultFileMode {
			t.Fatalf("the file a write with no body left: %+v, %v", info, err)
		}
	})

	t.Run("ADirectoryIsNotAStream", func(t *testing.T) {
		if err := openErr(d, "one", "/workspace"); !errors.Is(err, driver.ErrInvalid) {
			t.Errorf("Open of a directory: %v", err)
		}
	})

	t.Run("AContextThatEnded", func(t *testing.T) {
		ended, cancel := context.WithCancel(ctx)
		cancel()
		if err := errOf(d.Stat(ended, "one", "/workspace")); !errors.Is(err, context.Canceled) {
			t.Errorf("Stat under a cancelled context: %v", err)
		}
		if err := errOf(d.Write(ended, "one", driver.WriteRequest{Path: "/workspace/cancelled", Body: strings.NewReader("x")})); !errors.Is(err, context.Canceled) {
			t.Errorf("Write under a cancelled context: %v", err)
		}
	})
}

// TestFileStoreReadsWhatTheProcessWrote is the reason the driver exists: what
// a command inside the sandbox writes is what the file routes read back,
// through the same workspace directory.
func TestFileStoreReadsWhatTheProcessWrote(t *testing.T) {
	ctx := context.Background()
	d, root := fresh(t)
	create(t, d, "one")
	if _, _, code, err := run(t, d, "one", driver.ExecRequest{Command: []string{"sh", "-c", "printf written > made.txt"}}); err != nil || code != 0 {
		t.Fatalf("the command exited %d: %v", code, err)
	}
	body, info, err := d.Open(ctx, "one", "/workspace/made.txt")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = body.Close() }()
	b, err := io.ReadAll(body)
	if err != nil || string(b) != "written" || info.Size != 7 {
		t.Fatalf("the file the command wrote: %q %+v %v", b, info, err)
	}
	// The workspace directory on the host is the one the store wrote into.
	if _, err := os.Stat(filepath.Join(root, "one", "workspace", "made.txt")); err != nil {
		t.Fatal(err)
	}
}

// TestContainRefusesAWorkspaceThatIsGone covers the containment probe when
// the workspace directory itself cannot be resolved.
func TestContainRefusesAWorkspaceThatIsGone(t *testing.T) {
	d, root := fresh(t)
	create(t, d, "one")
	if err := os.RemoveAll(filepath.Join(root, "one", "workspace")); err != nil {
		t.Fatal(err)
	}
	if err := errOf(d.Stat(context.Background(), "one", "/workspace/f")); err == nil {
		t.Fatal("a workspace that is gone answered a stat")
	}
}

func errOf[T any](_ T, err error) error { return err }

func openErr(d *Driver, id, path string) error {
	body, _, err := d.Open(context.Background(), id, path)
	if body != nil {
		_ = body.Close()
	}
	return err
}
