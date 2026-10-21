// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

//go:build darwin || linux

package device

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"syscall"

	"latere.ai/x/cella/runtime/local"
)

// homeFolders are the folders directly in the home directory that hold a
// path the sandbox always denies one level down: ~/.config/gcloud and
// ~/.docker/config.json. While a denied path does not exist, macOS's
// Seatbelt keeps a stage from creating any missing directory on the way to
// it inside a writable grant, so that it cannot plant the denied path under
// a name it controls. A grant of the home directory or a folder above it
// would then leave a session unable to create ~/.config or ~/.docker, where
// many tools keep their settings, so the engine creates both before it
// serves such a grant. Mode 0700 is what the XDG base directory
// specification and the docker command create them with.
var homeFolders = []string{".config", ".docker"}

// createHomeFolders creates each of homeFolders that is missing in home when
// a writable grant is home or a folder above it. A name that already exists
// is left as it is, whatever it is: a directory, a file, or a symbolic link,
// which is never followed, since the sandbox's own checks of links govern
// the grant. Any other failure refuses the grant with
// CodeHomeFolderFailed. Nothing is created in a home directory another user
// owns, as when the engine runs as root, since a folder made there would be
// that process's rather than the person's.
func createHomeFolders(home string, grants []local.Grant) error {
	if !coversHome(home, grants) || !ownHome(home) {
		return nil
	}
	for _, name := range homeFolders {
		// Mkdir creates the name itself and fails with EEXIST on anything
		// already there, a link that leads nowhere included, so nothing is
		// created through a link and no check races the create.
		err := os.Mkdir(filepath.Join(home, name), 0o700)
		if err != nil && !errors.Is(err, fs.ErrExist) {
			return Fail(CodeHomeFolderFailed, "", err)
		}
	}
	return nil
}

// coversHome reports whether a writable grant is home or a folder above it,
// as written or with its links resolved, as the sandbox resolves both. A
// grant that does not resolve covers nothing here; the local driver refuses
// it.
func coversHome(home string, grants []local.Grant) bool {
	realHome, err := filepath.EvalSymlinks(home)
	if err != nil {
		realHome = home
	}
	for _, g := range grants {
		if !g.Write {
			continue
		}
		real, err := filepath.EvalSymlinks(g.Path)
		if err != nil {
			continue
		}
		if atOrAbove(g.Path, home) || atOrAbove(real, realHome) {
			return true
		}
	}
	return false
}

// atOrAbove reports whether the clean absolute path dir is p or a folder
// above it.
func atOrAbove(dir, p string) bool {
	return dir == p || dir == "/" || strings.HasPrefix(p, dir+string(filepath.Separator))
}

// ownHome reports whether this process's user owns home, so a folder it
// creates there is the person's.
func ownHome(home string) bool {
	info, err := os.Stat(home)
	if err != nil {
		// The create that follows names the failure.
		return true
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	return !ok || int(stat.Uid) == os.Geteuid()
}
