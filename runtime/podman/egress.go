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
	"time"

	"latere.ai/x/cella/egress"
	driver "latere.ai/x/cella/runtime"
)

// egressEnv is the sandbox's own environment with the boundary's added: the
// two doors, the credential that authenticates it at them, and the trust
// store that holds the gateway's authority. A create that carries no gateway
// adds nothing, so a sandbox on an installation with none is unchanged.
func egressEnv(s driver.CreateSpec) map[string]string {
	env := maps.Clone(s.Env)
	projection := egress.Projection{
		ProxyAddr:   s.Egress.ProxyAddr,
		ReverseAddr: s.Egress.ReverseAddr,
		Credential:  s.Egress.Credential,
	}
	if s.Egress.CAPEM != "" {
		projection.CAPath = egress.CAPath
	}
	added := projection.Env()
	if len(added) == 0 {
		return env
	}
	if env == nil {
		env = make(map[string]string, len(added))
	}
	maps.Copy(env, added)
	return env
}

// putEgressCA writes the gateway's authority into the container, read-only to
// the workload, at the path the trust variables name. It runs between the
// create and the start, so the workload's first request already trusts the
// door it is pointed at.
func (d *Driver) putEgressCA(ctx context.Context, id, certPEM string) error {
	// The archive endpoint extracts into a directory that exists, and no
	// image has /run/cella in it, so the archive carries the directory
	// ahead of the file and is extracted into /run, which every image has.
	parent, dir := path.Split(path.Dir(egress.CAPath))
	now := time.Now().UTC()
	var body bytes.Buffer
	tw := tar.NewWriter(&body)
	entries := []struct {
		header *tar.Header
		body   string
	}{
		{&tar.Header{Name: dir + "/", Mode: 0o555, Typeflag: tar.TypeDir, ModTime: now}, ""},
		{&tar.Header{Name: path.Join(dir, path.Base(egress.CAPath)), Mode: 0o444, Size: int64(len(certPEM)), Typeflag: tar.TypeReg, ModTime: now}, certPEM},
	}
	for _, entry := range entries {
		if err := tw.WriteHeader(entry.header); err != nil {
			return err
		}
		if _, err := tw.Write([]byte(entry.body)); err != nil {
			return err
		}
	}
	if err := tw.Close(); err != nil {
		return err
	}
	url := d.client().compatURL("/containers/" + containerName(id) + "/archive?" + query("path", path.Clean(parent)))
	req, err := http.NewRequestWithContext(ctx, http.MethodPut, url, bytes.NewReader(body.Bytes()))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/x-tar")
	resp, err := d.client().hc.Do(req)
	if err != nil {
		return fmt.Errorf("podman: projecting the gateway's authority into %s: %w", id, err)
	}
	defer func() { _ = resp.Body.Close() }()
	if err = statusErr(resp); err != nil {
		return fmt.Errorf("podman: projecting the gateway's authority into %s: %w", id, err)
	}
	return nil
}
