// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package auth

import "time"

// Metrics is what the decision point counts: design 017's two rows that
// designs 006 and 007 own. The registry of design 017 satisfies it.
type Metrics interface {
	// Decision counts one decision by endpoint and outcome, and observes how
	// long it took. A decision the client served from its cache is counted
	// under the cached outcome and not timed.
	Decision(endpoint, outcome string, d time.Duration)
}

// The label values this package passes, fixed by design 017.
const (
	// MetricEndpointAuthorizer is the endpoint label of every authorization
	// decision, whether an operator's endpoint or the built-in owner policy
	// answered it: the question is the same and the seam is the same.
	MetricEndpointAuthorizer = "authorizer"
	// The outcomes of one decision. A refusal the endpoint made is a deny; a
	// call that produced no decision at all is unavailable, which is the
	// outcome an alert reads.
	MetricAllow       = "allow"
	MetricDeny        = "deny"
	MetricUnavailable = "unavailable"
)

// nopMetrics is what an authorizer built with no recorder counts.
type nopMetrics struct{}

func (nopMetrics) Decision(string, string, time.Duration) {}

// outcomeOf maps one decision's error to design 017's outcome.
func outcomeOf(err error) string {
	switch {
	case err == nil:
		return MetricAllow
	case CodeOf(err) == CodeAuthorizerUnavailable:
		return MetricUnavailable
	default:
		return MetricDeny
	}
}
