// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package local

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"

	"latere.ai/x/pkg/hostsandbox"

	v1 "latere.ai/x/cella/manifest/v1"
	driver "latere.ai/x/cella/runtime"
)

// validID is the shape of a sandbox id this driver names a directory after.
var validID = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_-]{0,127}$`)

// record is what the driver keeps of one sandbox beside its scratch
// directories, which is all a second driver over the same data directory
// needs to read the sandbox back and observe its stages.
type record struct {
	State driver.State
	// Env is the sandbox's own environment with the paths this driver
	// projects, and Command the main stage's argv.
	Env     map[string]string
	Command []string
	// Guest is the workspace path the manifest names and Root the host
	// directory it is: the granted host folder, or work/ below the
	// sandbox's directory. Host is the folder when there is one, and Write
	// whether the workspace may be written.
	Guest string
	Root  string
	Host  string `json:",omitempty"`
	Write bool
	// Workdir is the host directory a command starts in.
	Workdir string
	// Egress is the boundary the sandbox got: what it asked for, narrowed
	// to the host's own network.
	Egress v1.Egress
	// Main is the main stage while it runs or has run, and Stages the
	// detached commands started since, which a stop and a delete end.
	Main   *hostsandbox.StageHandle  `json:",omitempty"`
	Stages []hostsandbox.StageHandle `json:",omitempty"`
	// Execs counts the commands run, which names each one's files.
	Execs int
}

func (d *Driver) dir(id string) string    { return filepath.Join(d.data, "sandboxes", id) }
func (d *Driver) record(id string) string { return filepath.Join(d.dir(id), "record.json") }

func (d *Driver) load(id string) (record, error) {
	if !validID.MatchString(id) {
		return record{}, fmt.Errorf("%w: %q is not a sandbox id", driver.ErrInvalid, id)
	}
	raw, err := os.ReadFile(d.record(id))
	if errors.Is(err, os.ErrNotExist) {
		return record{}, driver.ErrNotFound
	}
	if err != nil {
		return record{}, err
	}
	var r record
	if err = json.Unmarshal(raw, &r); err != nil {
		return record{}, fmt.Errorf("local: the record of %s does not read: %w", id, err)
	}
	return r, nil
}

// save writes the record whole or not at all.
func (d *Driver) save(r record) error {
	raw, err := json.Marshal(r)
	if err != nil {
		return err
	}
	p := d.record(r.State.ID)
	if err = os.WriteFile(p+".tmp", raw, 0o600); err != nil {
		return err
	}
	return os.Rename(p+".tmp", p)
}

// paths is where one sandbox's scratch directories are.
type paths struct{ dir, home, tmp, work, logs, streams, token string }

func (d *Driver) paths(id string) paths {
	dir := d.dir(id)
	return paths{
		dir: dir, home: filepath.Join(dir, "home"), tmp: filepath.Join(dir, "tmp"), work: filepath.Join(dir, "work"),
		logs: filepath.Join(dir, "logs"), streams: filepath.Join(dir, "streams"),
		token: filepath.Join(dir, "run", "cella", "token"),
	}
}

// grantFor is the grant a host folder is in, read with its links followed,
// and false for a folder no grant holds or one on the deny table.
func (d *Driver) grantFor(folder string) (Grant, string, bool) {
	if !filepath.IsAbs(folder) || filepath.Clean(folder) != folder {
		return Grant{}, "", false
	}
	real, err := filepath.EvalSymlinks(folder)
	if err != nil || d.denied(real) || d.denied(folder) {
		return Grant{}, "", false
	}
	var found Grant
	held := false
	for _, g := range d.grants {
		if under(real, g.Path) && (!held || len(g.Path) > len(found.Path)) {
			found, held = g, true
		}
	}
	return found, real, held
}

// Create makes one sandbox: its scratch directories, its record, and its
// main stage when it names a command. A host folder is checked against this
// driver's own grants, not the control plane's view of them.
func (d *Driver) Create(ctx context.Context, s driver.CreateSpec) (driver.Ref, error) {
	if err := ctx.Err(); err != nil {
		return driver.Ref{}, err
	}
	if !validID.MatchString(s.ID) || s.Lifecycle.TTL < 0 || s.Lifecycle.AutoStop < 0 || s.Lifecycle.AutoDelete < 0 ||
		(len(s.Args) > 0 && len(s.Command) == 0) {
		return driver.Ref{}, driver.ErrInvalid
	}
	switch {
	case s.Prewarm:
		return driver.Ref{}, fmt.Errorf("%w: the local driver keeps no pool", driver.ErrUnsupported)
	case s.Image != "":
		return driver.Ref{}, fmt.Errorf("%w: the local driver runs the host's own tools and no image", driver.ErrUnsupported)
	case len(s.Volumes) > 0:
		return driver.Ref{}, fmt.Errorf("%w: the local driver mounts no volume; a host folder takes its place", driver.ErrUnsupported)
	}
	p := d.paths(s.ID)
	r := record{
		Guest: path.Clean(cmpOr(s.Workspace.Path, driver.DefaultWorkdir)), Root: p.work, Write: true,
		Command: append(slices.Clone(s.Command), s.Args...),
	}
	if folder := s.Workspace.Host; folder != "" {
		grant, real, held := d.grantFor(folder)
		if !held {
			return driver.Ref{}, fmt.Errorf("%w: %s is outside every folder this host grants", driver.ErrWorkspaceNotGranted, folder)
		}
		r.Guest, r.Root, r.Host, r.Write = folder, real, real, grant.Write
	}
	workdir, err := r.hostPath(cmpOr(s.Workdir, r.Guest))
	if err != nil {
		return driver.Ref{}, err
	}
	r.Workdir = workdir
	if err = os.Mkdir(p.dir, 0o700); err != nil {
		if errors.Is(err, os.ErrExist) {
			return driver.Ref{}, driver.ErrAlreadyExists
		}
		return driver.Ref{}, err
	}
	made := false
	defer func() {
		if !made {
			_ = os.RemoveAll(p.dir)
		}
	}()
	dirs := []string{p.home, p.tmp, p.logs, p.streams, filepath.Dir(p.token)}
	if r.Host == "" {
		dirs = append(dirs, p.work)
	}
	for _, dir := range dirs {
		if err = os.MkdirAll(dir, 0o700); err != nil {
			return driver.Ref{}, err
		}
	}
	if err = d.linkReads(p.home); err != nil {
		return driver.Ref{}, err
	}
	r.Env = projected(p, s.Env)
	if len(s.Token) > 0 {
		if err = writeToken(p.token, s.Token); err != nil {
			return driver.Ref{}, err
		}
		r.Env[driver.TokenFileEnv] = p.token
	}
	r.Egress = v1.NarrowEgress(v1.Egress{
		Mode: v1.EgressMode(s.Egress.Mode), AllowedHosts: s.Egress.AllowedHosts, DeniedHosts: s.Egress.DeniedHosts,
	}, v1.HostNetwork{Mode: d.network.Mode, Hosts: d.network.Hosts})
	now := time.Now().UTC()
	r.State = driver.State{
		ID: s.ID, Name: s.Name, Owner: s.Owner, Phase: driver.Running, Isolation: v1.IsolationProcess,
		Labels: maps.Clone(s.Labels), CreatedAt: now, StartedAt: now, LastActivityAt: now,
		AutoStop: s.Lifecycle.AutoStop, AutoDelete: s.Lifecycle.AutoDelete, MeshID: s.Mesh.ID, Parent: s.Parent,
		Paths: &v1.SandboxPaths{Home: p.home, Tmp: p.tmp},
	}
	if s.Lifecycle.TTL > 0 {
		r.State.ExpiresAt = now.Add(s.Lifecycle.TTL)
	}
	if len(r.Command) > 0 {
		if err = d.startMain(ctx, &r); err != nil {
			return driver.Ref{}, err
		}
	}
	if err = d.save(r); err != nil {
		if r.Main != nil {
			_ = d.sandbox.Stop(context.WithoutCancel(ctx), *r.Main)
		}
		return driver.Ref{}, err
	}
	made = true
	return driver.Ref{ID: s.ID}, nil
}

// cmpOr is the first of two strings that is not empty.
func cmpOr(a, b string) string {
	if a != "" {
		return a
	}
	return b
}

// projected is the sandbox's own environment with the scratch directories
// named where tools look for them: the home, the temporary directory, and
// every cache a package manager keeps, so nothing is written into the
// person's own home.
func projected(p paths, env map[string]string) map[string]string {
	out := maps.Clone(env)
	if out == nil {
		out = map[string]string{}
	}
	cache := filepath.Join(p.home, ".cache")
	maps.Copy(out, map[string]string{
		"HOME": p.home, "TMPDIR": p.tmp, "XDG_CACHE_HOME": cache,
		"GOCACHE": filepath.Join(cache, "go-build"), "GOMODCACHE": filepath.Join(cache, "go-mod"),
		"npm_config_cache": filepath.Join(cache, "npm"), "PIP_CACHE_DIR": filepath.Join(cache, "pip"),
	})
	return out
}

// linkReads puts a link in the sandbox's home to each read path that is a
// file directly in the person's home, such as ~/.gitconfig, so tools that
// look for it in their home find it. The operating system's sandbox checks
// the link's target, so the deny table still holds through it.
func (d *Driver) linkReads(home string) error {
	for _, p := range d.reads {
		if filepath.Dir(p) != d.home {
			continue
		}
		info, err := os.Stat(p)
		if err != nil || info.IsDir() {
			continue
		}
		if err = os.Symlink(p, filepath.Join(home, filepath.Base(p))); err != nil && !errors.Is(err, os.ErrExist) {
			return err
		}
	}
	return nil
}

// writeToken puts the workload token at its path, readable by its owner
// alone, replacing the file so a reader sees one whole token or the other.
func writeToken(p string, token []byte) error {
	tmp := p + ".tmp"
	if err := os.WriteFile(tmp, token, 0o400); err != nil {
		return err
	}
	if err := os.Rename(tmp, p); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	return nil
}

// hostPath maps a path as the manifest names it, inside the workspace, to the
// host path it is.
func (r record) hostPath(p string) (string, error) {
	if !path.IsAbs(p) || strings.ContainsRune(p, 0) || slices.Contains(strings.Split(p, "/"), "..") {
		return "", fmt.Errorf("%w: %s is not a clean absolute path", driver.ErrInvalid, p)
	}
	p = path.Clean(p)
	if p != r.Guest && !strings.HasPrefix(p, r.Guest+"/") {
		return "", fmt.Errorf("%w: %s is outside the workspace %s", driver.ErrInvalid, p, r.Guest)
	}
	return filepath.Join(r.Root, filepath.FromSlash(strings.TrimPrefix(p, r.Guest))), nil
}

// refresh reads the main stage once more and, when it has ended, writes how:
// Stopped with exit code 0, Failed with any other, and Failed with the reason
// Lost when the stage left no status. It reports whether the record changed.
func (d *Driver) refresh(ctx context.Context, r *record) (bool, error) {
	if r.Main == nil || r.State.Phase != driver.Running {
		return false, nil
	}
	status, err := d.sandbox.Observe(ctx, *r.Main)
	if err != nil {
		return false, err
	}
	if status.Running {
		return false, nil
	}
	r.State.StoppedAt = time.Now().UTC()
	r.State.Phase, r.State.Reason = driver.Stopped, "Exited"
	switch {
	case status.Gone:
		r.State.Phase, r.State.Reason, r.State.ExitCode = "Failed", "Lost", nil
	default:
		code := status.ExitCode
		r.State.ExitCode = &code
		if code != 0 {
			r.State.Phase = "Failed"
		}
	}
	return true, nil
}

// Inspect reads one sandbox, with its main stage observed now.
func (d *Driver) Inspect(ctx context.Context, id string) (driver.State, error) {
	if err := ctx.Err(); err != nil {
		return driver.State{}, err
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	r, err := d.load(id)
	if err != nil {
		return driver.State{}, err
	}
	if changed, err := d.refresh(ctx, &r); err != nil {
		return driver.State{}, err
	} else if changed {
		if err = d.save(r); err != nil {
			return driver.State{}, err
		}
	}
	return r.State, nil
}

// List reads every sandbox the data directory holds that the filter selects.
func (d *Driver) List(ctx context.Context, f driver.Filter) ([]driver.State, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	entries, err := os.ReadDir(filepath.Join(d.data, "sandboxes"))
	if err != nil {
		return nil, err
	}
	out := []driver.State{}
	for _, entry := range entries {
		if !entry.IsDir() || !validID.MatchString(entry.Name()) {
			continue
		}
		state, err := d.Inspect(ctx, entry.Name())
		if errors.Is(err, driver.ErrNotFound) {
			// A directory with no record is a sandbox being created or
			// deleted, which is not one to list.
			continue
		}
		if err != nil {
			return nil, err
		}
		if f.Selects(state) {
			out = append(out, state)
		}
	}
	return out, nil
}

// startMain launches the main stage, detached, its output in the sandbox's
// main log.
func (d *Driver) startMain(ctx context.Context, r *record) error {
	spec := d.stage(*r, "main", r.Command, r.Env, r.Workdir)
	handle, err := d.sandbox.Launch(ctx, spec)
	if err != nil {
		return fmt.Errorf("local: starting the main command of %s: %w", r.State.ID, err)
	}
	r.Main = &handle
	return nil
}

// Start runs a stopped or failed sandbox again: its main stage, when it has
// a command, on the scratch and the workspace it had.
func (d *Driver) Start(ctx context.Context, id string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	r, err := d.load(id)
	if err != nil {
		return err
	}
	if _, err = d.refresh(ctx, &r); err != nil {
		return err
	}
	if r.State.Phase == driver.Running {
		return nil
	}
	if len(r.Command) > 0 {
		if err = d.startMain(ctx, &r); err != nil {
			return err
		}
	}
	now := time.Now().UTC()
	r.State.Phase, r.State.Reason, r.State.ExitCode = driver.Running, "", nil
	r.State.StartedAt, r.State.LastActivityAt, r.State.StoppedAt = now, now, time.Time{}
	return d.save(r)
}

// Stop ends the sandbox's main stage, every detached command and every
// attached one, and keeps its scratch and its workspace.
func (d *Driver) Stop(ctx context.Context, id string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	r, err := d.load(id)
	if err != nil {
		return err
	}
	if err = d.endAll(ctx, &r); err != nil {
		return err
	}
	if r.State.Phase != driver.Stopped {
		r.State.Phase, r.State.Reason, r.State.ExitCode = driver.Stopped, "", nil
		r.State.StoppedAt = time.Now().UTC()
	}
	return d.save(r)
}

// endAll stops every stage of a sandbox. It runs under the driver's lock.
func (d *Driver) endAll(ctx context.Context, r *record) error {
	var failed error
	stages := slices.Clone(r.Stages)
	if r.Main != nil {
		stages = append(stages, *r.Main)
	}
	for _, h := range stages {
		failed = errors.Join(failed, d.sandbox.Stop(ctx, h))
	}
	r.Stages = nil
	for e := range d.attached[r.State.ID] {
		e.end()
	}
	return failed
}

// Delete ends everything the sandbox runs and removes its directory under
// the data directory and nothing else. A host folder is never opened, walked
// or removed: it is not below the sandbox's directory, and a link the
// sandbox left there is removed as a link.
func (d *Driver) Delete(ctx context.Context, id string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	r, err := d.load(id)
	if err != nil {
		return err
	}
	if err = d.endAll(ctx, &r); err != nil {
		return err
	}
	delete(d.attached, id)
	return os.RemoveAll(d.dir(id))
}

// Touch stamps activity on the sandbox.
func (d *Driver) Touch(ctx context.Context, id string) error {
	return d.edit(ctx, id, func(r *record) error {
		r.State.LastActivityAt = time.Now().UTC()
		return nil
	})
}

// Update changes the labels, the environment, the token and the deadlines. A
// change of size and an adoption are refused: this driver sizes nothing and
// keeps no pool.
func (d *Driver) Update(ctx context.Context, id string, c driver.Change) error {
	if adoption, err := c.Adoption(); err != nil {
		return err
	} else if adoption != nil {
		return fmt.Errorf("%w: the local driver keeps no pool", driver.ErrUnsupported)
	}
	if err := c.FixedSize(); err != nil {
		return err
	}
	if c.Lifecycle != nil && (c.Lifecycle.TTL < 0 || c.Lifecycle.AutoStop < 0 || c.Lifecycle.AutoDelete < 0) {
		return driver.ErrInvalid
	}
	return d.edit(ctx, id, func(r *record) error {
		if c.Labels != nil {
			r.State.Labels = maps.Clone(*c.Labels)
		}
		if c.Env != nil {
			r.Env = projected(d.paths(id), *c.Env)
			if _, err := os.Stat(d.paths(id).token); err == nil {
				r.Env[driver.TokenFileEnv] = d.paths(id).token
			}
		}
		if len(c.Token) > 0 {
			if err := writeToken(d.paths(id).token, c.Token); err != nil {
				return err
			}
			r.Env[driver.TokenFileEnv] = d.paths(id).token
		}
		if c.Lifecycle != nil {
			r.State.AutoStop, r.State.AutoDelete, r.State.ExpiresAt = c.Lifecycle.AutoStop, c.Lifecycle.AutoDelete, time.Time{}
			if c.Lifecycle.TTL > 0 {
				r.State.ExpiresAt = r.State.CreatedAt.Add(c.Lifecycle.TTL)
			}
		}
		return nil
	})
}

func (d *Driver) edit(ctx context.Context, id string, fn func(*record) error) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	r, err := d.load(id)
	if err != nil {
		return err
	}
	if err = fn(&r); err != nil {
		return err
	}
	return d.save(r)
}

// pathDirs are the directories of PATH a command may read where they are:
// each one exactly, never its parent, and never the home directory itself.
func (d *Driver) pathDirs() []string {
	raw, _ := d.lookup("PATH")
	var out []string
	for _, dir := range filepath.SplitList(raw) {
		if !filepath.IsAbs(dir) {
			continue
		}
		dir = filepath.Clean(dir)
		if dir == d.home || d.denied(dir) || slices.Contains(out, dir) {
			continue
		}
		out = append(out, dir)
	}
	return out
}

// stage is one command as the operating system's sandbox runs it: the
// sandbox's scratch read-write, its workspace as its grant allows, the read
// paths, the PATH directories, the token, the deny table, and the network the
// sandbox got.
func (d *Driver) stage(r record, name string, argv []string, env map[string]string, workdir string) hostsandbox.StageSpec {
	p := d.paths(r.State.ID)
	access := hostsandbox.ReadOnly
	if r.Write {
		access = hostsandbox.ReadWrite
	}
	grants := []hostsandbox.Path{
		{Host: p.home, Access: hostsandbox.ReadWrite},
		{Host: p.tmp, Access: hostsandbox.ReadWrite},
		{Host: p.streams, Access: hostsandbox.ReadWrite},
		{Host: r.Root, Access: access},
	}
	if _, err := os.Stat(p.token); err == nil {
		grants = append(grants, hostsandbox.Path{Host: p.token, Access: hostsandbox.ReadOnly})
	}
	for _, read := range d.reads {
		grants = append(grants, hostsandbox.Path{Host: read, Access: hostsandbox.ReadOnly})
	}
	for _, dir := range d.pathDirs() {
		grants = append(grants, hostsandbox.Path{Host: dir, Access: hostsandbox.ReadOnly})
	}
	network := hostsandbox.Network{Mode: hostsandbox.NetworkNone}
	if r.Egress.Mode == v1.EgressAllowlist {
		network = hostsandbox.Network{Mode: hostsandbox.NetworkAllowlist, Domains: slices.Clone(r.Egress.AllowedHosts)}
	}
	// The sandbox runtime places its own sockets in TMPDIR, and a socket's
	// path has a short bound, which a sandbox's temporary directory deep in
	// the data directory passes. So the runtime keeps this process's TMPDIR,
	// and the command gets the sandbox's own through env inside the sandbox.
	stageEnv := maps.Clone(env)
	tmp := stageEnv["TMPDIR"]
	delete(stageEnv, "TMPDIR")
	if tmp != "" {
		argv = append([]string{"/usr/bin/env", "TMPDIR=" + tmp}, argv...)
	}
	return hostsandbox.StageSpec{
		Name: name, Argv: argv, Env: stageEnv, Workdir: workdir, Paths: grants,
		Denied: slices.Clone(d.deny), Network: network, LogPath: filepath.Join(p.logs, name+".log"),
	}
}

// execName is the name of the n-th command of a sandbox.
func execName(n int) string { return "exec-" + strconv.Itoa(n) }
