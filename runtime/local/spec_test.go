// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package local

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"latere.ai/x/pkg/hostsandbox"

	"latere.ai/x/cella/manifest"
	v1 "latere.ai/x/cella/manifest/v1"
	driver "latere.ai/x/cella/runtime"
)

// run executes one command and returns its output and exit code.
func run(t *testing.T, d *Driver, id string, req driver.ExecRequest) (string, string, int, error) {
	t.Helper()
	e, err := d.Exec(context.Background(), id, req)
	if err != nil {
		return "", "", 0, err
	}
	defer func() { _ = e.Close() }()
	var out, errOut []byte
	done := make(chan struct{})
	go func() { errOut, _ = io.ReadAll(e.Stderr()); close(done) }()
	out, _ = io.ReadAll(e.Stdout())
	<-done
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	code, err := e.Wait(ctx)
	return string(out), string(errOut), code, err
}

// underSRT skips a case that needs the real sandbox runtime where none is
// installed, and returns a driver under it otherwise.
func underSRT(t *testing.T, h host, o Options) *Driver {
	t.Helper()
	// The stand-in a case installed for its other half is set aside: this
	// driver runs the real runtime.
	standIn := newSandbox
	newSandbox = hostsandbox.New
	d := h.open(t, o)
	newSandbox = standIn
	if err := d.Preflight(context.Background()); err != nil {
		t.Skipf("the sandbox runtime is not installed here, so the stage half of this case is unchecked:\n%v", err)
	}
	return d
}

// TestLocalDeleteLeavesTheHostFolder: files written, renamed and removed in a
// host folder through a command and through the file operations are as the
// sandbox last left them once it is deleted; the delete removes the scratch
// directory alone, and a link to the folder the sandbox left there is removed
// as a link.
func TestLocalDeleteLeavesTheHostFolder(t *testing.T) {
	shim(t)
	h := newHost(t)
	d := h.open(t, Options{Lookup: os.LookupEnv})
	for path, body := range map[string]string{"a.txt": "a", "b.txt": "b", "dir/c.txt": "c"} {
		full := filepath.Join(h.project, path)
		if err := os.MkdirAll(filepath.Dir(full), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := d.Create(context.Background(), driver.CreateSpec{ID: "sbx_host", Workspace: driver.Workspace{Path: h.project, Host: h.project}}); err != nil {
		t.Fatal(err)
	}
	if _, errOut, code, err := run(t, d, "sbx_host", driver.ExecRequest{Command: []string{"/bin/sh", "-c",
		`printf changed > a.txt && mv b.txt b2.txt && rm dir/c.txt && ln -s "$PWD" "$HOME/project-link"`}}); err != nil || code != 0 {
		t.Fatalf("the command failed with %d: %v %s", code, err, errOut)
	}
	if _, err := d.Write(context.Background(), "sbx_host", driver.WriteRequest{Path: filepath.Join(h.project, "new.txt"), Body: strings.NewReader("new")}); err != nil {
		t.Fatal(err)
	}
	if err := d.Mkdir(context.Background(), "sbx_host", filepath.Join(h.project, "sub")); err != nil {
		t.Fatal(err)
	}
	if err := d.Move(context.Background(), "sbx_host", filepath.Join(h.project, "new.txt"), filepath.Join(h.project, "sub", "new.txt")); err != nil {
		t.Fatal(err)
	}
	if err := d.Delete(context.Background(), "sbx_host"); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if _, err := os.Stat(d.dir("sbx_host")); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("the scratch directory outlived the sandbox: %v", err)
	}
	want := map[string]string{"a.txt": "changed", "b2.txt": "b", "notes.txt": "notes", "secret.txt": "withheld", "sub/new.txt": "new"}
	got := map[string]string{}
	err := filepath.WalkDir(h.project, func(p string, entry os.DirEntry, err error) error {
		if err != nil || entry.IsDir() {
			return err
		}
		body, err := os.ReadFile(p)
		rel, _ := filepath.Rel(h.project, p)
		got[filepath.ToSlash(rel)] = string(body)
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != len(want) {
		t.Errorf("the folder holds %v, want %v", got, want)
	}
	for name, body := range want {
		if got[name] != body {
			t.Errorf("%s reads %q, want %q", name, got[name], body)
		}
	}
	if info, err := os.Stat(filepath.Join(h.project, "dir")); err != nil || !info.IsDir() {
		t.Errorf("the emptied directory went with the sandbox: %v", err)
	}
}

// TestLocalGrantsNeverReachTheDenyTable: with a grant over a home subfolder,
// ~/.ssh/id_ed25519 and ~/.env are refused to the file operations by an
// absolute path, through .., and through a link placed in the grant, and so
// is a Deny entry inside the grant; under the sandbox runtime a command's cat
// of each is refused the same way.
func TestLocalGrantsNeverReachTheDenyTable(t *testing.T) {
	shim(t)
	h := newHost(t)
	d := h.open(t, Options{Deny: []string{"~/project/secret.txt"}, Lookup: os.LookupEnv})
	sandbox(t, d, driver.CreateSpec{ID: "sbx_deny", Workspace: driver.Workspace{Path: h.project, Host: h.project}})
	for _, link := range []string{"ssh-key", "env-file"} {
		target := filepath.Join(h.home, ".ssh", "id_ed25519")
		if link == "env-file" {
			target = filepath.Join(h.home, ".env")
		}
		if err := os.Symlink(target, filepath.Join(h.project, link)); err != nil {
			t.Fatal(err)
		}
	}
	paths := []string{
		filepath.Join(h.home, ".ssh", "id_ed25519"), filepath.Join(h.home, ".env"),
		h.project + "/../.ssh/id_ed25519", h.project + "/../.env",
		filepath.Join(h.project, "ssh-key"), filepath.Join(h.project, "env-file"),
		filepath.Join(h.project, "secret.txt"),
	}
	for _, p := range paths {
		if _, _, err := d.Open(context.Background(), "sbx_deny", p); !errors.Is(err, driver.ErrInvalid) {
			t.Errorf("the file operations read %s: %v", p, err)
		}
	}
	entries, err := d.ReadDir(context.Background(), "sbx_deny", h.project)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if e.Name == "secret.txt" {
			t.Error("the listing names the Deny entry")
		}
	}

	real := underSRT(t, h, Options{Deny: []string{"~/project/secret.txt"}, DataDir: tempDir(t)})
	sandbox(t, real, driver.CreateSpec{ID: "sbx_deny_srt", Workspace: driver.Workspace{Path: h.project, Host: h.project}})
	if out, _, code, err := run(t, real, "sbx_deny_srt", driver.ExecRequest{Command: []string{"cat", "notes.txt"}}); err != nil || code != 0 || out != "notes" {
		t.Fatalf("a granted file under the runtime read %q with %d: %v", out, code, err)
	}
	for _, p := range paths {
		if out, _, code, err := run(t, real, "sbx_deny_srt", driver.ExecRequest{Command: []string{"cat", p}}); err != nil || code == 0 {
			t.Errorf("a command read %s under the runtime: %q, %d, %v", p, out, code, err)
		}
	}
}

// TestLocalReadsOutsideGrantsAreRefused: a file in the home directory outside
// every grant is refused to the file operations, and under the sandbox
// runtime to a command, with ~/bin and ~/.local/bin on PATH, each of which is
// readable as itself and never as its parent.
func TestLocalReadsOutsideGrantsAreRefused(t *testing.T) {
	shim(t)
	h := newHost(t)
	bins := []string{filepath.Join(h.home, "bin"), filepath.Join(h.home, ".local", "bin")}
	for _, dir := range bins {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	path := strings.Join(append(slices.Clone(bins), h.home, "relative", "/usr/bin", "/bin"), string(os.PathListSeparator))
	lookup := func(name string) (string, bool) {
		if name == "PATH" {
			return path, true
		}
		return os.LookupEnv(name)
	}
	d := h.open(t, Options{Lookup: lookup})
	sandbox(t, d, driver.CreateSpec{ID: "sbx_outside"})
	outside := filepath.Join(h.home, "outside.txt")
	if _, _, err := d.Open(context.Background(), "sbx_outside", outside); !errors.Is(err, driver.ErrInvalid) {
		t.Errorf("the file operations read a file outside every grant: %v", err)
	}
	dirs := d.pathDirs()
	if !slices.Equal(dirs, append(slices.Clone(bins), "/usr/bin", "/bin")) {
		t.Errorf("the readable PATH directories are %v", dirs)
	}

	real := underSRT(t, h, Options{DataDir: tempDir(t)})
	sandbox(t, real, driver.CreateSpec{ID: "sbx_outside_srt"})
	if _, err := real.Write(context.Background(), "sbx_outside_srt", driver.WriteRequest{Path: driver.DefaultWorkdir + "/inside.txt", Body: strings.NewReader("inside")}); err != nil {
		t.Fatal(err)
	}
	if out, _, code, err := run(t, real, "sbx_outside_srt", driver.ExecRequest{Command: []string{"cat", "inside.txt"}}); err != nil || code != 0 || out != "inside" {
		t.Fatalf("a file in the workspace under the runtime read %q with %d: %v", out, code, err)
	}
	if out, _, code, err := run(t, real, "sbx_outside_srt", driver.ExecRequest{Command: []string{"cat", outside}}); err != nil || code == 0 {
		t.Errorf("a command read a file outside every grant: %q, %d, %v", out, code, err)
	}
}

// TestLocalDataDirInsideHome: with the data directory inside the home
// directory, as a program on a person's computer keeps it, a command that
// stats its own output (cat) runs under the runtime. The runtime denies the
// whole home but what a stage is granted, so the stage's own log file must
// be among its grants.
func TestLocalDataDirInsideHome(t *testing.T) {
	shim(t)
	h := newHost(t)
	data := filepath.Join(h.home, "Library", "Caches", "latere", "devices", "dev-test")
	if err := os.MkdirAll(data, 0o700); err != nil {
		t.Fatal(err)
	}
	real := underSRT(t, h, Options{DataDir: data})
	sandbox(t, real, driver.CreateSpec{ID: "sbx_home_data"})
	if _, err := real.Write(context.Background(), "sbx_home_data", driver.WriteRequest{Path: driver.DefaultWorkdir + "/inside.txt", Body: strings.NewReader("inside")}); err != nil {
		t.Fatal(err)
	}
	if out, _, code, err := run(t, real, "sbx_home_data", driver.ExecRequest{Command: []string{"cat", "inside.txt"}}); err != nil || code != 0 || out != "inside" {
		t.Fatalf("cat under a data directory inside home read %q with %d: %v", out, code, err)
	}
}

// localEnvironment is a worker environment served by the driver: its
// capabilities and its host as one live worker reports them.
func localEnvironment(d *Driver) v1.Environment {
	env := manifest.NativeEnvironment("laptop")
	env.Spec.Isolation = v1.IsolationProcess
	env.Status.Isolation = v1.IsolationProcess
	env.Status.Capabilities = d.Capabilities()
	env.Status.Hosts = []v1.HostStatus{{Worker: "wrk_laptop", Host: d.Host()}}
	return env
}

// resolveOn resolves one manifest against the driver's environment.
func resolveOn(t *testing.T, d *Driver, spec v1.SandboxSpec) (*manifest.Resolved, error) {
	t.Helper()
	spec.Environment = "laptop"
	sb := v1.Sandbox{APIVersion: v1.APIVersion, Kind: v1.KindSandbox, Metadata: v1.Metadata{Name: "on-laptop"}, Spec: spec}
	return manifest.Resolve(context.Background(), &sb, manifest.Options{Lookup: manifest.FixedEnvironment(localEnvironment(d))})
}

// TestLocalWorkspaceNotGranted: resolve refuses a workspace.host outside the
// grants the environment reports with workspace_not_granted, and the driver
// refuses it at Create against its own grants when the control plane's view
// is stale.
func TestLocalWorkspaceNotGranted(t *testing.T) {
	shim(t)
	h := newHost(t)
	d := h.open(t, Options{Lookup: os.LookupEnv})
	_, err := resolveOn(t, d, v1.SandboxSpec{Workspace: v1.Workspace{Host: filepath.Join(h.home, "elsewhere")}})
	var refused *manifest.Error
	if !errors.As(err, &refused) || refused.Code != "workspace_not_granted" {
		t.Errorf("resolve answered %v, want workspace_not_granted", err)
	}
	ok, err := resolveOn(t, d, v1.SandboxSpec{Workspace: v1.Workspace{Host: filepath.Join(h.project)}})
	if err != nil || ok.Sandbox.Spec.Workspace.Path != h.project || ok.Sandbox.Spec.Image != "" {
		t.Errorf("a granted folder resolved to %+v, %v", ok, err)
	}
	// The control plane's view says the folder is granted; the host's own
	// grants say it is not.
	stale := filepath.Join(h.home, "revoked")
	if err := os.MkdirAll(stale, 0o700); err != nil {
		t.Fatal(err)
	}
	for _, folder := range []string{stale, h.project + "/../outside.txt", filepath.Join(h.home, ".ssh")} {
		_, err = d.Create(context.Background(), driver.CreateSpec{ID: "sbx_stale", Workspace: driver.Workspace{Path: folder, Host: folder}})
		if !errors.Is(err, driver.ErrWorkspaceNotGranted) {
			t.Errorf("the driver created on %s: %v", folder, err)
		}
	}
}

// TestLocalExecWithStdin: stdin reaches an attached command and its exit code
// returns, its two outputs apart; a timeout ends it; and the driver's process
// ending, which Close stands for, ends a running one with exit code -1 and
// ErrLost.
func TestLocalExecWithStdin(t *testing.T) {
	shim(t)
	h := newHost(t)
	d := h.open(t, Options{Lookup: os.LookupEnv})
	sandbox(t, d, driver.CreateSpec{ID: "sbx_stdin"})
	out, errOut, code, err := run(t, d, "sbx_stdin", driver.ExecRequest{
		Command: []string{"/bin/sh", "-c", `read line; printf 'got:%s' "$line"; printf onstderr >&2; exit 5`},
		Stdin:   strings.NewReader("hello\n"),
	})
	if err != nil || code != 5 || out != "got:hello" || errOut != "onstderr" {
		t.Fatalf("the attached command answered %q %q %d %v", out, errOut, code, err)
	}
	_, _, _, err = run(t, d, "sbx_stdin", driver.ExecRequest{Command: []string{"sleep", "30"}, Stdin: strings.NewReader(""), Timeout: 100 * time.Millisecond})
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("an attached command past its timeout answered %v", err)
	}

	e, err := d.Exec(context.Background(), "sbx_stdin", driver.ExecRequest{Command: []string{"sleep", "30"}, Stdin: strings.NewReader("")})
	if err != nil {
		t.Fatal(err)
	}
	if err = d.Close(); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if code, err := e.Wait(ctx); code != -1 || !errors.Is(err, ErrLost) {
		t.Errorf("a command lost with the driver answered %d, %v", code, err)
	}
	_ = e.Close()
}

// TestLocalNetworkTable is spec 081's network table, one case per cell, at
// resolve, which narrows with a warning and never refuses, and at the driver,
// which narrows again against its own network.
func TestLocalNetworkTable(t *testing.T) {
	shim(t)
	h := newHost(t)
	list := Network{Mode: NetworkAllowlist, Hosts: []string{"github.com", "proxy.golang.org"}}
	allow := v1.Egress{Mode: v1.EgressAllowlist, AllowedHosts: []string{"github.com", "example.com"}}
	for _, c := range []struct {
		name  string
		host  Network
		asked v1.Egress
		want  v1.Egress
	}{
		{"none host, none asked", Network{Mode: NetworkNone}, v1.Egress{Mode: v1.EgressNone}, v1.Egress{Mode: v1.EgressNone}},
		{"none host, allowlist asked", Network{Mode: NetworkNone}, allow, v1.Egress{Mode: v1.EgressNone}},
		{"none host, open asked", Network{Mode: NetworkNone}, v1.Egress{Mode: v1.EgressOpen}, v1.Egress{Mode: v1.EgressNone}},
		{"allowlist host, none asked", list, v1.Egress{Mode: v1.EgressNone}, v1.Egress{Mode: v1.EgressNone}},
		{"allowlist host, allowlist asked", list, allow, v1.Egress{Mode: v1.EgressAllowlist, AllowedHosts: []string{"github.com"}}},
		{"allowlist host, open asked", list, v1.Egress{Mode: v1.EgressOpen}, v1.Egress{Mode: v1.EgressAllowlist, AllowedHosts: list.Hosts}},
	} {
		t.Run(c.name, func(t *testing.T) {
			d := h.open(t, Options{Network: c.host, Lookup: os.LookupEnv})
			resolved, err := resolveOn(t, d, v1.SandboxSpec{Network: v1.Network{Egress: c.asked}})
			if err != nil {
				t.Fatalf("resolve refused %+v: %v", c.asked, err)
			}
			if got := resolved.Sandbox.Spec.Network.Egress; got.Mode != c.want.Mode || !slices.Equal(got.AllowedHosts, c.want.AllowedHosts) {
				t.Errorf("resolve gave %+v, want %+v", got, c.want)
			}
			narrowed := c.asked.Mode != c.want.Mode || !slices.Equal(c.asked.AllowedHosts, c.want.AllowedHosts)
			if narrowed != (len(resolved.Warnings) == 1 && strings.HasPrefix(resolved.Warnings[0], manifest.WarningEgressNarrowed)) {
				t.Errorf("resolve warned %v for a narrowing of %v", resolved.Warnings, narrowed)
			}
			// The driver narrows what reaches it whatever resolve did.
			id := "sbx_net"
			sandbox(t, d, driver.CreateSpec{ID: id, Egress: driver.Egress{Mode: string(c.asked.Mode), AllowedHosts: c.asked.AllowedHosts}})
			spec := d.stage(mustLoad(t, d, id), "probe", []string{"true"}, nil, "/")
			wantMode := "none"
			if c.want.Mode == v1.EgressAllowlist {
				wantMode = "allowlist"
			}
			if spec.Network.Mode != wantMode || !slices.Equal(spec.Network.Domains, c.want.AllowedHosts) {
				t.Errorf("the driver renders %+v, want %s %v", spec.Network, wantMode, c.want.AllowedHosts)
			}
		})
	}
}
