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
func case006Forbidden(ctx context.Context, e *Env) error {
	if e.cfg.AuthorizerControl == "" {
		return skipf("no authorizer control URL: set AuthorizerControl to drive a deny")
	}
	obj, err := e.sandbox(ctx, e.caller)
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

// case006NotFound: an object that is not there and an object of another
// subject are both 404, so a caller learns nothing about what it may not
// reach.
func case006NotFound(ctx context.Context, e *Env) error {
	x, err := e.caller.get(ctx, "/v1/sandboxes/"+missingID)
	if err != nil {
		return err
	}
	if err := x.refusal("not_found"); err != nil {
		return fmt.Errorf("reading an id nothing holds: %w", err)
	}
	other, err := e.second()
	if err != nil {
		return err
	}
	mine, err := e.create(ctx, e.caller, e.manifest(e.name()))
	if err != nil {
		return err
	}
	x, err = other.get(ctx, "/v1/sandboxes/"+mine.Status.ID)
	if err != nil {
		return err
	}
	if err := x.refusal("not_found"); err != nil {
		return fmt.Errorf("reading another subject's object: %w", err)
	}
	return nil
}

// case006WorkloadTokenScope: the token a driver projects inside a sandbox
// authenticates for that sandbox, carries none of the control plane's own
// record of it, and reaches no other subject's object.
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
	other, err := e.second()
	if err != nil {
		return err
	}
	theirs, err := e.create(ctx, other, e.manifest(e.name()))
	if err != nil {
		return err
	}
	refused, err := workload.get(ctx, "/v1/sandboxes/"+theirs.Status.ID)
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
