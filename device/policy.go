// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package device

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"
	"unicode"

	"go.yaml.in/yaml/v3"
	"latere.ai/x/pkg/atomicfile"
)

// The two networks a device allows. The local driver offers no open
// network: its sandbox runtime allows named hosts only.
const (
	NetworkAllowlist = "allowlist"
	NetworkNone      = "none"
)

// DefaultHosts seed the allowlist at connect: the git and package hosts a
// session's tools fetch from.
var DefaultHosts = []string{
	"github.com", "codeload.github.com", "registry.npmjs.org", "pypi.org",
	"files.pythonhosted.org", "proxy.golang.org", "sum.golang.org",
	"crates.io", "static.crates.io",
}

// The two answers to whether a session starts on the computer without the
// person being asked here first.
const (
	// SessionStartAllow starts a session the person started, and one a
	// scheduled trigger of theirs started, without asking; a trigger that
	// fires on delivered events still asks. It is the default.
	SessionStartAllow = "allow"
	// SessionStartAsk asks before every session starts.
	SessionStartAsk = "ask"
)

// RememberFor is how long a remembered scheduled trigger runs without
// asking.
const RememberFor = 30 * 24 * time.Hour

// Policy is the host policy file: the computer's own say over what its
// sessions may use. The platform never writes it, and the engine reads it at
// each start; session_start and the remembered triggers are read again at
// each create, so a change to either applies to the next session.
type Policy struct {
	// Device is the environment this computer serves, set once it is
	// connected.
	Device string `yaml:"device,omitempty" json:"device"`
	// Name is what the computer is called in its owner's devices.
	Name string `yaml:"name" json:"name"`
	// SessionStart is SessionStartAllow or SessionStartAsk. A file written
	// before the field existed holds none, which reads as allow.
	SessionStart       string       `yaml:"session_start" json:"session_start"`
	Folders            []Folder     `yaml:"folders" json:"folders"`
	Network            Network      `yaml:"network" json:"network"`
	ReadPaths          []string     `yaml:"read_paths" json:"read_paths"`
	RememberedTriggers []Remembered `yaml:"remembered_triggers" json:"remembered_triggers"`
}

// Folder is one granted folder. Path is absolute or starts with ~.
type Folder struct {
	Path  string `yaml:"path" json:"path"`
	Write bool   `yaml:"write" json:"write"`
}

// Network is the network sessions may reach: an allowlist of hosts, or
// none.
type Network struct {
	Mode  string   `yaml:"mode" json:"mode"`
	Hosts []string `yaml:"hosts,flow" json:"hosts"`
}

// Remembered is one scheduled trigger admitted without asking: by its id
// and the agent version it ran, until the expiry.
type Remembered struct {
	Trigger      string    `yaml:"trigger" json:"trigger"`
	AgentVersion string    `yaml:"agent_version" json:"agent_version"`
	Until        time.Time `yaml:"until" json:"until"`
}

// policyHeader opens the file, so a person who opens it knows whose it is.
const policyHeader = `# The host policy of this computer for latere devices (spec 016).
# The platform never writes this file. A change to folders, network or
# read_paths takes effect at the next 'latere devices connect'; one to
# session_start (allow or ask) or remembered_triggers at the next session.
# 'latere devices policy' prints and checks it.
`

// LoadPolicy reads the policy file at path, and reports false when there is
// none. A file that does not decode, or names a field this version does not
// know, is refused rather than read in part.
func LoadPolicy(path string) (Policy, bool, error) {
	raw, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return Policy{}, false, nil
	}
	if err != nil {
		return Policy{}, false, err
	}
	var p Policy
	dec := yaml.NewDecoder(bytes.NewReader(raw))
	dec.KnownFields(true)
	if err = dec.Decode(&p); err != nil && !errors.Is(err, io.EOF) {
		return Policy{}, true, fmt.Errorf("%s does not read as a host policy: %w", path, err)
	}
	return p, true, nil
}

// SavePolicy writes p to path, mode 0600, in a directory of mode 0700.
func SavePolicy(path string, p Policy) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	if p.Folders == nil {
		p.Folders = []Folder{}
	}
	if p.ReadPaths == nil {
		p.ReadPaths = []string{}
	}
	if p.RememberedTriggers == nil {
		p.RememberedTriggers = []Remembered{}
	}
	if p.Network.Hosts == nil {
		p.Network.Hosts = []string{}
	}
	if p.SessionStart == "" {
		// The value a file without the field reads as, written out.
		p.SessionStart = SessionStartAllow
	}
	var b bytes.Buffer
	b.WriteString(policyHeader)
	enc := yaml.NewEncoder(&b)
	enc.SetIndent(2)
	if err := enc.Encode(p); err != nil {
		return fmt.Errorf("encode the host policy: %w", err)
	}
	if err := enc.Close(); err != nil {
		return fmt.Errorf("encode the host policy: %w", err)
	}
	return atomicfile.WriteSync(path, b.Bytes(), 0o600)
}

// Expand resolves a path of the policy file against home: a leading ~ is
// the home directory, and anything else must be absolute.
func Expand(home, p string) (string, error) {
	switch {
	case p == "~":
		return home, nil
	case strings.HasPrefix(p, "~/"):
		return filepath.Join(home, p[2:]), nil
	case filepath.IsAbs(p):
		return filepath.Clean(p), nil
	}
	return "", fmt.Errorf("%q is neither absolute nor under ~", p)
}

// Shorten writes a path under home with a leading ~, as the file and the
// screens show it.
func Shorten(home, p string) string {
	if home == "" {
		return p
	}
	if p == home {
		return "~"
	}
	if rest, ok := strings.CutPrefix(p, home+string(filepath.Separator)); ok {
		return "~/" + rest
	}
	return p
}

// Problems lists what is wrong with the policy's own shape: a network that
// is not allowlist or none, a path neither absolute nor under ~, a
// remembered trigger missing its id, its version or its expiry. The folder
// rules themselves are the local driver's, which CheckFolders applies.
func (p Policy) Problems(home string) []string {
	var out []string
	switch p.SessionStart {
	case "", SessionStartAllow, SessionStartAsk:
	default:
		out = append(out, fmt.Sprintf("session_start is %q; it is allow or ask", p.SessionStart))
	}
	switch p.Network.Mode {
	case NetworkAllowlist, NetworkNone:
	default:
		out = append(out, fmt.Sprintf("network.mode is %q; it is allowlist or none", p.Network.Mode))
	}
	if len(p.Folders) == 0 {
		out = append(out, "folders is empty; a session works in one of the folders this computer grants")
	}
	for _, f := range p.Folders {
		if _, err := Expand(home, f.Path); err != nil {
			out = append(out, "folders: "+err.Error())
		}
	}
	for _, r := range p.ReadPaths {
		if _, err := Expand(home, r); err != nil {
			out = append(out, "read_paths: "+err.Error())
		}
	}
	for _, r := range p.RememberedTriggers {
		if r.Trigger == "" || r.AgentVersion == "" || r.Until.IsZero() {
			out = append(out, "remembered_triggers: each entry names a trigger, its agent_version and until")
			break
		}
	}
	return out
}

// StartsWithoutAsking reports whether a session starts on the computer with
// no one asked here: under session_start allow, a session a person started
// and one a trigger that fires on a schedule started. A trigger that fires
// on delivered events, mail or a webhook, always asks, since anyone with its
// address can send it input, and so does a trigger whose firing the create
// does not name.
func (p Policy) StartsWithoutAsking(a Approval) bool {
	if p.SessionStart == SessionStartAsk {
		return false
	}
	return a.Trigger == "" || a.FiresOn == FiresOnSchedule
}

// Remembers reports whether a scheduled trigger is admitted without asking:
// an entry names its id and the agent version it runs now, and has not
// expired.
func (p Policy) Remembers(trigger, version string, now time.Time) bool {
	if trigger == "" || version == "" {
		return false
	}
	return slices.ContainsFunc(p.RememberedTriggers, func(r Remembered) bool {
		return r.Trigger == trigger && r.AgentVersion == version && now.Before(r.Until)
	})
}

// Remember admits a scheduled trigger at an agent version for RememberFor,
// replacing any entry it had, and drops the entries that expired.
func (p *Policy) Remember(trigger, version string, now time.Time) {
	kept := p.RememberedTriggers[:0]
	for _, r := range p.RememberedTriggers {
		if r.Trigger != trigger && now.Before(r.Until) {
			kept = append(kept, r)
		}
	}
	p.RememberedTriggers = append(kept, Remembered{Trigger: trigger, AgentVersion: version, Until: now.Add(RememberFor).UTC()})
}

// Forget drops a trigger's entry, and reports whether there was one.
func (p *Policy) Forget(trigger string) bool {
	n := len(p.RememberedTriggers)
	p.RememberedTriggers = slices.DeleteFunc(p.RememberedTriggers, func(r Remembered) bool { return r.Trigger == trigger })
	return len(p.RememberedTriggers) != n
}

// ParseFolder reads one --folder value, PATH or PATH:ro, against the
// working directory and home: the path becomes absolute and clean, and the
// folder is read-write unless it ends in :ro.
func ParseFolder(value, cwd, home string) (Folder, error) {
	path, write := value, true
	if rest, ok := strings.CutSuffix(value, ":ro"); ok {
		path, write = rest, false
	} else if rest, ok := strings.CutSuffix(value, ":rw"); ok {
		path = rest
	}
	if path == "" {
		return Folder{}, fmt.Errorf("--folder %q names no folder", value)
	}
	switch {
	case path == "~" || strings.HasPrefix(path, "~/"):
		expanded, err := Expand(home, path)
		if err != nil {
			return Folder{}, err
		}
		path = expanded
	case !filepath.IsAbs(path):
		path = filepath.Join(cwd, path)
	}
	return Folder{Path: filepath.Clean(path), Write: write}, nil
}

// MaxNameLength is the longest name the platform keeps for a device.
const MaxNameLength = 64

// CleanName keeps a name the platform takes: printable characters, trimmed,
// at most MaxNameLength of them, and "This computer" for none.
func CleanName(name string) string {
	var b strings.Builder
	n := 0
	for _, r := range strings.TrimSpace(name) {
		if !unicode.IsPrint(r) {
			continue
		}
		if n == MaxNameLength {
			break
		}
		b.WriteRune(r)
		n++
	}
	if out := strings.TrimSpace(b.String()); out != "" {
		return out
	}
	return "This computer"
}

// Forget forgets on this computer the device id it was: its key goes, and
// so does the host policy file at policyFile when dropFile is set;
// otherwise the file keeps its folders and network and names no device, so
// the next connect proposes them again. A file that names another device is
// left as it is. With dropFile, a file that names no device goes too: the
// engine serving the device sees a removal at the platform first, and
// clears the device from the file before the caller that removed it gets
// here. `latere devices remove`, `latere logout` and the engine's removal
// through the control socket share it.
func Forget(keys KeyStore, policyFile, id string, dropFile bool) error {
	var errs []error
	if err := keys.Delete(id); err != nil {
		errs = append(errs, fmt.Errorf("delete the key: %w", err))
	}
	policy, ok, err := LoadPolicy(policyFile)
	switch {
	case err != nil:
		errs = append(errs, err)
	case !ok || (policy.Device != id && (!dropFile || policy.Device != "")):
	case dropFile:
		if err := os.Remove(policyFile); err != nil && !errors.Is(err, os.ErrNotExist) {
			errs = append(errs, fmt.Errorf("delete the host policy file: %w", err))
		}
	default:
		policy.Device = ""
		if err := SavePolicy(policyFile, policy); err != nil {
			errs = append(errs, fmt.Errorf("update the host policy file: %w", err))
		}
	}
	return errors.Join(errs...)
}
