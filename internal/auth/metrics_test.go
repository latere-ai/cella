// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package auth_test

import (
	"context"
	"errors"
	"slices"
	"sync"
	"testing"
	"time"

	"latere.ai/x/pkg/authz"

	"latere.ai/x/cella/internal/auth"
)

// decisionRecorder is design 017's seam under test.
type decisionRecorder struct {
	mu   sync.Mutex
	made [][2]string
	seen int
}

func (r *decisionRecorder) Decision(endpoint, outcome string, _ time.Duration) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.made = append(r.made, [2]string{endpoint, outcome})
	r.seen++
}

func (r *decisionRecorder) read() [][2]string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return slices.Clone(r.made)
}

// answering is an authorizer that gives the answer the case asks for.
type answering struct {
	decision authz.Decision
	err      error
}

func (a answering) Authorize(context.Context, authz.Request) (authz.Decision, error) {
	return a.decision, a.err
}

// TestDecisionsAreCounted is design 017's decision counter: every question
// this control plane asks is counted under the endpoint that answered it and
// the outcome it produced.
func TestDecisionsAreCounted(t *testing.T) {
	res := (auth.Sandbox{ID: "sbx_1", Owner: alice}).Resource()
	for _, tc := range []struct {
		name  string
		inner authz.Authorizer
		want  string
	}{
		{"allow", answering{decision: authz.Decision{Allow: true}}, auth.MetricAllow},
		{"deny", answering{decision: authz.Decision{Reason: "no"}}, auth.MetricDeny},
		{"unavailable", answering{err: errors.New("the endpoint did not answer")}, auth.MetricUnavailable},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rec := &decisionRecorder{}
			a := auth.NewAuthorizer(tc.inner).Measure(rec)
			_, _ = a.Decide(t.Context(), caller(alice, "alice", nil), info, "sandboxes.read", res)
			want := [][2]string{{auth.MetricEndpointAuthorizer, tc.want}}
			if got := rec.read(); !slices.Equal(got, want) {
				t.Errorf("counted %v, want %v", got, want)
			}
		})
	}
}

// TestLookupIsCountedToo is the second question a resolve asks: it runs
// through the same seam, so it is counted on the same series.
func TestLookupIsCountedToo(t *testing.T) {
	rec := &decisionRecorder{}
	a := auth.NewAuthorizer(answering{decision: authz.Decision{Allow: true}}).Measure(rec)
	res := (auth.Sandbox{ID: "sbx_1", Owner: alice}).Resource()
	if _, err := a.Lookup(t.Context(), caller(alice, "alice", nil), info, "secrets.use", res); err != nil {
		t.Fatal(err)
	}
	if got := rec.read(); !slices.Equal(got, [][2]string{{auth.MetricEndpointAuthorizer, auth.MetricAllow}}) {
		t.Errorf("counted %v, want one allow", got)
	}
}

// TestNoRecorderDecidesTheSame is the default: an authorizer built without a
// recorder answers as it did before design 017, and Measure with nil keeps it.
func TestNoRecorderDecidesTheSame(t *testing.T) {
	a := auth.NewAuthorizer(answering{decision: authz.Decision{Allow: true}}).Measure(nil)
	res := (auth.Sandbox{ID: "sbx_1", Owner: alice}).Resource()
	if _, err := a.Decide(t.Context(), caller(alice, "alice", nil), info, "sandboxes.read", res); err != nil {
		t.Fatal(err)
	}
}
