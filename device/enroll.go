// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

//go:build darwin || linux

package device

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"net/http"
	"os"
	"os/exec"
	goruntime "runtime"
	"strings"
	"time"
	"unicode"

	"latere.ai/x/cella/client"
	v1 "latere.ai/x/cella/manifest/v1"
)

// Sandboxes is how many sandboxes a device's environment holds at once, one
// per session working on the computer. Cella requires an environment to
// declare a capacity, and placement admits against it.
const Sandboxes = 8

// codeNotOwned is the plane's reason, in Cella's forbidden detail, for an
// environment that belongs to another person.
const codeNotOwned = "device_not_owned"

// Platform is what the plane that hosts the person's account answers about
// their devices, which this open core does not know: the labels every
// environment of the person carries, whether a sign-in acts in the person's
// own context, the device's name and its removal in the plane's records, and
// how many devices one person may connect. The embedder implements it over
// the plane's API.
type Platform interface {
	// Labels are what a create of the person's environment carries so that
	// the plane admits it.
	Labels(ctx context.Context) (map[string]string, error)
	// Context names the organization the sign-in acts in, "" for the
	// person's own context. A device is personal.
	Context(ctx context.Context) (string, error)
	// Rename names the device in the plane's records.
	Rename(ctx context.Context, device, name string) error
	// Remove removes the device from the plane's records, which deletes its
	// environment and revokes its keys.
	Remove(ctx context.Context, device string) error
	// Devices answers how many devices the person has and how many one
	// person may connect, for a refusal at the limit; a limit of zero is
	// unknown.
	Devices(ctx context.Context) (have, limit int, err error)
}

// NewCellaAccount is the engine's Account over Cella's API as the person:
// the client's token source presents the person's sign-in. An environment
// Cella holds none of, or that the plane refuses as removed or as another
// person's, answers ErrRemoved.
func NewCellaAccount(cella *client.Client) Account { return cellaAccount{cella: cella} }

type cellaAccount struct{ cella *client.Client }

func (a cellaAccount) MintKey(ctx context.Context, env string, ttl time.Duration) (Key, error) {
	key, _, err := a.cella.MintEnvironmentKeyFor(ctx, env, ttl)
	if err != nil {
		return Key{}, removedOr(err)
	}
	return Key{Environment: env, Token: key.Token, JTI: key.JTI, ExpiresAt: key.Expires}, nil
}

func (a cellaAccount) RevokeKey(ctx context.Context, env, jti string) error {
	return a.cella.RevokeEnvironmentKey(ctx, env, jti)
}

func (a cellaAccount) Exists(ctx context.Context, env string) (bool, error) {
	_, _, err := a.cella.GetEnvironment(ctx, env)
	if err == nil {
		return true, nil
	}
	if errors.Is(removedOr(err), ErrRemoved) {
		return false, nil
	}
	return false, err
}

// removedOr marks a refusal that says the device is no longer the person's:
// Cella holds no such environment, or the plane refuses it as removed or as
// another person's.
func removedOr(err error) error {
	var ce *client.Error
	if !errors.As(err, &ce) {
		return err
	}
	switch reason := platformReason(ce.Detail); {
	case ce.Code == "not_found":
	case ce.Code == "forbidden" && (reason == CodeRemoved || reason == codeNotOwned):
	default:
		return err
	}
	return fmt.Errorf("%w: %w", ErrRemoved, err)
}

// EnrollmentOptions build an Enrollment.
type EnrollmentOptions struct {
	// Paths are the engine's own, for the host policy file and the home.
	Paths Paths
	// Cella reaches Cella's API as the person.
	Cella *client.Client
	// Platform is the plane's part of a connect.
	Platform Platform
	// Labels are what the environment carries beside the plane's own, such
	// as the plane's mark of a device's environment.
	Labels map[string]string
	// Name is the computer's own name, the default of a connect; nil is
	// DefaultComputerName.
	Name func() string
}

// Enrollment is the engine's Enroller over Cella: it fills a connect's
// terms, creates the computer's environment as the person and keeps the
// host policy file, and asks the Platform for what the plane decides.
type Enrollment struct {
	paths    Paths
	cella    *client.Client
	platform Platform
	labels   map[string]string
	name     func() string
}

// NewEnrollment builds an Enrollment.
func NewEnrollment(o EnrollmentOptions) *Enrollment {
	if o.Name == nil {
		o.Name = DefaultComputerName
	}
	return &Enrollment{paths: o.Paths, cella: o.Cella, platform: o.Platform, labels: maps.Clone(o.Labels), name: o.Name}
}

// Propose fills a connect's terms from the query, then the previous host
// policy file, then the defaults (ProposePolicy). A folder in the query of
// a client is absolute or under ~, so the engine's working directory
// resolves only a terminal's ".".
func (e *Enrollment) Propose(previous Policy, q ConnectQuery) (Policy, error) {
	cwd, err := os.Getwd()
	if err != nil {
		return Policy{}, err
	}
	return ProposePolicy(previous, q, e.paths.Home, cwd, e.name)
}

// Connect connects this computer as the person, refusing a sign-in in an
// organization's context: the host policy file, the environment created at
// Cella, the device in the file, and its name in the plane's records, whose
// refusal leaves the computer connected and is answered beside it. The key
// is minted when the engine serves.
func (e *Enrollment) Connect(ctx context.Context, p Policy) (Connected, error) {
	org, err := e.platform.Context(ctx)
	if err != nil {
		return Connected{}, credentialsOr(err, Fail(CodeRefused, "", err))
	}
	if org != "" {
		return Connected{}, Fail(CodeOrgContext, "the sign-in is in the context of the organization "+org, nil)
	}
	if err = SavePolicy(e.paths.PolicyFile(), p); err != nil {
		return Connected{}, Fail(CodePolicyInvalid, "write the host policy file", err)
	}
	labels, err := e.platform.Labels(ctx)
	if err != nil {
		return Connected{}, credentialsOr(err, Fail(CodeRefused, "", err))
	}
	labels = maps.Clone(labels)
	if labels == nil {
		labels = map[string]string{}
	}
	maps.Copy(labels, e.labels)
	id := "dev-" + strings.ToLower(rand.Text()[:16])
	body, err := json.Marshal(v1.Environment{
		APIVersion: v1.APIVersion, Kind: v1.KindEnvironment,
		Metadata: v1.Metadata{Name: id, Labels: labels},
		Spec: v1.EnvironmentSpec{
			Mode: v1.EnvironmentWorker, Isolation: v1.IsolationProcess, Workers: v1.WorkersSpec{Max: 1},
			Capacity: v1.Capacity{Sandboxes: Sandboxes},
		},
	})
	if err != nil {
		return Connected{}, Fail(CodeServeFailed, "encode the environment", err)
	}
	if _, _, err = e.cella.CreateEnvironment(ctx, client.JSON(body)); err != nil {
		return Connected{}, credentialsOr(err, e.createRefusal(ctx, err))
	}
	p.Device = id
	if err = SavePolicy(e.paths.PolicyFile(), p); err != nil {
		return Connected{}, Fail(CodePolicyInvalid, "write the device into the host policy file", err)
	}
	done := Connected{Policy: p}
	if err = e.platform.Rename(ctx, id, p.Name); err != nil {
		done.NameError = err
	}
	return done, nil
}

// Rename names the device in the plane's records.
func (e *Enrollment) Rename(ctx context.Context, env, name string) error {
	return e.platform.Rename(ctx, env, name)
}

// Remove removes the device from the plane's records.
func (e *Enrollment) Remove(ctx context.Context, env string) error {
	return e.platform.Remove(ctx, env)
}

// createRefusal answers an environment create the plane refused. Its reason
// travels in Cella's developer detail, and each reason the plane names gets
// its own refusal; the device limit is told with the limit the plane
// answers for the person.
func (e *Enrollment) createRefusal(ctx context.Context, err error) error {
	var ce *client.Error
	if !errors.As(err, &ce) {
		return Fail(CodeRefused, "", err)
	}
	switch reason := platformReason(ce.Detail); reason {
	case CodePersonalOnly, CodeSignInRequired, CodeKeyRefused, CodeInvalid:
		return Fail(reason, ce.Detail, err)
	case CodeLimit:
		refusal := Fail(CodeLimit, ce.Detail, err)
		if have, limit, derr := e.platform.Devices(ctx); derr == nil && limit > 0 {
			refusal.Hint = fmt.Sprintf("You have %d devices, and one person may connect %d. %s", have, limit, refusal.Hint)
		}
		return refusal
	}
	if ce.Status == http.StatusUnauthorized {
		return Fail(CodeSignInRequired, ce.Detail, err)
	}
	detail := ce.Detail
	if detail == "" {
		detail = ce.Error()
	}
	return Fail(CodeRefused, detail, err)
}

// platformReason is the plane's reason in a Cella refusal's detail, which
// reads "forbidden: environment.create: device_limit": the last word, when
// it is a code.
func platformReason(detail string) string {
	_, word, ok := strings.CutLast(detail, ": ")
	if !ok {
		return ""
	}
	word = strings.TrimSpace(word)
	for _, r := range word {
		if !unicode.IsLower(r) && r != '_' {
			return ""
		}
	}
	return word
}

// credentialsOr is the refusal for a step that waited for a client's token
// it was never handed, and otherwise the refusal given.
func credentialsOr(err, otherwise error) error {
	if errors.Is(err, ErrCredentialsNeeded) {
		return Fail(CodeCredentialsNeeded, "", err)
	}
	return otherwise
}

// ProposePolicy fills a connect's host policy: each value of the query, then
// what the previous file said, then the default. The name defaults to name()
// and is cleaned; folders resolve against cwd and home, "." when none is
// given and none was before; the network is an allowlist of DefaultHosts and
// sessions start without asking, unless the query or the file says
// otherwise. The paths read outside the folders and the remembered triggers
// the file kept stay.
func ProposePolicy(previous Policy, q ConnectQuery, home, cwd string, name func() string) (Policy, error) {
	p := Policy{
		Name: previous.Name, SessionStart: previous.SessionStart, Folders: previous.Folders, Network: previous.Network,
		ReadPaths: previous.ReadPaths, RememberedTriggers: previous.RememberedTriggers,
	}
	if q.SessionStart != "" || p.SessionStart == "" {
		p.SessionStart = orDefault(q.SessionStart, SessionStartAllow)
	}
	if q.Name != "" {
		p.Name = q.Name
	}
	if p.Name == "" && name != nil {
		p.Name = name()
	}
	p.Name = CleanName(p.Name)
	if len(q.Folders) > 0 || len(p.Folders) == 0 {
		values := q.Folders
		if len(values) == 0 {
			values = []string{"."}
		}
		p.Folders = nil
		for _, v := range values {
			folder, err := ParseFolder(v, cwd, home)
			if err != nil {
				return Policy{}, err
			}
			folder.Path = Shorten(home, folder.Path)
			p.Folders = append(p.Folders, folder)
		}
	}
	if q.Network != "" || p.Network.Mode == "" {
		p.Network.Mode = orDefault(q.Network, NetworkAllowlist)
	}
	switch {
	case p.Network.Mode == NetworkNone:
		p.Network.Hosts = nil
	case len(q.Hosts) > 0:
		p.Network.Hosts = q.Hosts
	case len(p.Network.Hosts) == 0:
		p.Network.Hosts = DefaultHosts
	}
	return p, nil
}

func orDefault(v, otherwise string) string {
	if v == "" {
		return otherwise
	}
	return v
}

// DefaultComputerName is the computer's own name: the one macOS shows in its
// sharing settings, or the host name without its local domain.
func DefaultComputerName() string {
	if goruntime.GOOS == "darwin" {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if out, err := exec.CommandContext(ctx, "/usr/sbin/scutil", "--get", "ComputerName").Output(); err == nil {
			if name := strings.TrimSpace(string(out)); name != "" {
				return name
			}
		}
	}
	host, err := os.Hostname()
	if err != nil || host == "" {
		return "This computer"
	}
	return strings.TrimSuffix(host, ".local")
}
