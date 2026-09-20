// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package podman

import (
	"archive/tar"
	"bytes"
	"context"
	"fmt"
	"maps"
	"net/http"
	"path"
	"strconv"
	"strings"
	"time"

	driver "latere.ai/x/cella/runtime"
)

// tokenEnv names the projected token for the agent client of spec 011. A
// create that carries none adds nothing, so a sandbox on a control plane that
// mints no identity does not claim to hold one.
func tokenEnv(token []byte, env map[string]string) map[string]string {
	if len(token) == 0 {
		return env
	}
	out := maps.Clone(env)
	if out == nil {
		out = map[string]string{}
	}
	out[driver.TokenFileEnv] = driver.TokenPath
	return out
}

// putToken writes the workload token into the container at the reserved path,
// readable by the sandbox's user and by nobody else in it. It runs between
// the create and the start, and again on every re-projection, so a rotation
// reaches a running workload without restarting it.
//
// A numeric user is stamped on the file as its owner; a user given by name
// cannot be resolved from outside the image, so the file is projected
// readable inside the container instead, which is the same bound the
// gateway's authority takes.
func (d *Driver) putToken(ctx context.Context, id, user string, token []byte) error {
	mode, uid, gid := int64(0o400), 0, 0
	if u, g, ok := numericUser(user); ok {
		uid, gid = u, g
	} else if user != "" {
		mode = 0o444
	}
	return d.putFile(ctx, id, driver.TokenPath, token, mode, uid, gid, "the workload token")
}

// numericUser reads a uid or a uid:gid pair. A user name is not one: the
// image holds the passwd database that would resolve it.
func numericUser(user string) (uid, gid int, ok bool) {
	if user == "" {
		return 0, 0, false
	}
	first, second, pair := strings.Cut(user, ":")
	uid, err := strconv.Atoi(first)
	if err != nil || uid < 0 {
		return 0, 0, false
	}
	gid = uid
	if pair {
		if gid, err = strconv.Atoi(second); err != nil || gid < 0 {
			return 0, 0, false
		}
	}
	return uid, gid, true
}

// putFile projects one file into the container's own file system. The archive
// endpoint extracts into a directory that exists, and no image has
// /run/cella in it, so the archive carries the directory ahead of the file
// and is extracted into the parent every image does have.
func (d *Driver) putFile(ctx context.Context, id, dest string, body []byte, mode int64, uid, gid int, what string) error {
	parent, dir := path.Split(path.Dir(dest))
	now := time.Now().UTC()
	var archive bytes.Buffer
	tw := tar.NewWriter(&archive)
	entries := []struct {
		header *tar.Header
		body   []byte
	}{
		{&tar.Header{Name: dir + "/", Mode: 0o555, Typeflag: tar.TypeDir, ModTime: now}, nil},
		{&tar.Header{Name: path.Join(dir, path.Base(dest)), Mode: mode, Uid: uid, Gid: gid,
			Size: int64(len(body)), Typeflag: tar.TypeReg, ModTime: now}, body},
	}
	for _, entry := range entries {
		if err := tw.WriteHeader(entry.header); err != nil {
			return err
		}
		if _, err := tw.Write(entry.body); err != nil {
			return err
		}
	}
	if err := tw.Close(); err != nil {
		return err
	}
	url := d.client().compatURL("/containers/" + containerName(id) + "/archive?" + query("path", path.Clean(parent)))
	req, err := http.NewRequestWithContext(ctx, http.MethodPut, url, bytes.NewReader(archive.Bytes()))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/x-tar")
	resp, err := d.client().hc.Do(req)
	if err != nil {
		return fmt.Errorf("podman: projecting %s into %s: %w", what, id, err)
	}
	defer func() { _ = resp.Body.Close() }()
	if err = statusErr(resp); err != nil {
		return fmt.Errorf("podman: projecting %s into %s: %w", what, id, err)
	}
	return nil
}
