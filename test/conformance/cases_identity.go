// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package conformance

import (
	"context"
	"fmt"
	"net/http"
	"strings"
)

// missingID is a well-formed id no object holds. It is one past the id
// design 006 reserves for the authorizer's probe, which the suite never
// reads: a probe that is always denied would answer a refusal of its own.
const missingID = "sbx_00000000000000000000000001"

// identityCases prove who may act: the refusal classes of a caller, the
// scope of a sandbox's own token, and what every request becomes while the
// permission service is down.
func identityCases() []Case {
	return []Case{
		{"identity", "case006Unauthenticated", case006Unauthenticated},
		{"identity", "case006Forbidden", case006Forbidden},
		{"identity", "case006NotFound", case006NotFound},
		{"identity", "case006WorkloadTokenScope", case006WorkloadTokenScope},
		{"identity", "case006AuthorizerUnavailable", case006AuthorizerUnavailable},
	}
}

// case006Unauthenticated: no bearer and a bearer that verifies against
// nothing are both 401, and neither reaches a handler.
func case006Unauthenticated(ctx context.Context, e *Env) error {
	x, err := e.caller.send(ctx, http.MethodGet, "/v1/sandboxes", request{NoBearer: true})
	if err != nil {
		return err
	}
	if err := x.refusal("unauthenticated"); err != nil {
		return fmt.Errorf("with no bearer: %w", err)
	}
	x, err = e.caller.send(ctx, http.MethodGet, "/v1/sandboxes", request{Token: "not.a.token"})
	if err != nil {
		return err
	}
	if err := x.refusal("unauthenticated"); err != nil {
		return fmt.Errorf("with a bearer that verifies against nothing: %w", err)
	}
	return nil
}

// case006Forbidden: a deny on the caller's own action is 403, and it is the
// action that is refused and not the object that is hidden.
//
// The object is created and not read before the deny is in force: an allow
// for one object and one action may be held for the time the permission
// service granted it, so a case that read the object first would be reading
// that grant and not the deny.
func case006Forbidden(ctx context.Context, e *Env) error {
	if e.cfg.AuthorizerControl == "" {
		return skipf("no authorizer control URL: set AuthorizerControl to drive a deny")
	}
	obj, err := e.create(ctx, e.caller, e.manifest(e.name()))
	if err != nil {
		return err
	}
	if err := e.control(ctx, e.cfg.AuthorizerControl, "deny:sandbox.read"); err != nil {
		return err
	}
	defer func() { _ = e.control(context.WithoutCancel(ctx), e.cfg.AuthorizerControl, "") }()
	x, err := e.caller.get(ctx, "/v1/sandboxes/"+obj.Status.ID)
	if err != nil {
		return err
	}
	return x.refusal("forbidden")
}

// case006NotFound: an id nothing holds and a name none of the caller's
// objects holds are both 404, on a read and on a verb, so a caller learns
// nothing from the shape of a refusal.
//
// Whether one subject reads another's object is the permission service's
// answer and not the API's, so no case asserts it: a server behind an
// endpoint that allows it is conformant and one behind an endpoint that
// refuses it is too.
func case006NotFound(ctx context.Context, e *Env) error {
	calls := []struct {
		method, path string
	}{
		{http.MethodGet, "/v1/sandboxes/" + missingID},
		{http.MethodGet, "/v1/sandboxes/conformance-" + e.run + "-nothing"},
		{http.MethodPost, "/v1/sandboxes/" + missingID + "/stop"},
		{http.MethodDelete, "/v1/sandboxes/" + missingID},
	}
	for _, call := range calls {
		x, err := e.caller.send(ctx, call.method, call.path, request{})
		if err != nil {
			return err
		}
		if err := x.refusal("not_found"); err != nil {
			return err
		}
	}
	// A name resolves among the objects whose owner is the caller's, so a
	// name another subject holds is a name this caller has no object for.
	// That is the API's own rule and not a decision it asks for.
	other, err := e.second()
	if err != nil {
		return err
	}
	name := e.name()
	if _, err := e.create(ctx, other, e.manifest(name)); err != nil {
		return err
	}
	x, err := e.caller.get(ctx, "/v1/sandboxes/"+name)
	if err != nil {
		return err
	}
	return x.refusal("not_found")
}

// case006WorkloadTokenScope: the token a driver projects inside a sandbox
// authenticates for that sandbox and carries none of the control plane's own
// record of it.
func case006WorkloadTokenScope(ctx context.Context, e *Env) error {
	obj, err := e.sandbox(ctx, e.caller)
	if err != nil {
		return err
	}
	result, x, err := e.exec(ctx, e.caller, obj.Status.ID, "/bin/sh", "-c", `cat "$CELLA_TOKEN_FILE"`)
	if err != nil {
		return err
	}
	if x.Status != http.StatusOK || result.ExitCode != 0 {
		return x.disagree("the sandbox's own token at $CELLA_TOKEN_FILE",
			fmt.Sprintf("status %d, exit %d, stderr %q", x.Status, result.ExitCode, result.Stderr))
	}
	token := strings.TrimSpace(result.Stdout)
	if token == "" {
		return x.disagree("a token inside the sandbox", "an empty file at $CELLA_TOKEN_FILE")
	}
	workload := newClient(e.caller.base, token)
	read, err := workload.get(ctx, "/v1/sandboxes/"+obj.Status.ID)
	if err != nil {
		return err
	}
	if err := read.status(http.StatusOK); err != nil {
		return fmt.Errorf("the workload reading its own sandbox: %w", err)
	}
	if strings.Contains(string(read.Body), "tokenState") {
		return read.disagree("no record of the token in the answer", "status.tokenState in the body")
	}
	// The token names one sandbox, so an object it does not name is an
	// object it does not reach. What the permission service says about a
	// sibling is that service's answer; an id nothing holds is the API's.
	refused, err := workload.get(ctx, "/v1/sandboxes/"+missingID)
	if err != nil {
		return err
	}
	return refused.refusal("not_found")
}

// case006AuthorizerUnavailable: with the permission service down, every
// request is refused with authorizer_unavailable, and the server answers
// again when it returns. This is the criterion of design 001: a control
// plane that cannot ask never decides on its own.
func case006AuthorizerUnavailable(ctx context.Context, e *Env) error {
	if e.cfg.AuthorizerControl == "" {
		return skipf("no authorizer control URL: set AuthorizerControl to take the permission service down")
	}
	obj, err := e.create(ctx, e.caller, e.manifest(e.name()))
	if err != nil {
		return err
	}
	if err := e.control(ctx, e.cfg.AuthorizerControl, "unavailable"); err != nil {
		return err
	}
	defer func() { _ = e.control(context.WithoutCancel(ctx), e.cfg.AuthorizerControl, "") }()
	calls := []struct {
		method, path string
		body         []byte
	}{
		{http.MethodGet, "/v1/sandboxes", nil},
		{http.MethodGet, "/v1/sandboxes/" + obj.Status.ID, nil},
		{http.MethodPost, "/v1/sandboxes", e.manifest(e.name())},
		{http.MethodDelete, "/v1/sandboxes/" + obj.Status.ID, nil},
	}
	for _, call := range calls {
		x, err := e.caller.send(ctx, call.method, call.path, request{Body: call.body, ContentType: "application/json"})
		if err != nil {
			return err
		}
		if err := x.refusal("authorizer_unavailable"); err != nil {
			return err
		}
	}
	if err := e.control(ctx, e.cfg.AuthorizerControl, ""); err != nil {
		return err
	}
	back, err := e.caller.get(ctx, "/v1/sandboxes")
	if err != nil {
		return err
	}
	return back.status(http.StatusOK)
}
