// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

//go:build darwin || linux

package device

import (
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	cellaruntime "latere.ai/x/cella/runtime"
	"latere.ai/x/cella/runtime/local"
)

// The exported helpers an embedder calls around the engine: the words of a
// started session, the owner record a terminal connect writes, forgetting a
// device here, checking the driver before a connect, the shared paths, and
// the defaults of an Enrollment.

func TestStartedWords(t *testing.T) {
	for _, tc := range []struct {
		a    Approval
		want string
	}{
		{Approval{}, "started by a person"},
		{Approval{Trigger: "trg_1", FiresOn: FiresOnSchedule}, "started by trigger trg_1 on a schedule"},
		{Approval{Trigger: "trg_2", FiresOn: FiresOnEvents}, "started by trigger trg_2 on delivered events"},
		{Approval{Trigger: "trg_3"}, "started by trigger trg_3"},
	} {
		if got := tc.a.StartedWords(); got != tc.want {
			t.Errorf("StartedWords(%+v) = %q, want %q", tc.a, got, tc.want)
		}
	}
}

func TestRecordOwner(t *testing.T) {
	paths := testPaths(t)
	if err := RecordOwner(paths, testEnv, ""); err != nil {
		t.Fatalf("RecordOwner with no subject: %v", err)
	}
	if _, err := os.Stat(paths.OwnerFile()); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("no subject wrote an owner record: %v", err)
	}
	if err := RecordOwner(paths, testEnv, "alice"); err != nil {
		t.Fatal(err)
	}
	if owner, err := readOwner(paths.OwnerFile(), testEnv); err != nil || owner != "alice" {
		t.Errorf("the owner read back is %q, %v", owner, err)
	}
	if owner, err := readOwner(paths.OwnerFile(), "dev-other"); err != nil || owner != "" {
		t.Errorf("another device read an owner %q, %v", owner, err)
	}
}

func TestForget(t *testing.T) {
	setup := func(t *testing.T) (Paths, *Keys) {
		t.Helper()
		paths := testPaths(t)
		keys := OpenKeys(paths.KeyFile())
		connected(t, paths, keys)
		return paths, keys
	}
	t.Run("keeps the file without its device", func(t *testing.T) {
		paths, keys := setup(t)
		if err := Forget(keys, paths.PolicyFile(), testEnv, false); err != nil {
			t.Fatal(err)
		}
		p, ok, err := LoadPolicy(paths.PolicyFile())
		if err != nil || !ok || p.Device != "" || len(p.Folders) == 0 {
			t.Errorf("the file reads %+v, %v, %v; want its folders and no device", p, ok, err)
		}
		if _, held, _ := keys.Get(testEnv); held {
			t.Error("the key outlived Forget")
		}
	})
	t.Run("drops the file", func(t *testing.T) {
		paths, keys := setup(t)
		if err := Forget(keys, paths.PolicyFile(), testEnv, true); err != nil {
			t.Fatal(err)
		}
		if _, ok, _ := LoadPolicy(paths.PolicyFile()); ok {
			t.Error("the file outlived Forget with dropFile")
		}
	})
	t.Run("leaves another device's file alone", func(t *testing.T) {
		paths, keys := setup(t)
		if err := Forget(keys, paths.PolicyFile(), "dev-other", true); err != nil {
			t.Fatal(err)
		}
		if p, ok, _ := LoadPolicy(paths.PolicyFile()); !ok || p.Device != testEnv {
			t.Errorf("another device's Forget changed the file to %+v", p)
		}
	})
}

func TestCheckDriver(t *testing.T) {
	paths := testPaths(t)
	project := filepath.Join(paths.Home, "project")
	if err := os.MkdirAll(project, 0o700); err != nil {
		t.Fatal(err)
	}
	p := Policy{Name: "Desk", Folders: []Folder{{Path: project, Write: true}}, Network: Network{Mode: NetworkNone}}
	var seen local.Options
	built := func(o local.Options) (cellaruntime.Driver, error) {
		seen = o
		return nil, errors.New("no sandbox runtime here")
	}
	if err := CheckDriver(p, paths, runtime.GOOS, built); err == nil || !strings.Contains(err.Error(), "no sandbox runtime") {
		t.Fatalf("CheckDriver = %v, want the build's refusal", err)
	}
	if seen.DataDir == "" || !strings.HasPrefix(seen.DataDir, paths.Cache) {
		t.Errorf("the check built the driver over %q, want a scratch directory in the cache", seen.DataDir)
	}
	if _, err := os.Stat(seen.DataDir); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("the check's scratch directory outlived it: %v", err)
	}
	if err := CheckDriver(p, paths, runtime.GOOS, func(local.Options) (cellaruntime.Driver, error) { return nil, nil }); err != nil {
		t.Errorf("CheckDriver with a driver that builds = %v", err)
	}
}

func TestDefaultPathsFollowTheEnvironment(t *testing.T) {
	root := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(root, "config"))
	t.Setenv("XDG_STATE_HOME", filepath.Join(root, "state"))
	t.Setenv("XDG_CACHE_HOME", filepath.Join(root, "cache"))
	t.Setenv(EnvAuthTokenFile, filepath.Join(root, "token.json"))
	t.Setenv(EnvKeysFile, filepath.Join(root, "keys.json"))
	p, err := DefaultPaths()
	if err != nil {
		t.Fatal(err)
	}
	want := Paths{
		Home: p.Home, Config: filepath.Join(root, "config", "latere"), Token: filepath.Join(root, "token.json"),
		State: filepath.Join(root, "state", "latere"), Cache: filepath.Join(root, "cache", "latere"), KeysFile: filepath.Join(root, "keys.json"),
	}
	if p != want {
		t.Errorf("DefaultPaths = %+v, want %+v", p, want)
	}
	keys := p.Keys()
	if err := keys.Put(Key{Environment: testEnv, Token: "token", JTI: "key_1"}); err != nil {
		t.Fatal(err)
	}
	if !keys.InFile() || keys.Name() != p.KeysFile {
		t.Errorf("a keys file's key went to %q, want the file alone", keys.Name())
	}
	if p.KeyFile() != filepath.Join(root, "keys.json") {
		t.Errorf("KeyFile = %q, want the keys file", p.KeyFile())
	}

	t.Setenv(EnvAuthTokenFile, "")
	t.Setenv("XDG_CONFIG_HOME", "")
	defaults, err := DefaultPaths()
	if err != nil {
		t.Fatal(err)
	}
	if defaults.Config != filepath.Join(defaults.Home, ".config", "latere") || defaults.Token != filepath.Join(defaults.Config, "auth-token.json") {
		t.Errorf("the default config is %q and token %q", defaults.Config, defaults.Token)
	}
}

func TestAnEngineWithoutAnEnrollerRefusesToConnect(t *testing.T) {
	paths := testPaths(t)
	e := testEngine(Config{Paths: paths, Keys: OpenKeys(paths.KeyFile()), Account: &fakeAccount{}})
	if _, err := e.ProposeConnect(ConnectQuery{}); CodeOf(err) != CodeServeFailed {
		t.Errorf("ProposeConnect = %v, want %s", err, CodeServeFailed)
	}
	if _, err := e.Connect(t.Context(), Terms{}, ""); CodeOf(err) != CodeServeFailed {
		t.Errorf("Connect = %v, want %s", err, CodeServeFailed)
	}
}

func TestAnEnrollmentNamesTheComputerByDefault(t *testing.T) {
	paths := testPaths(t)
	e := NewEnrollment(EnrollmentOptions{Paths: paths, Cella: (&stubCella{}).client(t), Platform: &fakePlatform{}})
	p, err := e.Propose(Policy{}, ConnectQuery{Folders: []string{paths.Home}})
	if err != nil {
		t.Fatal(err)
	}
	if p.Name != CleanName(DefaultComputerName()) {
		t.Errorf("the default name is %q, want the computer's own, %q", p.Name, DefaultComputerName())
	}
}

func TestDefaultComputerNameFallsBackToTheHost(t *testing.T) {
	if runtime.GOOS == "darwin" {
		t.Skip("macOS answers its sharing name first")
	}
	host, err := os.Hostname()
	if err != nil {
		t.Skip(err)
	}
	if got := DefaultComputerName(); got != strings.TrimSuffix(host, ".local") {
		t.Errorf("DefaultComputerName = %q, want the host name %q", got, host)
	}
}
