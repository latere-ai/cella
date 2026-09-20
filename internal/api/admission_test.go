// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"encoding/json"
	"errors"
	"slices"
	"strings"
	"testing"

	"latere.ai/x/pkg/authz"

	"latere.ai/x/cella/internal/auth"
	"latere.ai/x/cella/manifest"
	v1 "latere.ai/x/cella/manifest/v1"
)

// admitting points the fixture's handler at one admission step.
func admitting(f *fixture, admit manifest.AdmitFunc) { f.h.(*handler).Admit = admit }

// TestCreatePassesTheCallerToAdmission: the admission step of design 007
// is handed the verified caller, its claims, the action, the environment
// and the request id of the apply that drew it.
func TestCreatePassesTheCallerToAdmission(t *testing.T) {
	f := setup(t, nil)
	var seen manifest.AdmitRequest
	admitting(f, func(_ context.Context, in *v1.Sandbox, req manifest.AdmitRequest) (*v1.Sandbox, []string, error) {
		seen = req
		out := *in
		return &out, []string{"The policy had something to say."}, nil
	})
	var obj v1.Sandbox
	if err := json.Unmarshal(f.request("POST", "/v1/sandboxes", f.alice, createBody, 201), &obj); err != nil {
		t.Fatal(err)
	}
	if seen.Action != "create" || seen.Existing != nil || seen.Workload != nil {
		t.Fatalf("request = %+v", seen)
	}
	if seen.Actor.Sub != "alice" || seen.Actor.Issuer != f.issuerURL {
		t.Fatalf("actor = %+v, want the verified halves of the subject", seen.Actor)
	}
	if seen.Actor.Subject != authz.Subject(f.issuerURL, "alice") {
		t.Fatalf("subject = %q", seen.Actor.Subject)
	}
	if seen.Claims["sub"] != "alice" {
		t.Fatalf("claims = %v, want the token's own", seen.Claims)
	}
	if seen.RequestID == "" {
		t.Fatal("the admission step was told no request id")
	}
	if seen.Environment == nil || seen.Environment.Metadata.Name != "default" {
		t.Fatalf("environment = %+v", seen.Environment)
	}
	if !slices.Contains(obj.Status.Warnings, "The policy had something to say.") {
		t.Fatalf("warnings = %v", obj.Status.Warnings)
	}
}

// TestAdmissionRefusalAndOutageAreTheirOwnAnswers: a policy refusal is a
// 422 the caller can act on, and an endpoint that gave no decision is a
// 503 the caller retries, with the step's own detail on each.
func TestAdmissionRefusalAndOutageAreTheirOwnAnswers(t *testing.T) {
	for _, tc := range []struct {
		name   string
		err    error
		status int
		code   string
	}{
		{"refused", &manifest.Error{
			Code:   manifest.CodeAdmissionRefused,
			Detail: "ceiling_exceeded: spec.resources.cpu is 8, above the plan's 4",
		}, 422, "admission_refused"},
		{"unavailable", &manifest.Error{
			Code:   manifest.CodeAdmissionUnavailable,
			Detail: "the admission endpoint gave no decision: connection refused",
		}, 503, "admission_unavailable"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := setup(t, nil)
			admitting(f, func(context.Context, *v1.Sandbox, manifest.AdmitRequest) (*v1.Sandbox, []string, error) {
				return nil, nil, tc.err
			})
			body := f.request("POST", "/v1/sandboxes", f.alice, createBody, tc.status)
			var envelope struct {
				Error struct {
					Code    string         `json:"code"`
					Message string         `json:"message"`
					Details map[string]any `json:"details"`
				} `json:"error"`
			}
			if err := json.Unmarshal(body, &envelope); err != nil {
				t.Fatal(err)
			}
			if envelope.Error.Code != tc.code {
				t.Fatalf("code = %q, want %q", envelope.Error.Code, tc.code)
			}
			var known *manifest.Error
			if !errors.As(tc.err, &known) {
				t.Fatal("the case is not a manifest error")
			}
			if detail, _ := envelope.Error.Details["detail"].(string); !strings.Contains(detail, known.Detail) {
				t.Fatalf("detail = %q, want the step's own", detail)
			}
			// Nothing was created: a refusal and an outage both stop the
			// apply before the controller sees it.
			if len(f.c.List()) != 0 {
				t.Fatalf("the refused apply left %d sandboxes", len(f.c.List()))
			}
		})
	}
}

// TestAdmissionMutationIsWhatTheCallerReadsBack: what the step returns is
// the object the controller stores, so a caller reads back the labels and
// annotations a platform stamped.
func TestAdmissionMutationIsWhatTheCallerReadsBack(t *testing.T) {
	f := setup(t, nil)
	admitting(f, func(_ context.Context, in *v1.Sandbox, _ manifest.AdmitRequest) (*v1.Sandbox, []string, error) {
		out := *in
		out.Metadata.Annotations = map[string]string{"example.org/tier": "warm"}
		out.Spec.Lifecycle = v1.Lifecycle{TTL: "1h"}
		return &out, nil, nil
	})
	var obj v1.Sandbox
	if err := json.Unmarshal(f.request("POST", "/v1/sandboxes", f.alice, createBody, 201), &obj); err != nil {
		t.Fatal(err)
	}
	if obj.Metadata.Annotations["example.org/tier"] != "warm" || obj.Spec.Lifecycle.TTL != "1h" {
		t.Fatalf("object = %+v", obj)
	}
	var read v1.Sandbox
	if err := json.Unmarshal(f.request("GET", "/v1/sandboxes/"+obj.Status.ID, f.alice, "", 200), &read); err != nil {
		t.Fatal(err)
	}
	if read.Metadata.Annotations["example.org/tier"] != "warm" {
		t.Fatalf("read back = %+v", read.Metadata)
	}
}

// TestAdmissionSeesAWorkload: a sandbox applying through its own token is
// named as one, with its status, so an endpoint decides on a workload as a
// workload and not as the person who owns it.
func TestAdmissionSeesAWorkload(t *testing.T) {
	f := setup(t, allowAll{})
	signer := signing(t, f, rows{})
	var mine v1.Sandbox
	if err := json.Unmarshal(f.request("POST", "/v1/sandboxes", f.alice, createBody, 201), &mine); err != nil {
		t.Fatal(err)
	}
	token, err := signer.MintWorkload(auth.Workload{Sandbox: mine.Status.ID, Environment: "default"})
	if err != nil {
		t.Fatal(err)
	}
	var seen manifest.AdmitRequest
	admitting(f, func(_ context.Context, in *v1.Sandbox, req manifest.AdmitRequest) (*v1.Sandbox, []string, error) {
		seen = req
		out := *in
		return &out, nil, nil
	})
	child := strings.Replace(createBody, `"work"`, `"child"`, 1)
	f.request("POST", "/v1/sandboxes", token.Value, child, 201)
	if !seen.Actor.Workload || seen.Workload == nil || seen.Workload.ID != mine.Status.ID {
		t.Fatalf("request = %+v, workload = %+v", seen.Actor, seen.Workload)
	}
	// A workload whose own sandbox this node cannot read is refused rather
	// than decided on as if a person had applied.
	gone, err := signer.MintWorkload(auth.Workload{Sandbox: "sb_missing", Environment: "default"})
	if err != nil {
		t.Fatal(err)
	}
	f.request("POST", "/v1/sandboxes", gone.Value, strings.Replace(createBody, `"work"`, `"orphan"`, 1), 404)
}

// allowAll answers every question yes, for a case about what reaches the
// admission step rather than about who may ask.
type allowAll struct{}

func (allowAll) Authorize(context.Context, authz.Request) (authz.Decision, error) {
	return authz.Decision{Allow: true}, nil
}

// TestDefaultImageReachesTheResolver: the operator's CELLA_DEFAULT_IMAGE
// is passed through to the resolver, where the environment that runs no
// image still refuses one.
func TestDefaultImageReachesTheResolver(t *testing.T) {
	f := setup(t, nil)
	f.h.(*handler).Defaults = manifest.Defaults{Image: "registry.example/base:1"}
	// The native environment runs no image, so the default is not applied
	// and the create still passes.
	var obj v1.Sandbox
	if err := json.Unmarshal(f.request("POST", "/v1/sandboxes", f.alice, createBody, 201), &obj); err != nil {
		t.Fatal(err)
	}
	if obj.Spec.Image != "" {
		t.Fatalf("image = %q, want none on an environment that runs none", obj.Spec.Image)
	}
	// An admission step that names one on such an environment is refused.
	admitting(f, func(_ context.Context, in *v1.Sandbox, _ manifest.AdmitRequest) (*v1.Sandbox, []string, error) {
		out := *in
		out.Spec.Image = "registry.example/base:1"
		return &out, nil, nil
	})
	body := f.request("POST", "/v1/sandboxes", f.alice, strings.Replace(createBody, `"work"`, `"imaged"`, 1), 422)
	if !strings.Contains(string(body), "capability_unsupported") {
		t.Fatalf("body = %s", body)
	}
}

// TestCountCeilingCountsEveryDesiredSandbox is design 007's definition of
// the per-subject count: every desired sandbox of the subject whose phase
// is not Deleting, a stopped one included. The ceiling is the authorizer's
// limits.max_sandboxes, which means that same figure.
func TestCountCeilingCountsEveryDesiredSandbox(t *testing.T) {
	f := setup(t, limit(2))
	var first, second v1.Sandbox
	if err := json.Unmarshal(f.request("POST", "/v1/sandboxes", f.alice, createBody, 201), &first); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(f.request("POST", "/v1/sandboxes", f.alice,
		strings.Replace(createBody, `"work"`, `"second"`, 1), 201), &second); err != nil {
		t.Fatal(err)
	}
	third := strings.Replace(createBody, `"work"`, `"third"`, 1)
	body := f.request("POST", "/v1/sandboxes", f.alice, third, 422)
	if !strings.Contains(string(body), "quota_exceeded") {
		t.Fatalf("body = %s", body)
	}
	// A stopped sandbox holds a workspace and a name, so it counts.
	f.request("POST", "/v1/sandboxes/"+second.Status.ID+"/stop", f.alice, "", 200)
	f.request("POST", "/v1/sandboxes", f.alice, third, 422)
	// A delete frees the slot.
	f.request("DELETE", "/v1/sandboxes/"+second.Status.ID, f.alice, "", 202)
	f.request("POST", "/v1/sandboxes", f.alice, third, 201)
	// Another subject's sandboxes are not this subject's count.
	f.request("POST", "/v1/sandboxes", f.bob, createBody, 201)
}

// limit is an authorizer that allows everything and grants one ceiling.
type limit int

func (l limit) Authorize(_ context.Context, req authz.Request) (authz.Decision, error) {
	if req.Action != "sandbox.create" {
		return authz.Decision{Allow: true}, nil
	}
	max := int(l)
	raw, err := json.Marshal(map[string]any{"max_sandboxes": max})
	if err != nil {
		return authz.Decision{}, errors.New("limit: " + err.Error())
	}
	return authz.Decision{Allow: true, Limits: raw}, nil
}
