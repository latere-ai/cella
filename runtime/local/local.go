// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

// Package local is the driver of the process isolation class (spec 081): each
// sandbox's commands run on the machine the driver runs on, confined by the
// operating system's own sandbox through latere.ai/x/pkg/hostsandbox, which is
// Seatbelt on macOS and bubblewrap with seccomp on Linux.
//
// It is built for a person's own computer. The person grants folders, and a
// sandbox may work in one of them in place: the folder is bound as it is,
// never copied, and never deleted with the sandbox. Everything else a sandbox
// holds is a scratch directory under the driver's data directory, which is
// all a delete removes. The home directory is denied to every command but for
// what is granted, the always-deny table of credential paths holds even inside
// a grant, and the network is the host's own allow list or none.
//
// A command with no stdin runs as a detached stage, so it survives a crash of
// the process that started it, and one with stdin as an attached stage whose
// pipes this driver holds. The file operations run in this process, outside
// the operating system's sandbox, so they enforce the same grants and the
// same deny table themselves.
package local

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	goruntime "runtime"
	"slices"
	"strings"
	"sync"
	"syscall"

	"latere.ai/x/pkg/hostsandbox"

	v1 "latere.ai/x/cella/manifest/v1"
	driver "latere.ai/x/cella/runtime"
)

// Name is this driver's name.
const Name = "local"

// The two network modes a host allows.
const (
	NetworkNone      = "none"
	NetworkAllowlist = "allowlist"
)

// ErrRunsAsRoot is a driver started as root. It confines a person's own
// account, and as root every grant would mean nothing. Its code is
// local_runs_as_root: a worker stops on it before it registers, so no
// response carries it, and a program running the worker tests for it with
// errors.Is.
var ErrRunsAsRoot = errors.New("local_runs_as_root: the local driver refuses to run as root; run it as the person whose folders it grants")

// ErrLost is an attached command that ended with the process that held its
// pipes, which reports exit code -1.
var ErrLost = fmt.Errorf("%w: the command was lost with the process that held it", driver.ErrNotRunning)

// Options configures the driver. Grants, ReadPaths, Deny and Network are the
// host's policy, which no caller widens.
type Options struct {
	// DataDir holds every sandbox's scratch directory, sandboxes/<id> below
	// it.
	DataDir string
	// Grants are the host folders a sandbox may use as its workspace.
	Grants []Grant
	// ReadPaths are files or folders a command may read where they are, such
	// as a toolchain outside every PATH directory. None may be on the deny
	// table.
	ReadPaths []string
	// Deny are paths an embedder denies on top of the always-deny table,
	// such as its own configuration. A leading ~ is the home directory.
	Deny []string
	// Network is what the host allows a sandbox to reach.
	Network Network
	// Home is the person's home directory, denied to every command but for
	// what is granted. Empty reads it from the operating system.
	Home string
	// Lookup reads one variable of the process environment, which a command
	// inherits its allowlisted variables from, PATH among them. Nil is
	// os.LookupEnv.
	Lookup func(string) (string, bool)
}

// Grant is one host folder a sandbox may use: an absolute, clean path to an
// existing directory the process's user owns, writable or read-only.
type Grant struct {
	Path  string
	Write bool
}

// Network is what the host allows: none, or an allow list of hosts.
type Network struct {
	Mode  string
	Hosts []string
}

// systemDirs are the directories no grant may be or be under, by operating
// system: what the person's tools run from and the system's own state.
var systemDirs = map[string][]string{
	"darwin": {"/System", "/usr", "/bin", "/sbin", "/etc", "/var", "/Library", "/Applications", "/opt"},
	"linux":  {"/usr", "/bin", "/sbin", "/etc", "/var", "/boot", "/proc", "/sys", "/dev"},
}

// Driver runs sandboxes as confined host processes. One Driver owns its data
// directory; a second built over the same directory reads the sandboxes back
// from their records.
type Driver struct {
	data    string
	home    string
	grants  []Grant
	reads   []string
	deny    []string
	network Network
	lookup  func(string) (string, bool)
	sandbox *hostsandbox.Driver
	// goos and euid are the platform and the effective user, read once and
	// replaced by tests that check the refusals of another platform.
	goos string
	euid int

	mu sync.Mutex
	// attached is every command with stdin running now, by sandbox, which a
	// stop, a delete and Close end.
	attached map[string]map[*attachedExec]struct{}
	// self is this process as the record of an attached command names its
	// holder, read by holder the first time it is needed.
	self process
}

var (
	_ driver.Driver        = (*Driver)(nil)
	_ driver.FileStore     = (*Driver)(nil)
	_ driver.HostDescriber = (*Driver)(nil)
)

// New builds the driver over its options, refusing a grant, a read path or a
// network the host's policy cannot hold.
func New(o Options) (*Driver, error) {
	if o.DataDir == "" {
		return nil, fmt.Errorf("%w: the local driver needs a data directory", driver.ErrInvalid)
	}
	data, err := filepath.Abs(o.DataDir)
	if err != nil {
		return nil, err
	}
	if err = os.MkdirAll(filepath.Join(data, "sandboxes"), 0o700); err != nil {
		return nil, err
	}
	home := o.Home
	if home == "" {
		if home, err = os.UserHomeDir(); err != nil {
			return nil, fmt.Errorf("local: reading the home directory: %w", err)
		}
	}
	home = resolved(filepath.Clean(home))
	lookup := o.Lookup
	if lookup == nil {
		lookup = os.LookupEnv
	}
	d := &Driver{
		data: resolved(data), home: home, lookup: lookup, goos: goruntime.GOOS, euid: os.Geteuid(),
		attached: map[string]map[*attachedExec]struct{}{},
	}
	for _, p := range append(slices.Clone(hostsandbox.AlwaysDenyRead), o.Deny...) {
		d.deny = append(d.deny, expand(home, p))
	}
	for _, g := range o.Grants {
		grant, err := d.checkGrant(g)
		if err != nil {
			return nil, err
		}
		d.grants = append(d.grants, grant)
	}
	for _, p := range o.ReadPaths {
		read, err := d.checkRead(p)
		if err != nil {
			return nil, err
		}
		d.reads = append(d.reads, read)
	}
	if d.network, err = checkNetwork(o.Network); err != nil {
		return nil, err
	}
	d.sandbox = newSandbox(hostsandbox.Config{
		Home: home, Lookup: lookup,
		Alternative: "Or run this worker with another runtime, such as CELLA_RUNTIME=podman.",
	})
	// A worker killed outright leaves its attached commands running, and
	// this is the next worker over the same data directory starting.
	if err = d.endOrphans(context.Background()); err != nil {
		return nil, fmt.Errorf("local: reading the sandboxes: %w", err)
	}
	return d, nil
}

// newSandbox builds the operating system's sandbox the driver runs stages
// under. A test replaces it to resolve the sandbox runtime to a stand-in.
var newSandbox = hostsandbox.New

// expand resolves a leading ~ against home and cleans the path.
func expand(home, p string) string {
	switch {
	case p == "~":
		return home
	case strings.HasPrefix(p, "~/"):
		return filepath.Join(home, p[2:])
	}
	return filepath.Clean(p)
}

// resolved is a path with its symbolic links followed, or the path as it is
// where it does not exist yet.
func resolved(p string) string {
	if real, err := filepath.EvalSymlinks(p); err == nil {
		return real
	}
	return p
}

// under reports whether p is base or below it.
func under(p, base string) bool {
	return p == base || base == "/" || strings.HasPrefix(p, base+string(filepath.Separator))
}

// denied reports whether a host path is on or under the deny table.
func (d *Driver) denied(p string) bool {
	return slices.ContainsFunc(d.deny, func(entry string) bool { return under(p, entry) })
}

// system reports whether a path is one of the platform's system directories,
// or below one when below is set.
func (d *Driver) system(p string, below bool) bool {
	return slices.ContainsFunc(systemDirs[d.goos], func(dir string) bool {
		return p == dir || (below && under(p, dir))
	})
}

// checkGrant holds one grant to the host's rules: an absolute clean path to a
// directory the user owns, not the root, the home directory or a system
// directory, and never on or under the deny table, through a link or not.
func (d *Driver) checkGrant(g Grant) (Grant, error) {
	refuse := func(why string) (Grant, error) {
		return Grant{}, fmt.Errorf("%w: the grant %s %s", driver.ErrInvalid, g.Path, why)
	}
	if !filepath.IsAbs(g.Path) || filepath.Clean(g.Path) != g.Path {
		return refuse("is not a clean absolute path")
	}
	real, err := filepath.EvalSymlinks(g.Path)
	if err != nil {
		return refuse("does not exist")
	}
	info, err := os.Stat(real)
	if err != nil || !info.IsDir() {
		return refuse("is not a directory")
	}
	for _, p := range []string{g.Path, real} {
		switch {
		case p == "/":
			return refuse("is the root")
		case p == d.home:
			return refuse("is the home directory")
		case d.system(p, true):
			return refuse("is a system directory")
		case d.denied(p):
			return refuse("is on the deny table")
		}
	}
	if stat, ok := info.Sys().(*syscall.Stat_t); ok && int(stat.Uid) != os.Getuid() {
		return refuse("belongs to another user")
	}
	return Grant{Path: real, Write: g.Write}, nil
}

// checkRead holds one read path to the host's rules: an absolute path, not
// the home directory or a system directory, and never on or under the deny
// table.
func (d *Driver) checkRead(p string) (string, error) {
	if !filepath.IsAbs(p) {
		return "", fmt.Errorf("%w: the read path %s is not absolute", driver.ErrInvalid, p)
	}
	clean, real := filepath.Clean(p), resolved(filepath.Clean(p))
	for _, q := range []string{clean, real} {
		if q == d.home || q == "/" || d.system(q, false) || d.denied(q) {
			return "", fmt.Errorf("%w: the read path %s is the home directory, a system directory or on the deny table", driver.ErrInvalid, p)
		}
	}
	return real, nil
}

// checkNetwork reads the host's network: none when unset, or an allow list of
// hosts, lowercased.
func checkNetwork(n Network) (Network, error) {
	switch n.Mode {
	case "", NetworkNone:
		return Network{Mode: NetworkNone}, nil
	case NetworkAllowlist:
		out := Network{Mode: NetworkAllowlist}
		for _, h := range n.Hosts {
			if h = strings.ToLower(strings.TrimSpace(h)); h != "" && !slices.Contains(out.Hosts, h) {
				out.Hosts = append(out.Hosts, h)
			}
		}
		return out, nil
	}
	return Network{}, fmt.Errorf("%w: the network is none or allowlist, not %q", driver.ErrInvalid, n.Mode)
}

func (d *Driver) Name() string      { return Name }
func (d *Driver) Isolation() string { return v1.IsolationProcess }

// Capabilities declares what an operating system's sandbox around a host
// process provides: the two egress modes its allow-only proxy enforces, with
// no gateway; files; a stage that outlives this process; a granted host
// folder as the workspace; and stdin on an exec with no terminal. It declares
// no attach, dial, display, input, resize, pool, mesh, ingress, volume or
// snapshot.
func (d *Driver) Capabilities() driver.Capabilities {
	return driver.Capabilities{
		Egress: []v1.EgressMode{v1.EgressNone, v1.EgressAllowlist},
		Files:  true, Detach: true, HostWorkspace: true, EgressLocal: true, ExecStdin: true,
	}
}

// Host is the machine this driver runs on as a worker registers it.
func (d *Driver) Host() v1.HostInfo {
	host := v1.HostInfo{OS: d.goos, Arch: goruntime.GOARCH, Network: v1.HostNetwork{Mode: d.network.Mode, Hosts: slices.Clone(d.network.Hosts)}}
	for _, g := range d.grants {
		host.Grants = append(host.Grants, v1.HostGrant{Path: g.Path, Write: g.Write})
	}
	return host
}

// Preflight refuses a platform with no sandbox this driver drives, a process
// running as root, and a machine missing the sandbox runtime, whose refusal
// names what to install.
func (d *Driver) Preflight(ctx context.Context) error {
	if _, ok := systemDirs[d.goos]; !ok {
		return fmt.Errorf("%w: the local driver runs on macOS and Linux, not %s", driver.ErrUnsupported, d.goos)
	}
	if d.euid == 0 {
		return ErrRunsAsRoot
	}
	if err := d.sandbox.Preflight(ctx); err != nil {
		return fmt.Errorf("local: the sandbox runtime is not ready: %w", err)
	}
	return nil
}

// Ready reports whether the data directory is still there.
func (d *Driver) Ready(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	_, err := os.Stat(filepath.Join(d.data, "sandboxes"))
	return err
}

// Close ends every attached command, whose pipes this process holds. A
// detached stage is left running: it belongs to its sandbox, which a stop or
// a delete ends.
func (d *Driver) Close() error {
	d.mu.Lock()
	var all []*attachedExec
	for _, set := range d.attached {
		for e := range set {
			all = append(all, e)
		}
	}
	d.mu.Unlock()
	for _, e := range all {
		e.lose()
	}
	return nil
}
