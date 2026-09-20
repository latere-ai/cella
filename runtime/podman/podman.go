// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

// Package podman runs each sandbox as one container on a local or remote
// podman engine, reached over the libpod REST API on a unix socket. The
// boundary is the container's: kernel namespaces and cgroups around an OCI
// image, which is the `container` isolation class of the runtime contract.
//
// Three objects carry one sandbox. The workspace volume cella-ws-<id> holds
// the files and the identity that never changes. A record volume
// cella-rec-<id>-<n> holds the half that does, as labels, and is replaced
// rather than edited because podman fixes an object's labels at create. The
// container cella-<id> is the compute, and its status and clocks are the
// phase. Nothing is kept in this process, so a second driver over the same
// engine reads every sandbox back.
//
// Phases, from the container's status and the record's stop flag:
//
//	absent                                     Stopped
//	created, configured, initialized           Pending
//	running, paused                            Running
//	stopping                                   Stopping
//	removing                                   Deleting
//	exited, stopped, dead, stop flag set       Stopped
//	exited, stopped, dead, exit code 0         Stopped, with the code
//	exited, stopped, dead, exit code not 0     Failed, with the code
//
// Exec cancellation closes the caller's streams and ends Wait, and does not
// signal the process inside: libpod offers no way to signal an exec session,
// and the pid it reports belongs to the engine. Such a process is reaped when
// the sandbox is stopped or deleted.
package podman

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"maps"
	"net/http"
	"path"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"latere.ai/x/cella/manifest"
	v1 "latere.ai/x/cella/manifest/v1"
	driver "latere.ai/x/cella/runtime"
)

// The label keys this driver stamps. Every key is under the contract's group,
// so an object podman holds names which control plane owns it and nothing of
// where that control plane runs.
const (
	prefix          = "cella.latere.ai/"
	labelID         = prefix + "id"
	labelKind       = prefix + "kind"
	labelName       = prefix + "name"
	labelOwner      = prefix + "owner"
	labelCreatedAt  = prefix + "created-at"
	labelImage      = prefix + "image"
	labelDigest     = prefix + "image-digest"
	labelWorkspace  = prefix + "workspace-path"
	labelDisk       = prefix + "disk"
	labelGeneration = prefix + "generation"
	labelActivity   = prefix + "last-activity-at"
	labelStopped    = prefix + "stopped"
	labelTTL        = prefix + "ttl"
	labelAutoStop   = prefix + "auto-stop"
	labelAutoDelete = prefix + "auto-delete"
	labelEnv        = prefix + "env"
	labelUser       = prefix + "label."
	// The pool's three keys, on the record volume rather than the workspace
	// one: podman fixes a volume's labels at create, and an adoption
	// rewrites all three (spec 020).
	labelMesh      = prefix + "mesh"
	labelParent    = prefix + "parent"
	labelPool      = driver.PoolLabel
	labelAdopted   = prefix + "adopted-at"
	labelAdoptedBy = prefix + "adopted-by"
	labelAdoptedAs = prefix + "adopted-as"
)

// The kinds the id label's three objects distinguish themselves by.
const (
	kindWorkspace = "workspace"
	kindRecord    = "record"
	kindSandbox   = "sandbox"
)

// idleCommand keeps a sandbox created without a command Running, so it can be
// executed into. Every OCI base image can run it.
var idleCommand = []string{"sh", "-c", "while :; do sleep 3600; done"}

// stopGrace is how long the container gets to leave on its own before podman
// kills it. Short, because a caller polls for Stopped.
const stopGrace = 2

// cpuPeriod is the CFS period a cpu request is expressed as a quota over.
const cpuPeriod = 100000

// validID is what a sandbox id may be: podman's own name syntax, which the
// contract's sbx_ form satisfies. The driver refuses anything else rather
// than letting the engine decide.
var validID = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_.-]{0,127}$`)

// Options configures a driver.
type Options struct {
	// Socket is the libpod unix socket. Empty tries DefaultSockets in order.
	Socket string
	// PullTimeout bounds one image pull. Zero is DefaultPullTimeout.
	PullTimeout time.Duration
}

// DefaultPullTimeout bounds an image pull when Options does not.
const DefaultPullTimeout = 5 * time.Minute

// Driver implements the runtime contract against one podman engine.
type Driver struct {
	candidates  []string
	pullTimeout time.Duration
	conn        atomic.Pointer[client]

	mu    sync.Mutex
	locks map[string]*sync.Mutex
}

var _ driver.Driver = (*Driver)(nil)
var _ driver.Attacher = (*Driver)(nil)

// New builds a driver over the named socket, or over the default candidates
// when none is named. It opens no connection: Preflight decides which
// candidate answers.
func New(o Options) (*Driver, error) {
	candidates := DefaultSockets()
	if o.Socket != "" {
		candidates = []string{o.Socket}
	}
	if len(candidates) == 0 {
		return nil, fmt.Errorf("%w: no podman socket to try", driver.ErrInvalid)
	}
	d := &Driver{candidates: candidates, pullTimeout: o.PullTimeout, locks: map[string]*sync.Mutex{}}
	if d.pullTimeout <= 0 {
		d.pullTimeout = DefaultPullTimeout
	}
	d.conn.Store(newClient(candidates[0]))
	return d, nil
}

func (d *Driver) Name() string      { return "podman" }
func (d *Driver) Isolation() string { return v1.IsolationContainer }

// Capabilities declares what this driver enforces. Files, because the archive
// endpoints answer on a stopped container as well as a running one. Detach,
// because the driver keeps nothing in this process and a second instance over
// the same engine reads every sandbox back. Attach, because the engine runs an
// exec session with a TTY over a connection it speaks bytes both ways on.
// Pool, because a generation of the record is written once: the engine refuses
// a second volume of one name, which is the compare-and-swap adoption needs.
// Mesh, because one network per mesh carries the members and their aliases on
// it. Display and Input, because the desktop runs as a second process in the
// sandbox's own container and every operation on it is one exec session.
func (d *Driver) Capabilities() driver.Capabilities {
	return driver.Capabilities{Files: true, Detach: true, Attach: true, Pool: true, Mesh: true, Display: true, Input: true}
}

// Socket is the socket the driver last found answering, for the start-up line.
func (d *Driver) Socket() string { return d.conn.Load().socket }

// Preflight pings each candidate in order and keeps the first that answers.
// With none answering it names every candidate, which is the whole of what an
// operator has to fix.
func (d *Driver) Preflight(ctx context.Context) error {
	var reasons []string
	for _, socket := range d.candidates {
		c := newClient(socket)
		if err := c.ping(ctx, 5*time.Second); err != nil {
			reasons = append(reasons, fmt.Sprintf("%s: %v", socket, err))
			continue
		}
		d.conn.Store(c)
		return nil
	}
	return fmt.Errorf("podman: no engine answered; set CELLA_PODMAN_SOCKET (tried %s)", strings.Join(reasons, "; "))
}

// Ready reports whether the engine found at Preflight still answers.
func (d *Driver) Ready(ctx context.Context) error {
	return d.conn.Load().ping(ctx, 3*time.Second)
}

// Close releases the idle connections the client holds.
func (d *Driver) Close() error {
	d.conn.Load().hc.CloseIdleConnections()
	return nil
}

func (d *Driver) client() *client      { return d.conn.Load() }
func workspaceVolume(id string) string { return "cella-ws-" + id }
func containerName(id string) string   { return "cella-" + id }
func recordVolume(id string, n uint64) string {
	return "cella-rec-" + id + "-" + strconv.FormatUint(n, 10)
}

// lock serialises the read, write and sweep of one sandbox's record
// generations, so two mutations of one sandbox cannot both write generation
// n+1. Mutations of different sandboxes do not wait for each other.
func (d *Driver) lock(id string) func() {
	d.mu.Lock()
	m := d.locks[id]
	if m == nil {
		m = &sync.Mutex{}
		d.locks[id] = m
	}
	d.mu.Unlock()
	m.Lock()
	return m.Unlock
}

// ---- wire types ----

type specGenerator struct {
	Image          string            `json:"image"`
	Name           string            `json:"name"`
	Command        []string          `json:"command,omitempty"`
	Env            map[string]string `json:"env,omitempty"`
	Labels         map[string]string `json:"labels,omitempty"`
	WorkDir        string            `json:"work_dir,omitempty"`
	User           string            `json:"user,omitempty"`
	RestartPolicy  string            `json:"restart_policy,omitempty"`
	StopTimeout    *uint             `json:"stop_timeout,omitempty"`
	Volumes        []namedVolume     `json:"volumes,omitempty"`
	ResourceLimits *resourceLimits   `json:"resource_limits,omitempty"`
}

type namedVolume struct {
	Name    string   `json:"Name"`
	Dest    string   `json:"Dest"`
	Options []string `json:"Options,omitempty"`
}

type resourceLimits struct {
	CPU    *cpuLimit    `json:"cpu,omitempty"`
	Memory *memoryLimit `json:"memory,omitempty"`
}
type cpuLimit struct {
	Quota  int64  `json:"quota,omitempty"`
	Period uint64 `json:"period,omitempty"`
}
type memoryLimit struct {
	Limit int64 `json:"limit,omitempty"`
}

type volumeCreate struct {
	Name   string            `json:"Name"`
	Labels map[string]string `json:"Labels,omitempty"`
}

type volumeInspect struct {
	Name   string            `json:"Name"`
	Labels map[string]string `json:"Labels"`
}

type containerInspect struct {
	State struct {
		Status     string    `json:"Status"`
		ExitCode   int       `json:"ExitCode"`
		StartedAt  time.Time `json:"StartedAt"`
		FinishedAt time.Time `json:"FinishedAt"`
	} `json:"State"`
	Config struct {
		User string `json:"User"`
	} `json:"Config"`
}

type containerListItem struct {
	State     string            `json:"State"`
	ExitCode  int               `json:"ExitCode"`
	Labels    map[string]string `json:"Labels"`
	StartedAt int64             `json:"StartedAt"`
	ExitedAt  int64             `json:"ExitedAt"`
	Exited    bool              `json:"Exited"`
}

// status is the part of a container Inspect and List agree on, plus the user
// it runs as, which only an Inspect reports and which a re-projected file
// takes as its owner.
type status struct {
	present    bool
	state      string
	exitCode   int
	startedAt  time.Time
	finishedAt time.Time
	user       string
}

// ---- identity and record ----

// identity is the half of a sandbox that is fixed at create, read from the
// workspace volume's labels.
type identity struct {
	id, name, owner string
	image, digest   string
	workspacePath   string
	disk            string
	createdAt       time.Time
	// display is the desktop the sandbox was created with, nil for one that
	// asked for none, and ports are what it declared runs inside it. Both are
	// immutable by spec 003, which is why they live here and not in record.
	display *driver.Geometry
	ports   []driver.Port
	// mesh is the mesh this sandbox belongs to and parent the sandbox that
	// spawned it (spec 022). Both are fixed at create, which is why they
	// are on the identity and not on the record.
	mesh, parent string
}

func (i identity) labels() map[string]string {
	l := map[string]string{
		labelID: i.id, labelKind: kindWorkspace, labelName: i.name, labelOwner: i.owner,
		labelCreatedAt: i.createdAt.Format(time.RFC3339Nano), labelImage: i.image,
		labelWorkspace: i.workspacePath,
	}
	if i.digest != "" {
		l[labelDigest] = i.digest
	}
	if i.disk != "" {
		l[labelDisk] = i.disk
	}
	if g := geometryLabel(i.display); g != "" {
		l[labelDisplay] = g
	}
	if p := portsLabel(i.ports); p != "" {
		l[labelPorts] = p
	}
	if i.mesh != "" {
		l[labelMesh] = i.mesh
	}
	if i.parent != "" {
		l[labelParent] = i.parent
	}
	return l
}

func identityOf(l map[string]string) identity {
	created, _ := time.Parse(time.RFC3339Nano, l[labelCreatedAt])
	p := l[labelWorkspace]
	if p == "" {
		p = driver.DefaultWorkdir
	}
	return identity{
		id: l[labelID], name: l[labelName], owner: l[labelOwner], image: l[labelImage],
		digest: l[labelDigest], workspacePath: p, disk: l[labelDisk], createdAt: created.UTC(),
		display: geometryOf(l[labelDisplay]), ports: portsOf(l[labelPorts]),
		mesh: l[labelMesh], parent: l[labelParent],
	}
}

// record is the half that changes. It lives on its own volume so that writing
// it neither edits a label, which podman refuses, nor re-creates the
// container, which would kill the workload.
type record struct {
	generation                uint64
	labels, env               map[string]string
	stopped                   bool
	lastActivityAt            time.Time
	ttl, autoStop, autoDelete time.Duration
	// pool marks a prewarmed entry, and the three below are what an
	// adoption writes that the workspace volume's fixed labels cannot
	// carry: who owns the sandbox now, what it is called, and when it
	// began, which for an adopted sandbox is the adoption and not the
	// prewarm (spec 020).
	pool        bool
	owner, name string
	adoptedAt   time.Time
}

func (r record) volumeLabels(id string) map[string]string {
	l := map[string]string{
		labelID: id, labelKind: kindRecord,
		labelGeneration: strconv.FormatUint(r.generation, 10),
		labelActivity:   r.lastActivityAt.Format(time.RFC3339Nano),
	}
	if r.stopped {
		l[labelStopped] = "true"
	}
	if r.pool {
		l[labelPool] = "true"
	}
	if !r.adoptedAt.IsZero() {
		l[labelAdopted] = r.adoptedAt.Format(time.RFC3339Nano)
		l[labelAdoptedBy] = r.owner
		l[labelAdoptedAs] = r.name
	}
	for key, d := range map[string]time.Duration{labelTTL: r.ttl, labelAutoStop: r.autoStop, labelAutoDelete: r.autoDelete} {
		if d > 0 {
			l[key] = d.String()
		}
	}
	if len(r.env) > 0 {
		if b, err := json.Marshal(r.env); err == nil {
			l[labelEnv] = string(b)
		}
	}
	for k, v := range r.labels {
		l[labelUser+k] = v
	}
	return l
}

func recordOf(l map[string]string) record {
	r := record{stopped: l[labelStopped] == "true", pool: l[labelPool] == "true",
		owner: l[labelAdoptedBy], name: l[labelAdoptedAs]}
	if t, err := time.Parse(time.RFC3339Nano, l[labelAdopted]); err == nil {
		r.adoptedAt = t.UTC()
	}
	r.generation, _ = strconv.ParseUint(l[labelGeneration], 10, 64)
	if t, err := time.Parse(time.RFC3339Nano, l[labelActivity]); err == nil {
		r.lastActivityAt = t.UTC()
	}
	for key, into := range map[string]*time.Duration{labelTTL: &r.ttl, labelAutoStop: &r.autoStop, labelAutoDelete: &r.autoDelete} {
		if d, err := time.ParseDuration(l[key]); err == nil {
			*into = d
		}
	}
	if raw := l[labelEnv]; raw != "" {
		env := map[string]string{}
		if json.Unmarshal([]byte(raw), &env) == nil {
			r.env = env
		}
	}
	for k, v := range l {
		if name, ok := strings.CutPrefix(k, labelUser); ok {
			if r.labels == nil {
				r.labels = map[string]string{}
			}
			r.labels[name] = v
		}
	}
	return r
}

// ---- engine calls ----

func (d *Driver) createVolume(ctx context.Context, name string, labels map[string]string) error {
	return d.client().json(ctx, http.MethodPost, "/volumes/create", volumeCreate{Name: name, Labels: labels}, nil)
}

func (d *Driver) removeVolume(ctx context.Context, name string) error {
	err := d.client().json(ctx, http.MethodDelete, "/volumes/"+name+"?force=true", nil, nil)
	if notFound(err) {
		return nil
	}
	return err
}

func (d *Driver) inspectVolume(ctx context.Context, name string) (volumeInspect, error) {
	var vi volumeInspect
	err := d.client().json(ctx, http.MethodGet, "/volumes/"+name+"/json", nil, &vi)
	if notFound(err) {
		return vi, driver.ErrNotFound
	}
	return vi, err
}

// listVolumes returns every volume this driver stamped, both kinds.
func (d *Driver) listVolumes(ctx context.Context) ([]volumeInspect, error) {
	var out []volumeInspect
	err := d.client().json(ctx, http.MethodGet, "/volumes/json?"+query("filters", labelKeyFilter(labelID)), nil, &out)
	return out, err
}

func (d *Driver) inspectContainer(ctx context.Context, id string) (status, error) {
	var ci containerInspect
	err := d.client().json(ctx, http.MethodGet, "/containers/"+containerName(id)+"/json", nil, &ci)
	if notFound(err) {
		return status{}, nil
	}
	if err != nil {
		return status{}, err
	}
	return status{present: true, state: ci.State.Status, exitCode: ci.State.ExitCode,
		startedAt: engineInstant(ci.State.StartedAt), finishedAt: engineInstant(ci.State.FinishedAt),
		user: ci.Config.User}, nil
}

// engineInstant is one resolution for every instant a container reports. The
// list endpoint carries them as unix seconds and the inspect endpoint to the
// nanosecond, so the finer one is cut down: the same sandbox must read the
// same clock whether a caller asked for it by name or in a list, which is what
// a controller comparing an observed state against a stored one depends on.
func engineInstant(t time.Time) time.Time {
	if t.IsZero() {
		return time.Time{}
	}
	return t.UTC().Truncate(time.Second)
}

// listContainers returns every sandbox container keyed by sandbox id.
func (d *Driver) listContainers(ctx context.Context) (map[string]status, error) {
	var items []containerListItem
	path := "/containers/json?" + query("all", "true", "filters", labelKeyFilter(labelID))
	if err := d.client().json(ctx, http.MethodGet, path, nil, &items); err != nil {
		return nil, err
	}
	out := map[string]status{}
	for _, it := range items {
		id := it.Labels[labelID]
		if id == "" || it.Labels[labelKind] != kindSandbox {
			continue
		}
		s := status{present: true, state: it.State, exitCode: it.ExitCode}
		if it.StartedAt > 0 {
			s.startedAt = time.Unix(it.StartedAt, 0).UTC()
		}
		if it.Exited && it.ExitedAt > 0 {
			s.finishedAt = time.Unix(it.ExitedAt, 0).UTC()
		}
		out[id] = s
	}
	return out, nil
}

func (d *Driver) removeContainer(ctx context.Context, id string) error {
	err := d.client().json(ctx, http.MethodDelete, "/containers/"+containerName(id)+"?"+query("force", "true", "timeout", "0"), nil, nil)
	if notFound(err) {
		return nil
	}
	return err
}

// imageDigest returns the digest of a local image, pulling it first when the
// engine does not have it. The pull is bounded so a create cannot hang on a
// registry.
func (d *Driver) imageDigest(ctx context.Context, image string) (string, error) {
	if digest, err := d.localDigest(ctx, image); err == nil {
		return digest, nil
	}
	pullCtx, cancel := context.WithTimeout(ctx, d.pullTimeout)
	defer cancel()
	resp, err := d.client().do(pullCtx, http.MethodPost, d.client().libpodURL("/images/pull?"+query("reference", image)), nil)
	if err != nil {
		return "", fmt.Errorf("podman: pulling %s: %w", image, err)
	}
	defer func() { _ = resp.Body.Close() }()
	if serr := statusErr(resp); serr != nil {
		return "", fmt.Errorf("podman: pulling %s: %w", image, serr)
	}
	// The pull streams progress; the image is present only once it ends.
	if _, err := io.Copy(io.Discard, resp.Body); err != nil {
		return "", fmt.Errorf("podman: pulling %s: %w", image, err)
	}
	digest, err := d.localDigest(ctx, image)
	if err != nil {
		return "", fmt.Errorf("podman: %s is absent after the pull: %w", image, err)
	}
	return digest, nil
}

func (d *Driver) localDigest(ctx context.Context, image string) (string, error) {
	var out struct {
		Digest string `json:"Digest"`
	}
	if err := d.client().json(ctx, http.MethodGet, "/images/"+image+"/json", nil, &out); err != nil {
		return "", err
	}
	return out.Digest, nil
}

// ---- record generations ----

// records returns every record generation of one sandbox, highest first.
func (d *Driver) records(ctx context.Context, id string) ([]record, error) {
	filter, err := json.Marshal(map[string][]string{"label": {labelID + "=" + id, labelKind + "=" + kindRecord}})
	if err != nil {
		return nil, err
	}
	var vols []volumeInspect
	if err := d.client().json(ctx, http.MethodGet, "/volumes/json?"+query("filters", string(filter)), nil, &vols); err != nil {
		return nil, err
	}
	out := make([]record, 0, len(vols))
	for _, v := range vols {
		out = append(out, recordOf(v.Labels))
	}
	slices.SortFunc(out, func(a, b record) int { return int(b.generation) - int(a.generation) })
	return out, nil
}

// current returns the highest generation and sweeps the ones below it, which
// a crash between the write and the removal of a mutation can leave.
func (d *Driver) current(ctx context.Context, id string) (record, error) {
	all, err := d.records(ctx, id)
	if err != nil {
		return record{}, err
	}
	if len(all) == 0 {
		return record{}, driver.ErrNotFound
	}
	for _, stale := range all[1:] {
		_ = d.removeVolume(ctx, recordVolume(id, stale.generation))
	}
	return all[0], nil
}

// writeRecord stores r as the next generation and removes the one it replaces,
// in that order, so a crash between them leaves the newer readable.
func (d *Driver) writeRecord(ctx context.Context, id string, previous uint64, r record) error {
	r.generation = previous + 1
	if err := d.createVolume(ctx, recordVolume(id, r.generation), r.volumeLabels(id)); err != nil {
		return err
	}
	if previous > 0 {
		_ = d.removeVolume(ctx, recordVolume(id, previous))
	}
	return nil
}

// edit reads the current record, applies fn, and writes the next generation.
// fn returning false means the read found nothing to change.
func (d *Driver) edit(ctx context.Context, id string, fn func(*record) bool) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if !validID.MatchString(id) {
		return driver.ErrInvalid
	}
	unlock := d.lock(id)
	defer unlock()
	if _, err := d.inspectVolume(ctx, workspaceVolume(id)); err != nil {
		return err
	}
	r, err := d.current(ctx, id)
	if err != nil {
		return err
	}
	previous := r.generation
	if !fn(&r) {
		return nil
	}
	return d.writeRecord(ctx, id, previous, r)
}

// ---- lifecycle ----

// Create makes the workspace volume, the first record, and the container, and
// starts it. A step that fails removes what the steps before it made, so a
// refused create leaves the engine as it found it.
func (d *Driver) Create(ctx context.Context, s driver.CreateSpec) (driver.Ref, error) {
	if err := ctx.Err(); err != nil {
		return driver.Ref{}, err
	}
	if !validID.MatchString(s.ID) || s.Lifecycle.TTL < 0 || s.Lifecycle.AutoStop < 0 || s.Lifecycle.AutoDelete < 0 {
		return driver.Ref{}, driver.ErrInvalid
	}
	if err := s.CheckPrewarm(); err != nil {
		return driver.Ref{}, err
	}
	if s.Image == "" {
		return driver.Ref{}, fmt.Errorf("%w: podman needs an image", driver.ErrInvalid)
	}
	if len(s.Args) > 0 && len(s.Command) == 0 {
		return driver.Ref{}, driver.ErrInvalid
	}
	root, err := workspaceRoot(s.Workspace.Path)
	if err != nil {
		return driver.Ref{}, err
	}
	workdir := s.Workdir
	if workdir == "" {
		workdir = root
	} else if _, err := relative(root, workdir); err != nil {
		return driver.Ref{}, err
	}
	limits, err := resourcesOf(s.Resources)
	if err != nil {
		return driver.Ref{}, err
	}
	digest, err := d.imageDigest(ctx, s.Image)
	if err != nil {
		return driver.Ref{}, err
	}

	unlock := d.lock(s.ID)
	defer unlock()
	// The create instant is cut to the same resolution the engine reports its
	// own clocks at, so a container that starts in the same second as the
	// sandbox was created does not read as started before it.
	now := time.Now().UTC().Truncate(time.Second)
	ident := identity{id: s.ID, name: s.Name, owner: s.Owner, image: s.Image, digest: digest,
		workspacePath: root, disk: s.Resources.Disk, createdAt: now,
		display: s.Display, ports: s.Ports, mesh: s.Mesh.ID, parent: s.Parent}
	if err := d.createVolume(ctx, workspaceVolume(s.ID), ident.labels()); err != nil {
		if conflict(err) {
			return driver.Ref{}, driver.ErrAlreadyExists
		}
		return driver.Ref{}, err
	}
	undo := func() {
		clean := context.WithoutCancel(ctx)
		_ = d.removeContainer(clean, s.ID)
		_ = d.removeVolume(clean, recordVolume(s.ID, 1))
		_ = d.removeVolume(clean, workspaceVolume(s.ID))
	}
	env := displayEnv(s, tokenEnv(s.Token, egressEnv(s.Env, s.Egress)))
	rec := record{labels: maps.Clone(s.Labels), env: maps.Clone(env), lastActivityAt: time.Now().UTC(),
		ttl: s.Lifecycle.TTL, autoStop: s.Lifecycle.AutoStop, autoDelete: s.Lifecycle.AutoDelete,
		pool: s.Prewarm}
	if err := d.writeRecord(ctx, s.ID, 0, rec); err != nil {
		undo()
		return driver.Ref{}, err
	}
	command := append(slices.Clone(s.Command), s.Args...)
	if len(command) == 0 {
		command = idleCommand
	}
	grace := uint(stopGrace)
	sg := specGenerator{
		Image: s.Image, Name: containerName(s.ID), Command: command, Env: maps.Clone(env),
		Labels: map[string]string{labelID: s.ID, labelKind: kindSandbox}, WorkDir: workdir,
		User: s.User, RestartPolicy: "no", StopTimeout: &grace,
		Volumes:        []namedVolume{{Name: workspaceVolume(s.ID), Dest: root, Options: []string{"rw"}}},
		ResourceLimits: limits,
	}
	if err := d.client().json(ctx, http.MethodPost, "/containers/create", sg, nil); err != nil {
		undo()
		return driver.Ref{}, fmt.Errorf("podman: creating the container: %w", err)
	}
	// The mesh network is joined between the create and the start, so a
	// member is reachable by its peers from the moment its workload runs
	// (spec 022).
	if err := d.joinMesh(ctx, s.ID, s.Name, s.Mesh.ID); err != nil {
		undo()
		return driver.Ref{}, err
	}
	// The authority the gateway signs with is projected between the create
	// and the start, so the workload's first request already trusts the
	// door its environment points at (spec 018).
	if s.Egress.CAPEM != "" {
		if err := d.putEgressCA(ctx, s.ID, s.Egress.CAPEM); err != nil {
			undo()
			return driver.Ref{}, err
		}
	}
	// The identity of spec 006 is projected in the same window, so the
	// workload's first call to the control plane carries it.
	if len(s.Token) > 0 {
		if err := d.putToken(ctx, s.ID, s.User, s.Token); err != nil {
			undo()
			return driver.Ref{}, err
		}
	}
	if err := d.client().json(ctx, http.MethodPost, "/containers/"+containerName(s.ID)+"/start", nil, nil); err != nil {
		undo()
		return driver.Ref{}, fmt.Errorf("podman: starting the container: %w", err)
	}
	d.startDesktop(ctx, s.ID, s.Display)
	return driver.Ref{ID: s.ID}, nil
}

// exists reports whether the sandbox is there at all, which is the workspace
// volume: it outlives the container and every record generation.
func (d *Driver) exists(ctx context.Context, id string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if !validID.MatchString(id) {
		return driver.ErrInvalid
	}
	_, err := d.inspectVolume(ctx, workspaceVolume(id))
	return err
}

// Start runs the container again and clears the stop flag. Podman answers a
// container that is already running with 304, so a second Start moves no clock.
func (d *Driver) Start(ctx context.Context, id string) error {
	if err := d.exists(ctx, id); err != nil {
		return err
	}
	if err := d.client().json(ctx, http.MethodPost, "/containers/"+containerName(id)+"/start", nil, nil); err != nil {
		return fmt.Errorf("podman: starting %s: %w", id, err)
	}
	// The desktop lives in the process tree the stop ended, so a start builds
	// it again; spec 023 states the rebuild and the supervisor is idempotent.
	if vi, err := d.inspectVolume(ctx, workspaceVolume(id)); err == nil {
		d.startDesktop(ctx, id, geometryOf(vi.Labels[labelDisplay]))
	}
	return d.edit(ctx, id, func(r *record) bool {
		if !r.stopped {
			return false
		}
		r.stopped = false
		r.lastActivityAt = time.Now().UTC()
		return true
	})
}

// Stop sets the stop flag and stops the container. The flag is what tells a
// sandbox an operator stopped from one whose process died: podman kills after
// the grace period and the container then reads exit 137 either way.
func (d *Driver) Stop(ctx context.Context, id string) error {
	if err := d.exists(ctx, id); err != nil {
		return err
	}
	if err := d.edit(ctx, id, func(r *record) bool {
		if r.stopped {
			return false
		}
		r.stopped = true
		return true
	}); err != nil {
		return err
	}
	if err := d.client().json(ctx, http.MethodPost, "/containers/"+containerName(id)+"/stop?"+query("timeout", strconv.Itoa(stopGrace)), nil, nil); err != nil {
		return fmt.Errorf("podman: stopping %s: %w", id, err)
	}
	return nil
}

// Delete removes the container, every record generation, and the workspace
// volume. Deleting what is not there is not an error.
func (d *Driver) Delete(ctx context.Context, id string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if !validID.MatchString(id) {
		return driver.ErrInvalid
	}
	unlock := d.lock(id)
	defer unlock()
	return d.deleteLocked(ctx, id)
}

// deleteLocked is Delete with this sandbox's lock already held, for the
// adoption that discards an entry it could not finish projecting into.
func (d *Driver) deleteLocked(ctx context.Context, id string) error {
	// The mesh is read before anything goes, because the membership the
	// last-member rule counts is on the workspace volume this delete removes.
	mesh := ""
	if state, err := d.Inspect(ctx, id); err == nil {
		mesh = state.MeshID
	}
	if err := d.removeContainer(ctx, id); err != nil {
		return err
	}
	all, err := d.records(ctx, id)
	if err != nil {
		return err
	}
	for _, r := range all {
		if err := d.removeVolume(ctx, recordVolume(id, r.generation)); err != nil {
			return err
		}
	}
	if err := d.removeVolume(ctx, workspaceVolume(id)); err != nil {
		return err
	}
	return d.leaveMesh(ctx, id, mesh)
}

// Update writes the next record. Podman fixes a container's labels and
// environment at create, so neither is rewritten: the labels are read from the
// record, and the environment reaches the sandbox through Exec.
func (d *Driver) Update(ctx context.Context, id string, c driver.Change) error {
	adoption, err := c.Adoption()
	if err != nil {
		return err
	}
	if adoption != nil {
		return d.adopt(ctx, id, *adoption)
	}
	if c.Lifecycle != nil && (c.Lifecycle.TTL < 0 || c.Lifecycle.AutoStop < 0 || c.Lifecycle.AutoDelete < 0) {
		return driver.ErrInvalid
	}
	// The token is written into the container rather than into the record,
	// because the container's file system is where the workload reads it.
	// It goes in before the record so a re-projection that fails leaves the
	// record naming the file the container still holds.
	if len(c.Token) > 0 {
		running, err := d.inspectContainer(ctx, id)
		if err != nil {
			return err
		}
		if !running.present {
			return driver.ErrNotFound
		}
		if err := d.putToken(ctx, id, running.user, c.Token); err != nil {
			return err
		}
	}
	return d.edit(ctx, id, func(r *record) bool {
		if c.Labels != nil {
			r.labels = maps.Clone(*c.Labels)
		}
		if c.Env != nil {
			r.env = maps.Clone(*c.Env)
		}
		if c.Lifecycle != nil {
			r.ttl, r.autoStop, r.autoDelete = c.Lifecycle.TTL, c.Lifecycle.AutoStop, c.Lifecycle.AutoDelete
		}
		return true
	})
}

// Touch advances the sandbox's last activity, which is what the reaper's idle
// rule reads.
func (d *Driver) Touch(ctx context.Context, id string) error {
	return d.edit(ctx, id, func(r *record) bool {
		r.lastActivityAt = time.Now().UTC()
		return true
	})
}

// ---- reading state back ----

// Inspect assembles one sandbox from its workspace volume, its record, and its
// container.
func (d *Driver) Inspect(ctx context.Context, id string) (driver.State, error) {
	if err := ctx.Err(); err != nil {
		return driver.State{}, err
	}
	if !validID.MatchString(id) {
		return driver.State{}, driver.ErrInvalid
	}
	vi, err := d.inspectVolume(ctx, workspaceVolume(id))
	if err != nil {
		return driver.State{}, err
	}
	rec, err := d.current(ctx, id)
	if err != nil {
		return driver.State{}, err
	}
	st, err := d.inspectContainer(ctx, id)
	if err != nil {
		return driver.State{}, err
	}
	ident := identityOf(vi.Labels)
	state := stateOf(ident, rec, st)
	state.Conditions, state.Ports = d.observe(ctx, id, ident.display, ident.ports, state.Phase == driver.Running)
	return state, nil
}

// List reads every sandbox with two calls, one over the volumes and one over
// the containers, and narrows in this process: podman filters on a label, and
// the contract's filter is a phase as well.
func (d *Driver) List(ctx context.Context, f driver.Filter) ([]driver.State, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	vols, err := d.listVolumes(ctx)
	if err != nil {
		return nil, err
	}
	containers, err := d.listContainers(ctx)
	if err != nil {
		return nil, err
	}
	identities := map[string]identity{}
	records := map[string]record{}
	for _, v := range vols {
		id := v.Labels[labelID]
		if id == "" {
			continue
		}
		switch v.Labels[labelKind] {
		case kindWorkspace:
			identities[id] = identityOf(v.Labels)
		case kindRecord:
			if r := recordOf(v.Labels); r.generation >= records[id].generation {
				records[id] = r
			}
		}
	}
	out := []driver.State{}
	for id, ident := range identities {
		if s := stateOf(ident, records[id], containers[id]); f.Selects(s) {
			out = append(out, s)
		}
	}
	slices.SortFunc(out, func(a, b driver.State) int { return strings.Compare(a.ID, b.ID) })
	return out, nil
}

// stateOf is the one place the three objects become a State, so Inspect and
// List cannot disagree.
func stateOf(i identity, r record, st status) driver.State {
	s := driver.State{
		ID: i.id, Name: i.name, Owner: i.owner, Isolation: v1.IsolationContainer,
		Labels: maps.Clone(r.labels), CreatedAt: i.createdAt, LastActivityAt: r.lastActivityAt,
		AutoStop: r.autoStop, AutoDelete: r.autoDelete, Pool: r.pool,
		MeshID: i.mesh, Parent: i.parent,
	}
	// An adopted sandbox reads its owner, its name and its beginning from
	// the record, which is the half a mutation may rewrite. Nothing else
	// writes those three, so a sandbox that was created rather than adopted
	// reads the workspace volume's fixed labels as before.
	if !r.adoptedAt.IsZero() {
		s.Owner, s.Name, s.CreatedAt = r.owner, r.name, r.adoptedAt
	}
	if r.ttl > 0 {
		s.ExpiresAt = s.CreatedAt.Add(r.ttl)
	}
	s.Phase, s.ExitCode = phaseOf(st, r.stopped)
	if st.present {
		s.StartedAt = st.startedAt
		if s.Phase != driver.Running && !st.finishedAt.IsZero() {
			s.StoppedAt = st.finishedAt
		}
	}
	if s.StartedAt.IsZero() {
		s.StartedAt = i.createdAt
	}
	return s
}

// phaseOf is the package documentation's table. The stop flag is read only for
// a container that has ended, because that is where an operator's stop and a
// workload that died look the same to podman.
func phaseOf(st status, stopped bool) (string, *int) {
	if !st.present {
		return driver.Stopped, nil
	}
	switch st.state {
	case "running", "paused":
		return driver.Running, nil
	case "created", "configured", "initialized":
		return driver.Pending, nil
	case "stopping":
		return "Stopping", nil
	case "removing":
		return "Deleting", nil
	case "exited", "stopped", "dead":
		if stopped {
			return driver.Stopped, nil
		}
		code := st.exitCode
		if code != 0 {
			return "Failed", &code
		}
		return driver.Stopped, &code
	default:
		return driver.Pending, nil
	}
}

// ---- resources and paths ----

// resourcesOf maps the manifest's quantities onto libpod's limits: cpu as a
// CFS quota over a fixed period, memory as bytes. Disk has no enforcement
// point on podman's local volume driver, so it is recorded and not applied.
func resourcesOf(r driver.Resources) (*resourceLimits, error) {
	var out resourceLimits
	set := false
	if r.CPU != "" {
		milli, err := manifest.ParseQuantity(v1.Quantity(r.CPU))
		if err != nil {
			return nil, fmt.Errorf("%w: resources.cpu: %w", driver.ErrInvalid, err)
		}
		if milli > 0 {
			out.CPU = &cpuLimit{Period: cpuPeriod, Quota: milli * cpuPeriod / 1000}
			set = true
		}
	}
	if r.Memory != "" {
		milli, err := manifest.ParseQuantity(v1.Quantity(r.Memory))
		if err != nil {
			return nil, fmt.Errorf("%w: resources.memory: %w", driver.ErrInvalid, err)
		}
		if milli > 0 {
			out.Memory = &memoryLimit{Limit: milli / 1000}
			set = true
		}
	}
	if r.Disk != "" {
		if _, err := manifest.ParseQuantity(v1.Quantity(r.Disk)); err != nil {
			return nil, fmt.Errorf("%w: resources.disk: %w", driver.ErrInvalid, err)
		}
	}
	if !set {
		return nil, nil
	}
	return &out, nil
}

// workspaceRoot is where the sandbox's own files live. Empty is the contract's
// default; anything else is absolute, clean, and not the root of the image.
func workspaceRoot(p string) (string, error) {
	if p == "" {
		return driver.DefaultWorkdir, nil
	}
	if strings.Contains(p, "\x00") || !path.IsAbs(p) || path.Clean(p) != p || p == "/" {
		return "", fmt.Errorf("%w: workspace path %q", driver.ErrInvalid, p)
	}
	return p, nil
}

// relative turns an absolute path inside the workspace into its name relative
// to the workspace root, and refuses anything outside it.
func relative(root, p string) (string, error) {
	if strings.Contains(p, "\x00") || !path.IsAbs(p) {
		return "", fmt.Errorf("%w: path %q is not absolute", driver.ErrInvalid, p)
	}
	if slices.Contains(strings.Split(p, "/"), "..") {
		return "", fmt.Errorf("%w: path %q traverses", driver.ErrInvalid, p)
	}
	p = path.Clean(p)
	if p == root {
		return ".", nil
	}
	rest, ok := strings.CutPrefix(p, root+"/")
	if !ok {
		return "", fmt.Errorf("%w: path %q is outside the workspace", driver.ErrInvalid, p)
	}
	return rest, nil
}
