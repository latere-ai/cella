// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package podman

import (
	"archive/tar"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"net/http"
	"slices"
	"strings"

	driver "latere.ai/x/cella/runtime"
)

// A sandbox's environment holds its own values and the gateway's credential,
// so no label of any object this driver writes carries it: a label is read by
// every listing of the engine's volumes and containers. The container holds
// the environment it was created with, and a command inherits it. What an
// update or an adoption sets afterwards, which a created container can no
// longer take, is kept inside that container at envPath, beside its token,
// readable by root alone, and a command is given the variables of it its
// container did not start with at the same value.

// envPath is the file inside the container that holds the environment an
// update or an adoption set, as one JSON object.
const envPath = "/run/cella/env.json"

// putEnv writes the environment an update or an adoption set into the
// sandbox's container. A container that is not there is driver.ErrNotFound.
func (d *Driver) putEnv(ctx context.Context, id string, env map[string]string) error {
	if env == nil {
		env = map[string]string{}
	}
	body, err := json.Marshal(env)
	if err != nil {
		return fmt.Errorf("podman: encoding the environment of %s: %w", id, err)
	}
	err = d.putFile(ctx, id, envPath, body, 0o400, 0, 0, "the environment")
	if notFound(err) {
		return driver.ErrNotFound
	}
	return err
}

// readEnv reads the environment an update or an adoption set back from the
// sandbox's container. A container without the file has been set none, and
// runs with the environment it was created with, which is nil here.
func (d *Driver) readEnv(ctx context.Context, id string) (map[string]string, error) {
	resp, err := d.client().do(ctx, http.MethodGet, d.client().compatURL("/containers/"+containerName(id)+"/archive?"+query("path", envPath)), nil)
	if err != nil {
		return nil, fmt.Errorf("podman: reading the environment of %s: %w", id, err)
	}
	defer func() { _ = resp.Body.Close() }()
	if serr := statusErr(resp); serr != nil {
		if notFound(serr) {
			return nil, nil
		}
		return nil, fmt.Errorf("podman: reading the environment of %s: %w", id, serr)
	}
	tr := tar.NewReader(resp.Body)
	for {
		h, err := tr.Next()
		if errors.Is(err, io.EOF) {
			return nil, fmt.Errorf("podman: the archive of %s's environment holds no file", id)
		}
		if err != nil {
			return nil, fmt.Errorf("podman: reading the environment of %s: %w", id, err)
		}
		if h.Typeflag != tar.TypeReg {
			continue
		}
		body, err := io.ReadAll(io.LimitReader(tr, MaxArchiveBytes))
		if err != nil {
			return nil, fmt.Errorf("podman: reading the environment of %s: %w", id, err)
		}
		env := map[string]string{}
		if err := json.Unmarshal(body, &env); err != nil {
			return nil, fmt.Errorf("%w: the environment of %s: %w", driver.ErrInvalid, id, err)
		}
		return env, nil
	}
}

// envOf reads the KEY=VALUE list an engine reports a container's environment
// as.
func envOf(list []string) map[string]string {
	out := make(map[string]string, len(list))
	for _, entry := range list {
		if k, v, ok := strings.Cut(entry, "="); ok {
			out[k] = v
		}
	}
	return out
}

// commandEnv is the environment one command is created with: the variables
// set holds that the container's own environment does not hold at the same
// value, since a command inherits that one, and the request's own on top.
func commandEnv(own, set, request map[string]string) map[string]string {
	out := map[string]string{}
	for k, v := range set {
		if held, ok := own[k]; !ok || held != v {
			out[k] = v
		}
	}
	maps.Copy(out, request)
	return out
}

// MoveCredentials rewrites every record an earlier version of this driver
// wrote with the sandbox's environment as a label: the values go into the
// sandbox's container first, and the record's next generation carries no
// value. It is what a control plane runs once at start, so the values leave
// the labels when the new version is rolled out rather than at each sandbox's
// next change. It reports how many records it rewrote and every failure; a
// record it could not rewrite is still read as it stands and is rewritten by
// its sandbox's next change.
func (d *Driver) MoveCredentials(ctx context.Context) (int, error) {
	// The engine is found first: a worker runs the move before the
	// preflight its own start runs.
	if err := d.Preflight(ctx); err != nil {
		return 0, err
	}
	vols, err := d.listVolumes(ctx)
	if err != nil {
		return 0, fmt.Errorf("podman: listing the records: %w", err)
	}
	held := map[string]bool{}
	for _, v := range vols {
		if v.Labels[labelKind] == kindRecord && v.Labels[labelEnv] != "" && v.Labels[labelID] != "" {
			held[v.Labels[labelID]] = true
		}
	}
	moved := 0
	var failed error
	for _, id := range slices.Sorted(maps.Keys(held)) {
		// edit moves an environment the current record holds whatever the
		// change, so the change here is none, and a record another process
		// rewrote first is left alone.
		rewrote := false
		err := d.edit(ctx, id, func(r *record) bool {
			rewrote = r.env != nil
			return false
		})
		switch {
		case errors.Is(err, driver.ErrNotFound):
			// The sandbox went between the list and the edit.
		case err != nil:
			failed = errors.Join(failed, fmt.Errorf("sandbox %s: %w", id, err))
		case rewrote:
			moved++
		}
	}
	return moved, failed
}
