// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

//go:build darwin || linux

package device

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	goruntime "runtime"
	"strings"
	"sync"
	"time"

	v1 "latere.ai/x/cella/manifest/v1"
	"latere.ai/x/cella/runtime"
	"latere.ai/x/cella/runtime/local"
	"latere.ai/x/cella/runtime/remote"
	"latere.ai/x/cella/worker"
)

// Account is what the engine asks of the control plane as the person, with
// the Cella token their sign-in mints: a key for the environment, its
// revocation, and whether the environment still exists.
type Account interface {
	// MintKey mints an environment key with the lifetime ttl. A device that
	// was removed answers an error that wraps ErrRemoved.
	MintKey(ctx context.Context, environment string, ttl time.Duration) (Key, error)
	// RevokeKey revokes one key by its jti.
	RevokeKey(ctx context.Context, environment, jti string) error
	// Exists reports whether the environment is still the person's: false
	// when the control plane holds none, or the platform refuses it as
	// removed.
	Exists(ctx context.Context, environment string) (bool, error)
}

// ErrRemoved is an environment the person no longer has.
var ErrRemoved = errors.New("the environment is no longer the person's")

// Config builds an engine.
type Config struct {
	Paths Paths
	// URL is the Cella control plane's public URL, with the base path the
	// routes sit under.
	URL     string
	Keys    KeyStore
	Account Account
	// Version is the CLI's version, which the worker reports and
	// /v1/status shows.
	Version string
	// NewDriver builds the driver from the host policy. Nil is the local
	// driver.
	NewDriver func(local.Options) (runtime.Driver, error)
	// HTTPClient makes the worker's registration. Nil is the worker's own,
	// with its timeout and the tracing transport.
	HTTPClient *http.Client
	// Upgrade reports a newer release, which a long serve announces once a
	// day; nil checks nothing. UpgradeEvery is how often, a day when zero.
	Upgrade      func(context.Context) (string, bool)
	UpgradeEvery time.Duration
	// AdmitTimeout is how long a create may wait for the person; zero is
	// the worker's default.
	AdmitTimeout time.Duration
	// Now is the clock; nil is time.Now. GOOS is the platform the deny list
	// is built for; empty is this one.
	Now  func() time.Time
	GOOS string
	// Enroll reaches the platform as the person for what a client does
	// through the control socket: connect, rename and remove. Nil refuses
	// those routes.
	Enroll Enroller
	// Credentials holds the tokens a client hands under `serve --credentials
	// client`, which Account and Enroll then present. Nil is the saved login.
	Credentials *Credentials
	// LoginSubject is the person the saved login names, recorded as the
	// device's owner once the platform answers that login as its own.
	LoginSubject string
	// WaitToConnect is `serve`: on a computer not connected, or once its
	// device is removed, the engine waits for a client to connect it through
	// the control socket instead of ending.
	WaitToConnect bool
}

// The causes that end one run of the worker without ending the engine.
var (
	errPause      = errors.New("the person paused this computer")
	errReload     = errors.New("the host policy was read again")
	errKeyExpired = errors.New("the environment key expired")
	errGone       = errors.New("the environment is gone")
)

// ErrNotRunning is a pause, resume or reload asked of an engine that is not
// serving.
var ErrNotRunning = errors.New("the engine is not serving")

// checkEvery is the least time between two checks that a refused key's
// environment still exists.
const checkEvery = time.Minute

// Engine serves this computer as one environment: the worker on the local
// driver, the person's approvals, the activity log, the key and its
// rotation.
type Engine struct {
	cfg    Config
	hub    *hub
	broker *broker

	// policyMu orders every read-modify-write of the host policy file.
	policyMu sync.Mutex
	// connectMu lets one connect through the socket run at a time.
	connectMu sync.Mutex
	// startMu orders a device's run starting against a removal through the
	// socket: the run checks the host policy file and registers itself under
	// it, and a removal that finds no run forgets the device under it, so a
	// removal right after a connect never leaves a run serving a device the
	// file no longer names.
	startMu sync.Mutex
	// connected wakes an engine waiting to be connected once a client
	// connected the computer.
	connected chan struct{}

	mu       sync.Mutex
	ctx      context.Context
	policy   Policy
	key      Key
	retiring *Key
	state    string
	detail   string
	driver   runtime.Driver
	host     v1.HostInfo
	sessions int
	// streaming is whether the worker's stream is open now, as its State
	// last said.
	streaming bool
	upgrade   string
	cancel    context.CancelCauseFunc
	// started is the engine's Run; running is one device's run within it.
	started bool
	running bool
	paused  bool
	resume  chan struct{}
	// devCancel ends the device's run, and devDone is closed once it has
	// returned.
	devCancel context.CancelCauseFunc
	devDone   chan struct{}
	activity  *Activity
	lastCheck time.Time
}

// New builds an engine. Nothing runs until Run.
func New(cfg Config) *Engine {
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	if cfg.GOOS == "" {
		cfg.GOOS = goruntime.GOOS
	}
	if cfg.NewDriver == nil {
		cfg.NewDriver = func(o local.Options) (runtime.Driver, error) { return local.New(o) }
	}
	if cfg.UpgradeEvery <= 0 {
		cfg.UpgradeEvery = 24 * time.Hour
	}
	e := &Engine{cfg: cfg, hub: newHub(), state: StateStarting, connected: make(chan struct{}, 1)}
	e.broker = newBroker(e.hub.publish)
	if cfg.Credentials != nil {
		cfg.Credentials.notifyNeed(e.publishStatus)
		// A client may hand tokens before Run reads the file: they are tied
		// to the device's recorded owner from the start.
		if p, ok, err := LoadPolicy(cfg.Paths.PolicyFile()); err == nil && ok && p.Device != "" {
			e.bindCredentials(p.Device)
		}
	}
	return e
}

// bindCredentials ties a client's tokens to the person recorded as the
// owner of device. With none recorded, any person's tokens are taken until
// the platform answers one as the owner (confirmOwner). It does nothing on
// the saved login.
func (e *Engine) bindCredentials(device string) {
	if e.cfg.Credentials == nil {
		return
	}
	subject, err := readOwner(e.cfg.Paths.OwnerFile(), device)
	if err != nil {
		e.notice("Whose device this computer serves could not be read, so it is confirmed again with the next sign-in: " + err.Error())
	}
	e.cfg.Credentials.SetOwner(subject)
}

// confirmOwner records the person the platform just answered as the
// owner of device, by minting its key or reading it as them, when no owner
// is recorded yet, and ties a client's tokens to them from then on.
func (e *Engine) confirmOwner(device string) {
	subject := e.cfg.LoginSubject
	if e.cfg.Credentials != nil {
		subject = e.cfg.Credentials.Subject()
	}
	if subject == "" || device == "" {
		return
	}
	recorded, err := readOwner(e.cfg.Paths.OwnerFile(), device)
	if err != nil || (recorded != "" && recorded != subject) {
		return
	}
	if recorded == "" {
		if err := writeOwner(e.cfg.Paths.OwnerFile(), device, subject); err != nil {
			e.notice("Whose device this computer serves could not be recorded: " + err.Error())
			return
		}
	}
	if e.cfg.Credentials != nil {
		e.cfg.Credentials.SetOwner(subject)
	}
}

// unbindCredentials ends the tie once device is no longer served, and
// deletes the record of its owner.
func (e *Engine) unbindCredentials(device string) {
	if err := removeOwner(e.cfg.Paths.OwnerFile(), device); err != nil {
		e.notice("The record of whose device this computer served could not be deleted: " + err.Error())
	}
	if e.cfg.Credentials != nil {
		e.cfg.Credentials.SetOwner("")
	}
}

// unconfirmed reports whether a client's tokens are not yet confirmed as
// the device owner's: a removal the platform answers to them may mean the
// tokens are another person's, not that the device is gone.
func (e *Engine) unconfirmed() bool {
	return e.cfg.Credentials != nil && e.cfg.Credentials.Owner() == ""
}

// notTheOwner answers a removal the platform gave tokens not confirmed as
// the device owner's: the device is kept, the tokens are dropped, and the
// status says the client should hand the owner's sign-in.
func (e *Engine) notTheOwner(err error) {
	e.cfg.Credentials.Drop()
	e.setState(StateOffline, "the sign-in the application handed is not this device's owner's ("+err.Error()+"); hand the owner's sign-in, or remove this computer here")
}

// publishStatus tells every client the status now.
func (e *Engine) publishStatus() {
	status := e.Status()
	e.hub.publish(Event{Kind: EventStatus, Status: &status})
}

// Subscribe answers every event from now on, and the function that ends
// the subscription.
func (e *Engine) Subscribe() (<-chan Event, func()) { return e.hub.subscribe() }

// Attach counts a client that can answer approvals until the returned
// detach. A create waits only while one is attached.
func (e *Engine) Attach() (detach func()) { return e.broker.attach() }

// Answer gives the person's answer to a waiting create, from the client
// named by.
func (e *Engine) Answer(id, answer, by string) error { return e.broker.answer(id, answer, by) }

// Waiting lists the creates waiting for the person.
func (e *Engine) Waiting() []Approval { return e.broker.waitingNow() }

// Status is the engine's status now.
func (e *Engine) Status() Status {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.statusLocked()
}

func (e *Engine) statusLocked() Status {
	s := Status{
		Device: e.policy.Device, Name: e.policy.Name, State: e.state, Online: e.state == StateOnline,
		Detail: e.detail, Sessions: e.sessions, Waiting: len(e.broker.waitingNow()),
		Version: e.cfg.Version, Upgrade: e.upgrade,
	}
	if e.cfg.Credentials != nil {
		state := e.cfg.Credentials.State()
		s.Credentials = &state
	}
	return s
}

// setState records the state and tells every client when it changed.
func (e *Engine) setState(state, detail string) {
	e.mu.Lock()
	if e.paused && state != StatePaused {
		// A worker ending under a pause reports its stream ended; the
		// computer is paused, which is what the person reads.
		e.mu.Unlock()
		return
	}
	if e.state == state && e.detail == detail {
		e.mu.Unlock()
		return
	}
	e.state, e.detail = state, detail
	status := e.statusLocked()
	e.mu.Unlock()
	e.hub.publish(Event{Kind: EventStatus, Status: &status})
}

// notice tells every client a sentence the person should read.
func (e *Engine) notice(text string) { e.hub.publish(Event{Kind: EventNotice, Notice: text}) }

// Run serves the device this computer is until ctx ends or the control
// plane refuses it, and stops every sandbox before it returns. Under
// WaitToConnect it serves its control socket's clients on a computer not
// connected yet, and after its device is removed, until ctx ends.
func (e *Engine) Run(ctx context.Context) error {
	e.mu.Lock()
	if e.started {
		e.mu.Unlock()
		return Fail(CodeAlreadyServed, "this engine is already running", nil)
	}
	e.started, e.ctx = true, ctx
	e.mu.Unlock()
	defer func() {
		e.mu.Lock()
		e.started = false
		e.mu.Unlock()
	}()
	if e.cfg.Upgrade != nil {
		go e.watchUpgrades(ctx)
	}
	for {
		policy, err := e.readPolicy()
		if err != nil {
			if !e.cfg.WaitToConnect || CodeOf(err) != CodeNotConnected {
				return err
			}
			if e.waitToConnect(ctx) != nil {
				return nil
			}
			continue
		}
		e.bindCredentials(policy.Device)
		err = e.serveDevice(ctx, policy)
		if ctx.Err() != nil && !errors.Is(err, errClientRemoved) {
			return err
		}
		removedHere := errors.Is(err, errClientRemoved)
		switch {
		case removedHere && e.cfg.WaitToConnect:
			e.notice("This computer was removed from your devices.")
			continue
		case removedHere:
			return Fail(CodeRemoved, "the device was removed through the control socket", err)
		case CodeOf(err) == CodeRemoved && e.cfg.WaitToConnect:
			e.notice("This computer was removed from your devices: " + err.Error())
			continue
		}
		return err
	}
}

// waitToConnect waits, in the state not_connected, for a client to connect
// this computer through the control socket, and answers the context's error
// when it ends first.
func (e *Engine) waitToConnect(ctx context.Context) error {
	e.mu.Lock()
	e.policy, e.paused = Policy{}, false
	e.mu.Unlock()
	if e.cfg.Credentials != nil {
		e.cfg.Credentials.SetOwner("")
	}
	e.setState(StateNotConnected, "")
	select {
	case <-e.connected:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// serveDevice serves one device until ctx ends, the control plane refuses
// it, or a client removes it through the control socket; the last answers
// errClientRemoved once every sandbox stopped and the device is forgotten
// here.
func (e *Engine) serveDevice(ctx context.Context, policy Policy) (err error) {
	e.startMu.Lock()
	if _, current := e.livePolicy(policy.Device); !current {
		// A removal through the socket, between the connect and this start,
		// forgot the device: nothing is served for it.
		e.startMu.Unlock()
		return errClientRemoved
	}
	activity, err := OpenActivity(e.cfg.Paths.ActivityDir(policy.Device))
	if err != nil {
		e.startMu.Unlock()
		return Fail(CodeServeFailed, "open the activity log", err)
	}
	devCtx, devCancel := context.WithCancelCause(ctx)
	done := make(chan struct{})
	e.mu.Lock()
	e.ctx, e.policy, e.activity, e.running = devCtx, policy, activity, true
	e.devCancel, e.devDone = devCancel, done
	e.mu.Unlock()
	e.startMu.Unlock()
	defer func() {
		if errors.Is(context.Cause(devCtx), errClientRemoved) {
			err = errClientRemoved
			// The worker deleted the sandboxes it held; a removal while the
			// computer was paused found no worker, so they are deleted here.
			if serr := e.sweep(context.WithoutCancel(ctx), policy); serr != nil {
				e.notice("A sandbox of the removed device could not be deleted: " + serr.Error())
			}
			if ferr := e.forgetHere(policy.Device); ferr != nil {
				err = fmt.Errorf("%w; on this computer: %w", errClientRemoved, ferr)
			}
		}
		e.mu.Lock()
		e.running, e.sessions, e.paused, e.devCancel, e.devDone = false, 0, false, nil, nil
		// The keys were this device's: a key to revoke after the next
		// registration would be revoked against another device.
		e.retiring, e.key = nil, Key{}
		if errors.Is(err, errClientRemoved) {
			e.policy = Policy{}
		}
		e.mu.Unlock()
		if errors.Is(err, errClientRemoved) {
			e.setState(StateNotConnected, "")
		}
		_ = activity.Close()
		devCancel(nil)
		close(done)
	}()
	if err = e.prepareKey(devCtx); err != nil {
		if devCtx.Err() != nil {
			// The engine stopped, or a client removed the device, while the
			// key waited for the person's sign-in: no refusal, and the
			// deferred function answers a removal.
			return nil
		}
		return err
	}
	if e.cfg.Keys.InFile() {
		e.notice("No keychain answered, so this computer's key is kept in " + e.cfg.Keys.Name() + ", readable by your user account.")
	}
	e.confirmSavedLogin(devCtx, policy.Device)
	e.notice(SessionStartNotice(policy))
	for {
		if err = e.waitWhilePaused(devCtx); err != nil {
			return nil //nolint:nilerr // the context ending under a pause is the person stopping the engine, or a removal the deferred function answers
		}
		again, err := e.serveOnce(devCtx)
		if err != nil || !again {
			return err
		}
	}
}

// confirmSavedLogin records the saved login's person as the owner of a
// device whose owner was never recorded, once the platform answers that
// login as the device's own, so an application that serves the device
// later refuses another person's tokens. A device connected before the
// record existed gets one this way.
func (e *Engine) confirmSavedLogin(ctx context.Context, device string) {
	if e.cfg.Credentials != nil || e.cfg.LoginSubject == "" {
		return
	}
	if recorded, err := readOwner(e.cfg.Paths.OwnerFile(), device); err != nil || recorded != "" {
		return
	}
	go func() {
		cctx, cancel := context.WithTimeout(ctx, 30*time.Second)
		defer cancel()
		if exists, err := e.cfg.Account.Exists(cctx, device); err == nil && exists {
			e.confirmOwner(device)
		}
	}()
}

// readPolicy reads the host policy file for a start or a reload, refusing a
// computer that is not connected and a file that cannot be used.
func (e *Engine) readPolicy() (Policy, error) {
	path := e.cfg.Paths.PolicyFile()
	policy, ok, err := LoadPolicy(path)
	if err != nil {
		return Policy{}, Fail(CodePolicyInvalid, "", err)
	}
	if !ok || policy.Device == "" {
		return Policy{}, Fail(CodeNotConnected, path+" names no device", nil)
	}
	if problems := policy.Problems(e.cfg.Paths.Home); len(problems) > 0 {
		return Policy{}, Fail(CodePolicyInvalid, path+": "+strings.Join(problems, "; "), nil)
	}
	return policy, nil
}

// waitWhilePaused blocks while the person paused the computer, and answers
// the context's error when it ends meanwhile.
func (e *Engine) waitWhilePaused(ctx context.Context) error {
	e.mu.Lock()
	paused, resume := e.paused, e.resume
	e.mu.Unlock()
	if !paused {
		return nil
	}
	select {
	case <-resume:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// DriverOptions is the local driver's configuration from a host policy: its
// folders and read paths resolved against home, its network, and the
// engine's own denied paths.
func DriverOptions(p Policy, paths Paths, goos string) (local.Options, error) {
	o := local.Options{
		DataDir: paths.DataDir(p.Device), Deny: paths.Deny(goos), Home: paths.Home,
		Network: local.Network{Mode: p.Network.Mode, Hosts: p.Network.Hosts},
	}
	for _, f := range p.Folders {
		path, err := Expand(paths.Home, f.Path)
		if err != nil {
			return local.Options{}, err
		}
		o.Grants = append(o.Grants, local.Grant{Path: path, Write: f.Write})
	}
	for _, r := range p.ReadPaths {
		path, err := Expand(paths.Home, r)
		if err != nil {
			return local.Options{}, err
		}
		o.ReadPaths = append(o.ReadPaths, path)
	}
	return o, nil
}

// CheckDriver builds the local driver from a policy over a throwaway data
// directory, which is how its folder, read path and network rules are
// checked without touching the sandboxes of a running engine.
func CheckDriver(p Policy, paths Paths, goos string, build func(local.Options) (runtime.Driver, error)) error {
	o, err := DriverOptions(p, paths, goos)
	if err != nil {
		return err
	}
	if err = os.MkdirAll(paths.Cache, 0o700); err != nil {
		return err
	}
	scratch, err := os.MkdirTemp(paths.Cache, "check-")
	if err != nil {
		return err
	}
	defer func() { _ = os.RemoveAll(scratch) }()
	o.DataDir = scratch
	d, err := build(o)
	if err != nil {
		return err
	}
	if c, ok := d.(io.Closer); ok {
		return c.Close()
	}
	return nil
}

// serveOnce runs the worker once, until it ends, and reports whether the
// engine serves again: after a pause, a reload, or a key renewed at its
// expiry.
func (e *Engine) serveOnce(ctx context.Context) (bool, error) {
	e.mu.Lock()
	policy := e.policy
	e.mu.Unlock()
	o, err := DriverOptions(policy, e.cfg.Paths, e.cfg.GOOS)
	if err != nil {
		return false, Fail(CodePolicyInvalid, "", err)
	}
	if err = os.MkdirAll(o.DataDir, 0o700); err != nil {
		return false, Fail(CodeServeFailed, "create the sandboxes' directory", err)
	}
	d, err := e.cfg.NewDriver(o)
	switch {
	case errors.Is(err, local.ErrRunsAsRoot):
		return false, Fail(CodeRunsAsRoot, "", err)
	case err != nil:
		return false, Fail(CodePolicyInvalid, "", err)
	}
	defer func() {
		if c, ok := d.(io.Closer); ok {
			_ = c.Close()
		}
	}()
	var host v1.HostInfo
	if describer, ok := d.(runtime.HostDescriber); ok {
		host = describer.Host()
	}
	runCtx, cancel := context.WithCancelCause(ctx)
	defer cancel(nil)
	e.mu.Lock()
	e.driver, e.host, e.cancel = d, host, cancel
	key := e.key
	paused := e.paused
	e.mu.Unlock()
	if paused {
		// A pause asked while the driver was being built.
		return true, nil
	}
	e.setState(StateStarting, "")
	expiry := time.AfterFunc(key.ExpiresAt.Sub(e.cfg.Now()), func() { cancel(errKeyExpired) })
	defer expiry.Stop()
	// The worker's own log lines are not the person's: what they read is
	// the engine's events, which State and Record feed.
	err = worker.Run(runCtx, worker.Options{
		URL: e.cfg.URL, Environment: policy.Device, Key: e.currentKey, Driver: d,
		Policy: e, Record: e.record, State: e.workerState, HTTPClient: e.cfg.HTTPClient,
		Log: slog.New(slog.DiscardHandler), Version: e.cfg.Version, AdmitTimeout: e.cfg.AdmitTimeout,
	})
	e.mu.Lock()
	e.driver, e.cancel, e.sessions, e.streaming = nil, nil, 0, false
	e.mu.Unlock()
	if ctx.Err() != nil {
		if errors.Is(context.Cause(ctx), errClientRemoved) {
			// The platform ends the sandboxes it knows of; these are stopped
			// here too, so no command outlives the removal.
			if err := deleteAll(ctx, d); err != nil {
				e.notice("A sandbox of the removed device could not be stopped: " + err.Error())
			}
		}
		return false, nil
	}
	cause := context.Cause(runCtx)
	switch {
	case errors.Is(cause, errPause):
		e.setState(StatePaused, "")
		return true, nil
	case errors.Is(cause, errReload):
		return true, nil
	case errors.Is(cause, errKeyExpired):
		if rerr := e.renewExpired(ctx); rerr != nil {
			if ctx.Err() != nil {
				// The engine stopped, or a client removed the device, while
				// the renewal waited for the person's sign-in.
				return false, nil
			}
			return false, rerr
		}
		return true, nil
	case errors.Is(cause, errGone):
		detail := "the environment " + policy.Device + " is gone and its key is refused"
		if err := deleteAll(ctx, d); err != nil {
			detail += "; " + err.Error()
		}
		return false, e.removed(detail)
	}
	var refusal *worker.Refusal
	if errors.As(err, &refusal) {
		return false, e.refused(refusal)
	}
	switch {
	case errors.Is(err, local.ErrRunsAsRoot):
		return false, Fail(CodeRunsAsRoot, "", err)
	case err != nil:
		return false, Fail(CodeSandboxUnavailable, "", err)
	}
	return false, nil
}

// deleteAll stops and deletes every sandbox the driver holds, for a device
// that was removed while the worker could not hear it. A failure on one
// sandbox does not keep the others.
func deleteAll(parent context.Context, d runtime.Driver) error {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(parent), time.Minute)
	defer cancel()
	states, err := d.List(ctx, runtime.Filter{})
	if err != nil {
		return fmt.Errorf("the sandboxes could not be listed to delete them: %w", err)
	}
	var errs []error
	for _, s := range states {
		if err := d.Stop(ctx, s.ID); err != nil && !errors.Is(err, runtime.ErrNotFound) && !errors.Is(err, runtime.ErrNotRunning) {
			errs = append(errs, fmt.Errorf("stop %s: %w", s.ID, err))
		}
		if err := d.Delete(ctx, s.ID); err != nil && !errors.Is(err, runtime.ErrNotFound) {
			errs = append(errs, fmt.Errorf("delete %s: %w", s.ID, err))
		}
	}
	return errors.Join(errs...)
}

// sweep stops and deletes every sandbox of a device removed through the
// control socket with a driver of its own, which also reaches a removal
// that came while the computer was paused and no worker held a driver, and
// deletes their directory.
func (e *Engine) sweep(ctx context.Context, policy Policy) error {
	o, err := DriverOptions(policy, e.cfg.Paths, e.cfg.GOOS)
	if err != nil {
		return err
	}
	if _, err = os.Stat(o.DataDir); errors.Is(err, os.ErrNotExist) {
		return nil
	}
	d, err := e.cfg.NewDriver(o)
	if err != nil {
		return err
	}
	err = deleteAll(ctx, d)
	if c, ok := d.(io.Closer); ok {
		if cerr := c.Close(); cerr != nil && err == nil {
			err = cerr
		}
	}
	if rerr := os.RemoveAll(o.DataDir); rerr != nil && err == nil {
		err = rerr
	}
	return err
}

// refused is the answers table for a refusal the worker stops on.
func (e *Engine) refused(r *worker.Refusal) error {
	switch r.Code {
	case remote.ReasonServeRefused, remote.ReasonKeyRevoked:
		return e.removed(r.Error())
	case remote.ReasonWorkerLimit:
		return Fail(CodeWorkerLimit, r.Error(), r)
	case remote.ReasonWorkerReplaced:
		return Fail(CodeWorkerReplaced, r.Error(), r)
	}
	return Fail(CodeServeFailed, r.Error(), r)
}

// removed forgets a device the person no longer has: the key goes, and the
// host policy file keeps its folders, network and remembered triggers but
// names no device, so the next connect connects the computer anew. The
// activity log stays.
func (e *Engine) removed(detail string) error {
	e.mu.Lock()
	env := e.policy.Device
	e.mu.Unlock()
	if err := e.cfg.Keys.Delete(env); err != nil {
		detail += "; the key could not be deleted: " + err.Error()
	}
	if err := e.forgetDevice(env); err != nil {
		detail += "; the host policy file could not be updated: " + err.Error()
	}
	e.unbindCredentials(env)
	return Fail(CodeRemoved, detail, nil)
}

// forgetDevice clears the device from the host policy file when it still
// names env.
func (e *Engine) forgetDevice(env string) error {
	e.policyMu.Lock()
	defer e.policyMu.Unlock()
	path := e.cfg.Paths.PolicyFile()
	p, ok, err := LoadPolicy(path)
	if err != nil || !ok || p.Device != env {
		return err
	}
	p.Device = ""
	return SavePolicy(path, p)
}

// prepareKey reads the key from the store, and mints one where none is kept,
// where the one kept expired, or where it is older than RotateAfter. A
// rotated key's predecessor is revoked once the worker registers with the
// new one.
func (e *Engine) prepareKey(ctx context.Context) error {
	e.mu.Lock()
	env := e.policy.Device
	e.mu.Unlock()
	k, ok, err := e.cfg.Keys.Get(env)
	if err != nil {
		return Fail(CodeKeyUnavailable, "", err)
	}
	now := e.cfg.Now()
	switch {
	case !ok:
		next, err := e.mintWaiting(ctx, env)
		if err != nil {
			return e.mintFailed(err, e.fail(CodeKeyUnavailable, "no key is kept for "+env+" and none could be minted", err))
		}
		k = next
	case !now.Before(k.ExpiresAt):
		next, err := e.mintWaiting(ctx, env)
		if err != nil {
			return e.mintFailed(err, e.fail(CodeSignInExpired, "the key expired at "+k.ExpiresAt.Format(time.RFC3339), err))
		}
		k = next
	case now.Sub(k.MintedAt) >= RotateAfter:
		next, err := e.mint(ctx, env)
		if errors.Is(err, ErrRemoved) && e.unconfirmed() {
			e.notTheOwner(err)
			break
		}
		if errors.Is(err, ErrRemoved) {
			return e.removed(err.Error())
		}
		if err != nil {
			renew := "Sign in again with `latere login` to renew it."
			if e.cfg.Credentials != nil {
				renew = "It is replaced once the application hands this computer your sign-in again."
			}
			e.notice(fmt.Sprintf("This computer's key could not be replaced, so it serves with its current key until %s. %s",
				k.ExpiresAt.Local().Format("2 January 2006"), renew))
			break
		}
		old := k
		e.mu.Lock()
		e.retiring = &old
		e.mu.Unlock()
		k = next
	}
	e.mu.Lock()
	e.key = k
	e.mu.Unlock()
	return nil
}

// mintFailed answers a failed mint: a removed device is forgotten, and any
// other failure is the refusal given.
func (e *Engine) mintFailed(err error, otherwise *Error) error {
	if errors.Is(err, ErrRemoved) {
		return e.removed(err.Error())
	}
	return otherwise
}

// mintWaiting mints a key for env. Under a client's credentials, a mint
// that lacks the person's token waits for the client to hand one, however
// long that takes, offline meanwhile with the status saying the token is
// needed, rather than ending the engine and the socket the client needs.
func (e *Engine) mintWaiting(ctx context.Context, env string) (Key, error) {
	for {
		k, err := e.mint(ctx, env)
		switch {
		case err == nil || e.cfg.Credentials == nil:
			return k, err
		case errors.Is(err, ErrRemoved) && e.unconfirmed():
			e.notTheOwner(err)
		case errors.Is(err, ErrCredentialsNeeded):
			e.setState(StateOffline, "this computer's key is minted once the application hands it your sign-in")
		default:
			return k, err
		}
		if err = e.cfg.Credentials.Wait(ctx, AudienceCella); err != nil {
			return Key{}, err
		}
	}
}

// fail builds a refusal as Fail does, with the next step a client's engine
// gives: where the saved login's refusal says to sign in with the CLI, the
// application hands the sign-in.
func (e *Engine) fail(code, detail string, err error) *Error {
	if errors.Is(err, ErrCredentialsNeeded) {
		code = CodeCredentialsNeeded
	}
	f := Fail(code, detail, err)
	if e.cfg.Credentials != nil && strings.Contains(f.Hint, "latere login") {
		f.Hint = refusals[CodeCredentialsNeeded].hint
	}
	return f
}

// mint mints a key for env as the person and keeps it in the store.
func (e *Engine) mint(ctx context.Context, env string) (Key, error) {
	k, err := e.cfg.Account.MintKey(ctx, env, KeyTTL)
	if err != nil {
		return Key{}, err
	}
	k.Environment = env
	if k.MintedAt.IsZero() {
		k.MintedAt = e.cfg.Now().UTC()
	}
	if k.ExpiresAt.IsZero() {
		k.ExpiresAt = k.MintedAt.Add(KeyTTL)
	}
	if err = e.cfg.Keys.Put(k); err != nil {
		return Key{}, Fail(CodeKeyUnavailable, "keep the new key", err)
	}
	// The platform minted it for this person: they own the device.
	e.confirmOwner(env)
	return k, nil
}

// renewExpired mints a key once the current one expired while serving, and
// stops with the sign-in refusal when it cannot.
func (e *Engine) renewExpired(ctx context.Context) error {
	e.mu.Lock()
	env, old := e.policy.Device, e.key
	e.mu.Unlock()
	k, err := e.mintWaiting(ctx, env)
	if err != nil {
		return e.mintFailed(err, e.fail(CodeSignInExpired, "the key expired at "+old.ExpiresAt.Format(time.RFC3339), err))
	}
	e.mu.Lock()
	e.key = k
	e.mu.Unlock()
	return nil
}

// currentKey is the worker's Key: read once per connection, so a key renewed
// while the engine serves is the one the next connection presents.
func (e *Engine) currentKey(context.Context) (string, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.key.Token == "" {
		return "", errors.New("no environment key is held")
	}
	return e.key.Token, nil
}

// registered is a registration the control plane accepted: the computer is
// online, and a key this start replaced is revoked now that the new one
// works.
func (e *Engine) registered() {
	e.setState(StateOnline, "")
	e.mu.Lock()
	old, ctx, env := e.retiring, e.ctx, e.policy.Device
	e.retiring = nil
	e.mu.Unlock()
	if old == nil {
		return
	}
	go func() {
		rctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 30*time.Second)
		defer cancel()
		if err := e.cfg.Account.RevokeKey(rctx, env, old.JTI); err != nil {
			e.notice(fmt.Sprintf("The key this computer replaced could not be revoked; it stops working on its own on %s.",
				old.ExpiresAt.Local().Format("2 January 2006")))
		}
	}()
}

// unauthorized is a registration refused at the key: the device may have
// been removed while the computer could not hear it, or the key expired or
// was revoked. It asks, as the person, whether the environment is still
// theirs, at most once every checkEvery.
func (e *Engine) unauthorized() {
	e.mu.Lock()
	now := e.cfg.Now()
	if now.Sub(e.lastCheck) < checkEvery {
		e.mu.Unlock()
		return
	}
	e.lastCheck = now
	ctx, cancel, env, key := e.ctx, e.cancel, e.policy.Device, e.key
	e.mu.Unlock()
	if cancel == nil {
		return
	}
	go func() {
		cctx, stop := context.WithTimeout(ctx, 30*time.Second)
		defer stop()
		exists, err := e.cfg.Account.Exists(cctx, env)
		if exists {
			e.confirmOwner(env)
		}
		switch {
		case !exists && err == nil && e.unconfirmed():
			e.notTheOwner(errors.New("the platform answered that the device is not theirs"))
		case err != nil:
			// The person's own sign-in cannot ask: the worker keeps trying,
			// and the next refusal asks again.
			e.setState(StateOffline, "the control plane refused this computer's key, and whether the device still exists could not be read: "+err.Error())
		case !exists:
			cancel(errGone)
		case !e.cfg.Now().Before(key.ExpiresAt):
			cancel(errKeyExpired)
		default:
			// The environment is the person's and the key is refused: it
			// was revoked elsewhere. A new one serves the next connection.
			k, err := e.mint(cctx, env)
			if err != nil {
				e.setState(StateOffline, "the control plane refused this computer's key, and a new one could not be minted: "+err.Error())
				return
			}
			e.mu.Lock()
			e.key = k
			e.mu.Unlock()
		}
	}()
}

// workerState is the worker's State: the computer is online once the
// control plane took the registration and the stream is open, and offline,
// with why, each time it stops being connected and reconnects. A key the
// control plane refused with 401 asks whether the device still exists. The
// last call, as Run returns, changes nothing: what ended Run (a pause, a
// reload, a stop or a refusal) is serveOnce's to answer. It runs on Run's
// goroutine, which waits for it, so whatever takes longer runs apart.
func (e *Engine) workerState(s worker.State) {
	e.mu.Lock()
	wasConnected := e.streaming
	e.streaming = s.Connected
	e.mu.Unlock()
	if s.Connected {
		e.registered()
		return
	}
	if !s.Reconnect {
		return
	}
	var refused *worker.RegistrationError
	switch {
	case errors.As(s.Err, &refused) && refused.StatusCode == http.StatusUnauthorized:
		e.setState(StateOffline, "the control plane refused this computer's key")
		e.unauthorized()
	case errors.As(s.Err, &refused):
		e.setState(StateOffline, "the control plane answered the registration with "+refused.Status)
	case wasConnected:
		e.setState(StateOffline, "the connection to the control plane ended: "+errText(s.Err)+"; reconnecting")
	default:
		e.setState(StateOffline, "the control plane could not be reached: "+errText(s.Err))
	}
}

// errText is an error's text, and a plain word for none.
func errText(err error) string {
	if err == nil {
		return "no reason was given"
	}
	return err.Error()
}

// record is the worker's Record: one line of activity per operation, and a
// new count of sessions after one that starts or ends a sandbox.
func (e *Engine) record(r worker.Record) {
	e.log(Entry{
		Time: r.Time.UTC(), Type: r.Type, Sandbox: r.Sandbox,
		Session: r.Labels[LabelSession], Agent: r.Labels[LabelAgent], Trigger: r.Labels[LabelTrigger],
		Command: r.Command, Path: r.Path, To: r.To, Outcome: r.Outcome, Reason: r.Reason, ExitCode: r.ExitCode,
	})
	switch r.Type {
	case remote.OpCreate, remote.OpStart, remote.OpStop, remote.OpDelete:
		e.recount()
	}
}

// log writes an entry to the activity log and tells every client.
func (e *Engine) log(entry Entry) {
	e.mu.Lock()
	activity := e.activity
	e.mu.Unlock()
	if activity != nil {
		if err := activity.Write(entry); err != nil {
			e.notice("The activity log could not be written: " + err.Error())
		}
	}
	e.hub.publish(Event{Kind: EventActivity, Activity: &entry})
}

// recount counts the sessions with a sandbox running, by their session
// label, and tells every client when the count changed.
func (e *Engine) recount() {
	e.mu.Lock()
	d := e.driver
	e.mu.Unlock()
	if d == nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	states, err := d.List(ctx, runtime.Filter{})
	if err != nil {
		return
	}
	sessions := map[string]bool{}
	for _, s := range states {
		if s.Phase != runtime.Running {
			continue
		}
		id := s.Labels[LabelSession]
		if id == "" {
			id = s.ID
		}
		sessions[id] = true
	}
	e.mu.Lock()
	changed := e.sessions != len(sessions)
	e.sessions = len(sessions)
	status := e.statusLocked()
	e.mu.Unlock()
	if changed {
		e.hub.publish(Event{Kind: EventStatus, Status: &status})
	}
}

// Admit is the host policy's question before a create: a remembered
// scheduled trigger, and under session_start allow a session a person
// started or a scheduled trigger started, run without asking; every other
// create waits for the person, or is refused at once when no one is attached
// to answer. Every create admitted is told to the clients.
func (e *Engine) Admit(ctx context.Context, c worker.Create) worker.Verdict {
	e.mu.Lock()
	host, env := e.host, e.policy.Device
	e.mu.Unlock()
	now := e.cfg.Now()
	a := approvalOf(c, host, now)
	// The file as it is now, so a trigger forgotten or a switch to ask
	// while the engine runs applies to this create. A file that cannot be
	// read, or names another device, asks.
	policy, current := e.livePolicy(env)
	switch {
	case current && a.Rememberable && policy.Remembers(a.Trigger, a.AgentVersion, now):
		e.logApproval(a, answerRemembered, "")
		return e.admitted(a)
	case current && policy.StartsWithoutAsking(a):
		e.logApproval(a, answerSessionStart, "")
		return e.admitted(a)
	}
	got, err := e.broker.ask(ctx, a)
	if err != nil {
		e.logApproval(a, "unanswered", "")
		return worker.Verdict{Reason: "no answer was given on this computer in time"}
	}
	e.logApproval(a, got.answer, got.by)
	switch got.answer {
	case AnswerAllow:
		return e.admitted(a)
	case AnswerRemember:
		if err := e.rememberTrigger(env, a, now); err != nil {
			e.notice("The trigger could not be remembered: " + err.Error())
		}
		return e.admitted(a)
	case answerNoOne:
		return worker.Verdict{Reason: ReasonNoOneHere}
	}
	return worker.Verdict{Reason: ReasonRefused}
}

// admitted tells every client that a session starts on the computer, and
// admits it.
func (e *Engine) admitted(a Approval) worker.Verdict {
	e.hub.publish(Event{Kind: EventStarted, Approval: &a})
	return worker.Verdict{Allow: true}
}

// Check allows every operation on a sandbox the person admitted. Rules per
// command are not part of this engine.
func (e *Engine) Check(context.Context, worker.Operation) worker.Verdict {
	return worker.Verdict{Allow: true}
}

// livePolicy reads the host policy file as it is now, and reports whether it
// still names env.
func (e *Engine) livePolicy(env string) (Policy, bool) {
	e.policyMu.Lock()
	defer e.policyMu.Unlock()
	p, ok, err := LoadPolicy(e.cfg.Paths.PolicyFile())
	if err != nil || !ok || p.Device != env {
		return Policy{}, false
	}
	return p, true
}

// rememberTrigger writes the trigger into the host policy file.
func (e *Engine) rememberTrigger(env string, a Approval, now time.Time) error {
	e.policyMu.Lock()
	defer e.policyMu.Unlock()
	path := e.cfg.Paths.PolicyFile()
	p, ok, err := LoadPolicy(path)
	if err != nil {
		return err
	}
	if !ok || p.Device != env {
		return errors.New(path + " no longer names this device")
	}
	p.Remember(a.Trigger, a.AgentVersion, now)
	return SavePolicy(path, p)
}

// logApproval writes an approval's answer to the activity log.
func (e *Engine) logApproval(a Approval, answer, by string) {
	e.log(Entry{
		Time: e.cfg.Now().UTC(), Type: TypeApproval, Sandbox: a.Sandbox, Session: a.Session,
		Agent: a.AgentRef, Trigger: a.Trigger, Answer: answer, By: by,
	})
}

// Pause disconnects the worker, stopping every sandbox, until Resume.
func (e *Engine) Pause() error {
	e.mu.Lock()
	if !e.running {
		e.mu.Unlock()
		return ErrNotRunning
	}
	if e.paused {
		e.mu.Unlock()
		return nil
	}
	e.paused, e.resume = true, make(chan struct{})
	if e.cancel != nil {
		e.cancel(errPause)
	}
	e.mu.Unlock()
	e.setState(StatePaused, "")
	return nil
}

// Resume connects the worker again after a pause.
func (e *Engine) Resume() error {
	e.mu.Lock()
	defer e.mu.Unlock()
	if !e.running {
		return ErrNotRunning
	}
	if !e.paused {
		return nil
	}
	e.paused = false
	close(e.resume)
	return nil
}

// Reload reads the host policy file again and registers anew. A file that
// cannot be used, or that names another device, is refused and the engine
// keeps serving as it was.
func (e *Engine) Reload() error {
	e.mu.Lock()
	running, env := e.running, e.policy.Device
	e.mu.Unlock()
	if !running {
		return ErrNotRunning
	}
	policy, err := e.readPolicy()
	if err != nil {
		return err
	}
	if policy.Device != env {
		return Fail(CodePolicyInvalid, fmt.Sprintf("the file names the device %s, and this engine serves %s", policy.Device, env), nil)
	}
	if err = CheckDriver(policy, e.cfg.Paths, e.cfg.GOOS, e.cfg.NewDriver); err != nil {
		return Fail(CodePolicyInvalid, "", err)
	}
	e.mu.Lock()
	e.policy = policy
	if e.cancel != nil {
		e.cancel(errReload)
	}
	e.mu.Unlock()
	return nil
}

// watchUpgrades asks for a newer release once every UpgradeEvery, and says
// once a day that one is available.
func (e *Engine) watchUpgrades(ctx context.Context) {
	ticker := time.NewTicker(e.cfg.UpgradeEvery)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
		latest, ok := e.cfg.Upgrade(ctx)
		if !ok {
			continue
		}
		text := "latere " + latest + " is available; restart to apply"
		e.mu.Lock()
		e.upgrade = text
		status := e.statusLocked()
		e.mu.Unlock()
		e.notice("An upgrade is available: " + text + ".")
		e.hub.publish(Event{Kind: EventStatus, Status: &status})
	}
}

// ensureDir makes a directory of mode 0700, and sets the mode of one that
// exists.
func ensureDir(dir string) error {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	return os.Chmod(filepath.Clean(dir), 0o700)
}
