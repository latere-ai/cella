// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package device

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"
)

func TestPolicyFileRoundTrip(t *testing.T) {
	path := filepath.Join(t.TempDir(), "conf", "device.yaml")
	until := time.Date(2026, 11, 17, 9, 0, 0, 0, time.UTC)
	want := Policy{
		Device: "dev-abcdefghijklmnop", Name: "MacBook Pro",
		Folders:            []Folder{{Path: "~/code/site", Write: true}, {Path: "~/notes"}},
		Network:            Network{Mode: NetworkAllowlist, Hosts: DefaultHosts},
		ReadPaths:          []string{"~/.gitconfig"},
		RememberedTriggers: []Remembered{{Trigger: "trg_1", AgentVersion: "4", Until: until}},
	}
	if err := SavePolicy(path, want); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Errorf("the policy file is %v, want 0600", info.Mode().Perm())
	}
	if dir, _ := os.Stat(filepath.Dir(path)); dir.Mode().Perm() != 0o700 {
		t.Errorf("the policy file's directory is %v, want 0700", dir.Mode().Perm())
	}
	raw, _ := os.ReadFile(path)
	if !strings.HasPrefix(string(raw), "# The host policy of this computer") {
		t.Errorf("the file does not open with its header:\n%s", raw)
	}
	got, ok, err := LoadPolicy(path)
	if err != nil || !ok {
		t.Fatalf("LoadPolicy = %v, %v", ok, err)
	}
	if got.Device != want.Device || got.Name != want.Name || !slices.Equal(got.Folders, want.Folders) ||
		got.Network.Mode != want.Network.Mode || !slices.Equal(got.Network.Hosts, want.Network.Hosts) ||
		!slices.Equal(got.ReadPaths, want.ReadPaths) || len(got.RememberedTriggers) != 1 ||
		!got.RememberedTriggers[0].Until.Equal(until) {
		t.Errorf("the policy read back as %+v", got)
	}
}

func TestPolicyFileMissingAndMalformed(t *testing.T) {
	dir := t.TempDir()
	if _, ok, err := LoadPolicy(filepath.Join(dir, "none.yaml")); ok || err != nil {
		t.Errorf("a missing file reads as %v, %v", ok, err)
	}
	typo := filepath.Join(dir, "typo.yaml")
	if err := os.WriteFile(typo, []byte("folder:\n  - path: ~/x\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, _, err := LoadPolicy(typo); err == nil || !strings.Contains(err.Error(), "folder") {
		t.Errorf("a file naming an unknown field reads with %v", err)
	}
	empty := filepath.Join(dir, "empty.yaml")
	if err := os.WriteFile(empty, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if p, ok, err := LoadPolicy(empty); !ok || err != nil || p.Device != "" {
		t.Errorf("an empty file reads as %+v, %v, %v", p, ok, err)
	}
	unreadable := filepath.Join(dir, "dir.yaml")
	if err := os.Mkdir(unreadable, 0o700); err != nil {
		t.Fatal(err)
	}
	if _, _, err := LoadPolicy(unreadable); err == nil {
		t.Error("a directory reads as a policy")
	}
}

func TestPolicyProblems(t *testing.T) {
	good := Policy{Folders: []Folder{{Path: "~/a"}}, Network: Network{Mode: NetworkNone}}
	if p := good.Problems("/home/a"); len(p) != 0 {
		t.Errorf("a good policy has problems %v", p)
	}
	bad := Policy{
		SessionStart: "sometimes",
		Folders:      []Folder{{Path: "relative"}}, Network: Network{Mode: "open"},
		ReadPaths:          []string{"also/relative"},
		RememberedTriggers: []Remembered{{Trigger: "trg_1"}},
	}
	problems := strings.Join(bad.Problems("/home/a"), "\n")
	for _, want := range []string{`session_start is "sometimes"`, "network.mode", "folders:", "read_paths:", "remembered_triggers"} {
		if !strings.Contains(problems, want) {
			t.Errorf("the problems %q do not name %s", problems, want)
		}
	}
	if p := (Policy{Network: Network{Mode: NetworkNone}}).Problems("/h"); len(p) != 1 || !strings.Contains(p[0], "folders is empty") {
		t.Errorf("a policy with no folders reads %v", p)
	}
}

// A file without session_start, as every file written before it existed,
// reads as allow, and a saved file says so; an explicit ask is kept.
func TestSessionStartInTheFile(t *testing.T) {
	dir := t.TempDir()
	older := filepath.Join(dir, "older.yaml")
	if err := os.WriteFile(older, []byte("device: dev-abcdefghijklmnop\nname: Mac\nfolders:\n  - path: ~/a\n    write: true\nnetwork:\n  mode: none\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	p, ok, err := LoadPolicy(older)
	if err != nil || !ok || p.SessionStart != "" || len(p.Problems("/h")) != 0 {
		t.Fatalf("a file without session_start reads %+v, %v, %v, problems %v", p, ok, err, p.Problems("/h"))
	}
	if !p.StartsWithoutAsking(Approval{}) {
		t.Error("a file without session_start asks before a session a person started")
	}
	if err = SavePolicy(older, p); err != nil {
		t.Fatal(err)
	}
	if raw, _ := os.ReadFile(older); !strings.Contains(string(raw), "\nsession_start: allow\n") {
		t.Errorf("a saved file does not write session_start allow:\n%s", raw)
	}
	p.SessionStart = SessionStartAsk
	if err = SavePolicy(older, p); err != nil {
		t.Fatal(err)
	}
	if got, _, err := LoadPolicy(older); err != nil || got.SessionStart != SessionStartAsk {
		t.Errorf("session_start ask read back as %q, %v", got.SessionStart, err)
	}
}

func TestStartsWithoutAsking(t *testing.T) {
	person := Approval{}
	scheduled := Approval{Trigger: "trg_1", FiresOn: FiresOnSchedule}
	events := Approval{Trigger: "trg_2", FiresOn: FiresOnEvents}
	unnamed := Approval{Trigger: "trg_3"}
	for _, mode := range []string{"", SessionStartAllow} {
		p := Policy{SessionStart: mode}
		if !p.StartsWithoutAsking(person) || !p.StartsWithoutAsking(scheduled) {
			t.Errorf("session_start %q asks before a person's session or a scheduled trigger", mode)
		}
		if p.StartsWithoutAsking(events) || p.StartsWithoutAsking(unnamed) {
			t.Errorf("session_start %q starts a trigger on delivered events, or one whose firing is unnamed, without asking", mode)
		}
	}
	ask := Policy{SessionStart: SessionStartAsk}
	for _, a := range []Approval{person, scheduled, events, unnamed} {
		if ask.StartsWithoutAsking(a) {
			t.Errorf("session_start ask starts %+v without asking", a)
		}
	}
}

func TestRememberedTriggers(t *testing.T) {
	now := time.Date(2026, 10, 18, 9, 0, 0, 0, time.UTC)
	var p Policy
	if p.Remembers("trg_1", "4", now) {
		t.Fatal("an empty policy remembers a trigger")
	}
	p.Remember("trg_1", "4", now)
	if !p.Remembers("trg_1", "4", now.Add(RememberFor-time.Minute)) {
		t.Error("a remembered trigger is asked within its 30 days")
	}
	if p.Remembers("trg_1", "5", now) {
		t.Error("a remembered trigger runs a changed agent without asking")
	}
	if p.Remembers("trg_1", "4", now.Add(RememberFor)) {
		t.Error("a remembered trigger runs after its 30 days")
	}
	if p.Remembers("trg_1", "", now) || p.Remembers("", "4", now) {
		t.Error("an entry is matched without a trigger or a version")
	}
	p.Remember("trg_2", "1", now)
	p.Remember("trg_1", "5", now)
	if len(p.RememberedTriggers) != 2 || !p.Remembers("trg_1", "5", now) || p.Remembers("trg_1", "4", now) {
		t.Errorf("remembering again does not replace the entry: %+v", p.RememberedTriggers)
	}
	p.Remember("trg_3", "1", now.Add(RememberFor+time.Hour))
	if len(p.RememberedTriggers) != 1 {
		t.Errorf("expired entries are kept: %+v", p.RememberedTriggers)
	}
	if !p.Forget("trg_3") || p.Forget("trg_3") || len(p.RememberedTriggers) != 0 {
		t.Errorf("forget left %+v", p.RememberedTriggers)
	}
}

func TestParseFolder(t *testing.T) {
	for _, tc := range []struct {
		value string
		want  Folder
	}{
		{"site", Folder{Path: "/work/site", Write: true}},
		{"/abs/notes:ro", Folder{Path: "/abs/notes"}},
		{"~/code:rw", Folder{Path: "/home/a/code", Write: true}},
		{"~", Folder{Path: "/home/a", Write: true}},
		{"../up/./x", Folder{Path: "/up/x", Write: true}},
	} {
		got, err := ParseFolder(tc.value, "/work", "/home/a")
		if err != nil || got != tc.want {
			t.Errorf("ParseFolder(%q) = %+v, %v; want %+v", tc.value, got, err, tc.want)
		}
	}
	if _, err := ParseFolder(":ro", "/work", "/home/a"); err == nil {
		t.Error("an empty folder is accepted")
	}
}

func TestExpandAndShorten(t *testing.T) {
	if p, err := Expand("/h", "~"); err != nil || p != "/h" {
		t.Errorf("Expand(~) = %q, %v", p, err)
	}
	if p, err := Expand("/h", "~/a/../b"); err != nil || p != "/h/b" {
		t.Errorf("Expand(~/a/../b) = %q, %v", p, err)
	}
	if p, err := Expand("/h", "/x/./y"); err != nil || p != "/x/y" {
		t.Errorf("Expand(/x/./y) = %q, %v", p, err)
	}
	if _, err := Expand("/h", "rel"); err == nil {
		t.Error("a relative path expands")
	}
	for in, want := range map[string]string{"/h": "~", "/h/a": "~/a", "/hx/a": "/hx/a", "/o": "/o"} {
		if got := Shorten("/h", in); got != want {
			t.Errorf("Shorten(%q) = %q, want %q", in, got, want)
		}
	}
	if got := Shorten("", "/h/a"); got != "/h/a" {
		t.Errorf("Shorten with no home = %q", got)
	}
}

func TestPathsAndDenyList(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(home, "cfg"))
	t.Setenv("XDG_STATE_HOME", filepath.Join(home, "st"))
	t.Setenv("XDG_CACHE_HOME", filepath.Join(home, "ca"))
	t.Setenv("LATERE_AUTH_TOKEN_FILE", filepath.Join(home, "elsewhere", "token.json"))
	p, err := DefaultPaths()
	if err != nil {
		t.Fatal(err)
	}
	want := Paths{
		Home: home, Config: filepath.Join(home, "cfg", "latere"), Token: filepath.Join(home, "elsewhere", "token.json"),
		State: filepath.Join(home, "st", "latere"), Cache: filepath.Join(home, "ca", "latere"),
	}
	if p != want {
		t.Fatalf("DefaultPaths = %+v, want %+v", p, want)
	}
	if p.PolicyFile() != filepath.Join(want.Config, "device.yaml") || p.KeyFile() != filepath.Join(want.Config, "device-keys.json") ||
		p.Socket() != filepath.Join(want.State, "devices", "control.sock") || p.LockFile() != filepath.Join(want.State, "devices", "lock") ||
		p.ActivityDir("dev-x") != filepath.Join(want.State, "devices", "dev-x") || p.DataDir("dev-x") != filepath.Join(want.Cache, "devices", "dev-x") {
		t.Errorf("the derived paths are wrong: %+v", p)
	}
	for goos, extra := range map[string][]string{
		"darwin": {filepath.Join(home, "Library", "Keychains"), filepath.Join(home, "Library", "Safari"),
			filepath.Join(home, "Library", "Application Support", "Google", "Chrome"),
			filepath.Join(home, "Library", "Application Support", "Firefox")},
		"linux": {filepath.Join(home, ".local", "share", "keyrings"), filepath.Join(home, ".mozilla"),
			filepath.Join(home, ".config", "google-chrome"), filepath.Join(home, ".config", "BraveSoftware")},
	} {
		deny := p.Deny(goos)
		for _, path := range append([]string{want.Config, want.State, want.Token}, extra...) {
			if !slices.Contains(deny, path) {
				t.Errorf("%s: the deny list %v lacks %s", goos, deny, path)
			}
		}
		if slices.Contains(deny, want.Cache) {
			t.Errorf("%s: the cache directory, where the sandboxes run, is denied", goos)
		}
	}
	t.Setenv("XDG_STATE_HOME", "")
	t.Setenv("XDG_CACHE_HOME", "")
	t.Setenv("LATERE_AUTH_TOKEN_FILE", "")
	p, err = DefaultPaths()
	if err != nil {
		t.Fatal(err)
	}
	if p.State != filepath.Join(home, ".local", "state", "latere") {
		t.Errorf("the default state directory is %s", p.State)
	}
	if cache, _ := os.UserCacheDir(); p.Cache != filepath.Join(cache, "latere") {
		t.Errorf("the default cache directory is %s", p.Cache)
	}
	if slices.Contains(p.Deny("linux"), p.Token) && filepath.Dir(p.Token) == p.Config {
		t.Error("a token inside the configuration directory is listed twice")
	}
}
