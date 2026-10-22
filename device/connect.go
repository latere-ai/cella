// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

//go:build darwin || linux

package device

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"latere.ai/x/cella/runtime/local"
)

// What a desktop client does through the control socket that the terminal
// does with `latere devices connect`, `latere devices policy` and `latere
// devices remove` (spec 018): connect this computer with the consent the
// client showed, hand the engine its tokens, change the host policy, and
// remove the device. Each goes through the same function the command does.

// Enroller is what the engine asks of the command for the steps that reach
// the platform as the person: the proposal's defaults, the connect after the
// person consented, a new name, and the removal.
type Enroller interface {
	// Propose fills a connect's terms from the query, then what the previous
	// host policy file said, then the defaults, as `connect` fills its
	// flags.
	Propose(previous Policy, q ConnectQuery) (Policy, error)
	// Connect does what `connect` does after its typed yes: the host policy
	// file, the environment at Cella and its name at the platform. It
	// answers the policy with its device, and why the name was not set when
	// the platform refused it, which leaves the computer connected.
	Connect(ctx context.Context, p Policy) (Connected, error)
	// Rename sets the device's name at the platform.
	Rename(ctx context.Context, env, name string) error
	// Remove removes the device at the platform, which deletes its
	// environment at Cella and revokes its keys.
	Remove(ctx context.Context, env string) error
}

// Connected is a connect that went through: the policy with its device, and
// NameError, the platform's refusal of the name, when it refused it.
type Connected struct {
	Policy    Policy
	NameError error
}

// ConnectAnswer is what POST /v1/connect answers: the status, and warnings
// for what the connect could not do while connecting the computer, such as
// setting its name.
type ConnectAnswer struct {
	Status
	Warnings []string `json:"warnings,omitempty"`
}

// ConnectQuery is what a client asks a proposal for, as the query of
// GET /v1/connect: the values `connect` takes as flags. An empty value is
// the previous file's, then the default.
type ConnectQuery struct {
	Name, Network, SessionStart string
	// Folders are PATH or PATH:ro, each absolute or under ~.
	Folders, Hosts []string
}

// Terms are what a person consents to when this computer is connected.
type Terms struct {
	Name         string   `json:"name"`
	SessionStart string   `json:"session_start"`
	Folders      []Folder `json:"folders"`
	Network      Network  `json:"network"`
}

// TermsOf are a policy's terms.
func TermsOf(p Policy) Terms {
	t := Terms{Name: p.Name, SessionStart: p.SessionStart, Folders: p.Folders, Network: p.Network}
	if t.SessionStart == "" {
		t.SessionStart = SessionStartAllow
	}
	if t.Folders == nil {
		t.Folders = []Folder{}
	}
	if t.Network.Hosts == nil {
		t.Network.Hosts = []string{}
	}
	return t
}

// Proposal is what GET /v1/connect answers: the terms, the facts the person
// is shown with them, the digest a connect presents to say the person saw
// both, and what is wrong with the terms, if anything.
type Proposal struct {
	Policy   Terms    `json:"policy"`
	Facts    []Fact   `json:"facts"`
	Consent  string   `json:"consent"`
	Problems []string `json:"problems"`
}

// The kinds of fact the person is shown before this computer is connected.
// FactNetworkPorts are the hosts the network any reaches on a port beyond
// 443 and 80.
const (
	FactFolders      = "folders"
	FactSecrets      = "secrets"
	FactNetwork      = "network"
	FactNetworkPorts = "network_ports"
	FactReadPaths    = "read_paths"
	FactWho          = "who"
	FactRemembered   = "remembered"
	FactLog          = "log"
)

// Fact is one thing the person is told before this computer is connected:
// a sentence, what it names (the folders, the hosts), and a sentence after
// them. The terminal's connect screen and GET /v1/connect render the same
// facts, so the two never say different things.
type Fact struct {
	Kind  string   `json:"kind"`
	Text  string   `json:"text"`
	Items []string `json:"items,omitempty"`
	Note  string   `json:"note,omitempty"`
	// inline prints the items on one line, joined, as the screen prints the
	// hosts.
	inline bool
}

// screen is the fact as the connect screen prints it.
func (f Fact) screen() string {
	var b strings.Builder
	b.WriteString(f.Text + "\n")
	switch {
	case f.inline && len(f.Items) > 0:
		b.WriteString("  " + strings.Join(f.Items, ", ") + "\n")
	default:
		for _, item := range f.Items {
			b.WriteString("  " + item + "\n")
		}
	}
	if f.Note != "" {
		b.WriteString(f.Note + "\n")
	}
	return b.String()
}

// connectFacts are the facts for a policy, in the connect screen's line
// breaks.
func connectFacts(p Policy) []Fact {
	folders := Fact{Kind: FactFolders, Text: "Sessions you start in Latere will run commands and read and edit files\non this computer, in these folders only:"}
	for _, f := range p.Folders {
		access := "read-only"
		if f.Write {
			access = "read-write"
		}
		folders.Items = append(folders.Items, fmt.Sprintf("%s (%s)", f.Path, access))
	}
	network := Fact{Kind: FactNetwork, Text: "Network: none. Commands on this computer reach no host."}
	var ports *Fact
	switch {
	case p.Network.Mode == NetworkAny:
		network = Fact{
			Kind: FactNetwork, Note: anyWords,
			Text: "Network: any. Commands on this computer may reach any public host on\n" +
				"the internet, over HTTPS and HTTP, but not this computer or the\nnetworks it is on.",
		}
		if len(p.Network.Blocked) > 0 {
			network.Text = "Network: any. Commands on this computer may reach any public host on\n" +
				"the internet, over HTTPS and HTTP, but not this computer, the networks\nit is on, or these hosts:"
			network.Items, network.inline = p.Network.Blocked, true
		}
		if len(p.Network.Hosts) > 0 {
			ports = &Fact{Kind: FactNetworkPorts, Text: "Commands may also reach these hosts on the port named:", Items: p.Network.Hosts, inline: true}
		}
	case p.Network.Mode != NetworkNone && len(p.Network.Hosts) > 0:
		network = Fact{
			Kind: FactNetwork, Text: "Network: commands may reach these hosts only:", Items: p.Network.Hosts, inline: true,
			Note: "A host that accepts writes, such as a git host, can carry data out.",
		}
	}
	who := "Only sessions you start, and triggers you own, can use this computer.\n"
	if p.SessionStart == SessionStartAsk {
		who += sessionStartAskWords + "\n" + switchToAllow
	} else {
		who += sessionStartAllowWords + "\n" +
			"Anyone who can start sessions as you can reach these folders, still\n" +
			"confined to them: never your credentials, keychains or browser profiles,\n" +
			"and no network beyond what is listed above.\n" + switchToAsk
	}
	facts := []Fact{
		folders,
		{Kind: FactSecrets, Text: "Everything in these folders is readable to the agent, a project's .env\nfile and other secrets kept there included."},
		network,
	}
	if ports != nil {
		facts = append(facts, *ports)
	}
	// What a host policy file kept from an earlier connect is granted too,
	// so the person is told it: the paths read outside the folders, and the
	// scheduled triggers that start without asking.
	if len(p.ReadPaths) > 0 {
		facts = append(facts, Fact{Kind: FactReadPaths, Text: "Commands may also read these paths, outside the folders above:", Items: p.ReadPaths})
	}
	facts = append(facts, Fact{Kind: FactWho, Text: who})
	// A remembered trigger past its date asks again, so it grants nothing
	// and is not stated.
	remembered := Fact{Kind: FactRemembered, Text: "These scheduled triggers you remembered start without asking, each until\nthe date beside it:"}
	now := time.Now()
	for _, r := range p.RememberedTriggers {
		if now.Before(r.Until) {
			remembered.Items = append(remembered.Items, fmt.Sprintf("%s (agent version %s) until %s", r.Trigger, r.AgentVersion, r.Until.UTC().Format("2 January 2006")))
		}
	}
	if len(remembered.Items) > 0 {
		facts = append(facts, remembered)
	}
	return append(facts, Fact{Kind: FactLog, Text: "What the agent reads on this computer goes to the session's log and to\nthe model. In an organization's conversation, that organization can\nread it too."})
}

// ConnectFacts are the facts for a policy as a client shows them: each
// sentence on one line, for the client to wrap.
func ConnectFacts(p Policy) []Fact {
	facts := connectFacts(p)
	for i := range facts {
		facts[i].Text = strings.ReplaceAll(facts[i].Text, "\n", " ")
		facts[i].Note = strings.ReplaceAll(facts[i].Note, "\n", " ")
	}
	return facts
}

// Consent is the digest of terms and the facts shown with them, which a
// connect through the socket presents to say the person saw both.
func Consent(t Terms, facts []Fact) string {
	b, err := json.Marshal(struct {
		Terms Terms  `json:"terms"`
		Facts []Fact `json:"facts"`
	}{t, facts})
	if err != nil {
		// Terms and facts are strings, booleans and slices of them, which
		// always encode.
		panic(err)
	}
	sum := sha256.Sum256(b)
	return "sha256:" + hex.EncodeToString(sum[:])
}

// ErrNotRemembered is a remembered trigger the host policy names none of.
var ErrNotRemembered = errors.New("no remembered trigger has this id")

// errClientRemoved ends a device's run that a client removed through the
// control socket.
var errClientRemoved = errors.New("the device was removed through the control socket")

// checkPolicy lists what is wrong with a policy: its own shape, then the
// local driver's folder, read path and network rules. It reports whether
// the driver refused the network as one this computer cannot enforce.
func (e *Engine) checkPolicy(p Policy) (problems []string, unenforceable bool) {
	problems = p.Problems(e.cfg.Paths.Home)
	if len(problems) == 0 {
		if err := CheckDriver(p, e.cfg.Paths, e.cfg.GOOS, e.cfg.NewDriver); err != nil {
			problems = append(problems, err.Error())
			unenforceable = errors.Is(err, local.ErrNetworkUnenforceable)
		}
	}
	if problems == nil {
		problems = []string{}
	}
	return problems, unenforceable
}

// refusePolicy is the refusal of a policy checkPolicy finds wrong, and nil
// for one it does not: CodeNetworkUnenforceable for the network any where
// this computer cannot enforce it, never a silent widening or narrowing,
// and CodePolicyInvalid for anything else.
func (e *Engine) refusePolicy(p Policy) error {
	problems, unenforceable := e.checkPolicy(p)
	switch {
	case unenforceable:
		return Fail(CodeNetworkUnenforceable, strings.Join(problems, "; "), local.ErrNetworkUnenforceable)
	case len(problems) > 0:
		return Fail(CodePolicyInvalid, strings.Join(problems, "; "), nil)
	}
	return nil
}

// enroller is the engine's Enroller, refusing an engine built without one.
func (e *Engine) enroller() (Enroller, error) {
	if e.cfg.Enroll == nil {
		return nil, Fail(CodeServeFailed, "this engine was built without a way to reach the platform", nil)
	}
	return e.cfg.Enroll, nil
}

// unconnectedPolicy reads the host policy file of a computer not connected
// yet, refusing one that is.
func (e *Engine) unconnectedPolicy() (Policy, error) {
	path := e.cfg.Paths.PolicyFile()
	previous, ok, err := LoadPolicy(path)
	if err != nil {
		return Policy{}, Fail(CodePolicyInvalid, "", err)
	}
	if ok && previous.Device != "" {
		return Policy{}, Fail(CodeAlreadyConnected, path+" names the device "+previous.Device, nil)
	}
	return previous, nil
}

// ProposeConnect is GET /v1/connect: the terms for the query, filled as
// `connect` fills its flags and checked as it checks them, with the facts
// the person is shown and the consent a connect presents.
func (e *Engine) ProposeConnect(q ConnectQuery) (Proposal, error) {
	enroll, err := e.enroller()
	if err != nil {
		return Proposal{}, err
	}
	for _, f := range q.Folders {
		if !strings.HasPrefix(f, "/") && f != "~" && !strings.HasPrefix(f, "~/") {
			return Proposal{}, Fail(CodePolicyInvalid, fmt.Sprintf("the folder %q is neither absolute nor under ~", f), nil)
		}
	}
	previous, err := e.unconnectedPolicy()
	if err != nil {
		return Proposal{}, err
	}
	p, err := enroll.Propose(previous, q)
	if err != nil {
		return Proposal{}, Fail(CodePolicyInvalid, "", err)
	}
	if len(q.Folders) == 0 && len(previous.Folders) == 0 {
		// The terminal proposes the folder it runs in; serve runs where its
		// application started it, often /, so a client names the folder.
		p.Folders = nil
	}
	terms, facts := TermsOf(p), ConnectFacts(p)
	problems, _ := e.checkPolicy(p)
	return Proposal{Policy: terms, Facts: facts, Consent: Consent(terms, facts), Problems: problems}, nil
}

// Connect is POST /v1/connect: with a consent that matches the terms and
// the facts the engine would show for them now, it connects this computer
// as `connect` does after its typed yes, and starts serving it.
func (e *Engine) Connect(ctx context.Context, t Terms, consent string) (ConnectAnswer, error) {
	enroll, err := e.enroller()
	if err != nil {
		return ConnectAnswer{}, err
	}
	if !e.connectMu.TryLock() {
		return ConnectAnswer{}, Fail(CodeAlreadyConnected, "another connect is in progress", nil)
	}
	defer e.connectMu.Unlock()
	previous, err := e.unconnectedPolicy()
	if err != nil {
		return ConnectAnswer{}, err
	}
	p := previous
	p.Device, p.Name, p.SessionStart, p.Folders, p.Network = "", CleanName(t.Name), t.SessionStart, t.Folders, cleanNetwork(t.Network)
	if err := e.refusePolicy(p); err != nil {
		return ConnectAnswer{}, err
	}
	// The facts are those of the file as it is now, the read paths and
	// remembered triggers it kept included, so a file changed since the
	// proposal fails the consent.
	if Consent(TermsOf(p), ConnectFacts(p)) != consent {
		return ConnectAnswer{}, Fail(CodeNotConfirmed, "the consent does not match the terms and the facts this engine shows for them now; ask GET /v1/connect again and show its answer", nil)
	}
	done, err := enroll.Connect(ctx, p)
	if err != nil {
		return ConnectAnswer{}, err
	}
	// The person whose tokens connected the computer owns its device.
	e.confirmOwner(done.Policy.Device)
	select {
	case e.connected <- struct{}{}:
	default:
	}
	answer := ConnectAnswer{Status: e.Status()}
	if done.NameError != nil {
		warning := fmt.Sprintf("This computer is connected, and its name %q was not set: %v", p.Name, done.NameError)
		answer.Warnings = append(answer.Warnings, warning)
		e.notice(warning)
	}
	return answer, nil
}

// PutCredentials is PUT /v1/credentials: the tokens a client hands an engine
// that serves with a client's credentials (the latere command's `serve
// --credentials client`), by "cella" and "platform".
func (e *Engine) PutCredentials(tokens map[string]HeldToken) (Status, error) {
	if e.cfg.Credentials == nil {
		return Status{}, ErrNoClientCredentials
	}
	byAudience := map[string]HeldToken{}
	for key, t := range tokens {
		switch key {
		case "cella":
			byAudience[AudienceCella] = t
		case "platform":
			platform := e.cfg.Credentials.PlatformAudience()
			if platform == "" {
				return Status{}, &CredentialsError{Reason: "this engine takes no platform token"}
			}
			byAudience[platform] = t
		default:
			return Status{}, &CredentialsError{Reason: fmt.Sprintf("%q names no audience; hand cella and platform", key)}
		}
	}
	if err := e.cfg.Credentials.Put(byAudience); err != nil {
		return Status{}, err
	}
	status := e.Status()
	e.hub.publish(Event{Kind: EventStatus, Status: &status})
	return status, nil
}

// Policy is GET /v1/policy: the host policy as the engine reads it.
func (e *Engine) Policy() (Policy, error) {
	e.policyMu.Lock()
	defer e.policyMu.Unlock()
	return e.connectedPolicy()
}

// connectedPolicy reads the host policy file of a connected computer. The
// caller holds policyMu.
func (e *Engine) connectedPolicy() (Policy, error) {
	path := e.cfg.Paths.PolicyFile()
	p, ok, err := LoadPolicy(path)
	if err != nil {
		return Policy{}, Fail(CodePolicyInvalid, "", err)
	}
	if !ok || p.Device == "" {
		return Policy{}, Fail(CodeNotConnected, path+" names no device", nil)
	}
	return p, nil
}

// PolicyChange is what PUT /v1/policy changes; a field left out keeps its
// value.
type PolicyChange struct {
	Name    *string   `json:"name"`
	Folders *[]Folder `json:"folders"`
	Network *Network  `json:"network"`
}

// apply is p with the change, checked as `connect` checks a policy.
func (e *Engine) apply(p Policy, c PolicyChange) (Policy, error) {
	if c.Name != nil {
		p.Name = CleanName(*c.Name)
	}
	if c.Folders != nil {
		p.Folders = nil
		for _, f := range *c.Folders {
			path, err := Expand(e.cfg.Paths.Home, f.Path)
			if err != nil {
				return Policy{}, Fail(CodePolicyInvalid, "folders: "+err.Error(), nil)
			}
			p.Folders = append(p.Folders, Folder{Path: Shorten(e.cfg.Paths.Home, path), Write: f.Write})
		}
	}
	if c.Network != nil {
		p.Network = cleanNetwork(*c.Network)
	}
	if err := e.refusePolicy(p); err != nil {
		return Policy{}, err
	}
	return p, nil
}

// cleanNetwork drops the lists a network's mode does not read: the hosts of
// none. A blocked list on a mode other than any is kept, for the check to
// refuse rather than drop silently.
func cleanNetwork(n Network) Network {
	if n.Mode == NetworkNone {
		n.Hosts = nil
	}
	return n
}

// servesTheSame reports whether two policies give the local driver the same
// folders and network, so a change between them needs no reload: a reload
// restarts the worker, which stops every running session.
func servesTheSame(a, b Policy) bool {
	return slices.Equal(a.Folders, b.Folders) && a.Network.Mode == b.Network.Mode && slices.Equal(a.Network.Hosts, b.Network.Hosts) &&
		slices.Equal(a.Network.Blocked, b.Network.Blocked)
}

// PutPolicy is PUT /v1/policy: the change is checked as `connect` checks a
// policy, a new name is set at the platform, and the file is written. A
// change of the folders or the network is read again by the engine serving
// the device, which restarts its worker; a new name alone, or no change,
// leaves every running session alone. The platform's rename runs outside
// the file's lock, so a session start is never held up by it.
func (e *Engine) PutPolicy(ctx context.Context, c PolicyChange) (Status, error) {
	e.policyMu.Lock()
	before, err := e.connectedPolicy()
	if err != nil {
		e.policyMu.Unlock()
		return Status{}, err
	}
	after, err := e.apply(before, c)
	e.policyMu.Unlock()
	if err != nil {
		return Status{}, err
	}
	if after.Name != before.Name {
		enroll, err := e.enroller()
		if err == nil {
			err = enroll.Rename(ctx, after.Device, after.Name)
		}
		if err != nil {
			return Status{}, e.fail(CodeRenameFailed, "", err)
		}
	}
	// The file is read again: what changed in it during the rename (a
	// remembered trigger) stays, and the change is applied over it.
	e.policyMu.Lock()
	now, err := e.connectedPolicy()
	if err == nil && now.Device != before.Device {
		err = Fail(CodeNotConnected, "the host policy file names another device now", nil)
	}
	if err == nil {
		after, err = e.apply(now, c)
	}
	if err == nil {
		if serr := SavePolicy(e.cfg.Paths.PolicyFile(), after); serr != nil {
			err = Fail(CodePolicyInvalid, "write the host policy file", serr)
		}
	}
	e.policyMu.Unlock()
	if err != nil {
		return Status{}, err
	}
	e.mu.Lock()
	served := e.policy
	serving := e.running && served.Device == after.Device
	if serving {
		// The name the status shows changes at once; the folders and the
		// network with the reload.
		e.policy.Name = after.Name
	}
	e.mu.Unlock()
	if serving && !servesTheSame(served, after) {
		if err := e.Reload(); err != nil {
			return Status{}, err
		}
	}
	status := e.Status()
	e.hub.publish(Event{Kind: EventStatus, Status: &status})
	return status, nil
}

// ForgetRemembered is DELETE /v1/policy/remembered/{trigger}: the trigger
// asks again at its next run.
func (e *Engine) ForgetRemembered(trigger string) error {
	e.policyMu.Lock()
	defer e.policyMu.Unlock()
	p, err := e.connectedPolicy()
	if err != nil {
		return err
	}
	if !p.Forget(trigger) {
		return ErrNotRemembered
	}
	if err = SavePolicy(e.cfg.Paths.PolicyFile(), p); err != nil {
		return Fail(CodePolicyInvalid, "write the host policy file", err)
	}
	return nil
}

// Remove is POST /v1/remove: what `latere devices remove` does for this
// computer. The device is removed at the platform, unless local is set for
// a device that belongs to another account or is gone there already; then
// every sandbox stops, the key and the host policy file are deleted, the
// activity log stays, and `serve` waits to be connected again.
func (e *Engine) Remove(ctx context.Context, local bool) (Status, error) {
	e.mu.Lock()
	env := e.policy.Device
	e.mu.Unlock()
	if env == "" {
		e.policyMu.Lock()
		p, err := e.connectedPolicy()
		e.policyMu.Unlock()
		if err != nil {
			return Status{}, err
		}
		env = p.Device
	}
	if !local {
		enroll, err := e.enroller()
		if err == nil {
			err = enroll.Remove(ctx, env)
		}
		if err != nil {
			return Status{}, e.fail(CodeRemoveFailed, "", err)
		}
	}
	// Under startMu a device's run either registered already, and is ended
	// here, or has not checked the file yet, and finds the device gone.
	e.startMu.Lock()
	defer e.startMu.Unlock()
	e.mu.Lock()
	cancel, done := e.devCancel, e.devDone
	e.mu.Unlock()
	if cancel != nil {
		// The device's run stops every sandbox and forgets the device here
		// before it returns.
		cancel(errClientRemoved)
		<-done
	} else if err := e.forgetHere(env); err != nil {
		return Status{}, Fail(CodeKeyUnavailable, "the device was removed, and on this computer: "+err.Error(), err)
	}
	return e.Status(), nil
}

// forgetHere deletes the key and the host policy file of a device removed
// through the control socket, and the record of its owner.
func (e *Engine) forgetHere(env string) error {
	path := e.cfg.Paths.PolicyFile()
	e.policyMu.Lock()
	err := Forget(e.cfg.Keys, path, env, true)
	e.policyMu.Unlock()
	e.unbindCredentials(env)
	return err
}
