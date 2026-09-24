// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

// Package conformance is the API contract as executable cases: given a
// server URL, a way to mint tokens and what the environment declares, it
// runs one case per acceptance criterion the specs mark and reports which
// held, which failed with the exchange that disagreed, and which skipped
// and why.
//
// A case is a pure function of the configuration and the server. It reads
// the wire and not this repository's types, so a server built from the
// specs alone passes it. The one dependency beyond the standard library is
// the exec and attach socket client of the exported client package, because
// the framing of those two streams is the same client every caller speaks.
package conformance

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"slices"
	"sort"
	"strings"
	"time"
)

// Version is the suite's own version, carried in the report's marker beside
// the version the server reports. It moves when a case is added, removed or
// changed, so two reports are comparable only when it matches.
const Version = "1"

// CaseTimeout bounds one case. Every wait inside a case polls until this
// deadline and no case sleeps for a state.
const CaseTimeout = 2 * time.Minute

// Config is what a caller supplies. Every field but URL is optional; a case
// whose input is empty is reported skipped with the input named, never
// silently and never as a pass.
type Config struct {
	// URL is the server under test: its public URL, with the path it is
	// served under when it has one. Each case writes its routes as a server
	// at the root serves them, and a path here takes the place of /v1.
	URL string
	// Token mints a token for one subject. It is the stub issuer's mint
	// route in a tier and a caller's own issuer elsewhere. With none, Caller
	// is the only identity and the two-subject cases skip.
	Token func(ctx context.Context, subject string) (string, error)
	// Caller and Admin are a caller's token and an administrator's. Each is
	// minted through Token when empty.
	Caller string
	Admin  string

	// Image is what every case creates from. Empty creates from a command
	// alone, which is what an environment with no image driver takes.
	Image string
	// DisplayImage carries a desktop; empty skips the computer-use case.
	DisplayImage string
	// Upstream is the host a sandbox reaches through the gateway; empty
	// skips the egress substitution half.
	Upstream string
	// QueuedEnvironment is an environment that queues; empty skips sets.
	QueuedEnvironment string
	// WorkerEnvironment is an environment a worker serves; empty skips the
	// indistinguishability case.
	WorkerEnvironment string

	// AuthorizerControl and AdmissionControl are the control URLs of the
	// stubs beside the server: POST <control>/fail with {"mode": "..."}
	// puts the stub into that failure mode and an empty mode clears it.
	AuthorizerControl string
	AdmissionControl  string
	// SinkControl is where the event sink serves what it received, GET
	// <control>/events in sequence order per object.
	SinkControl string
	// Cella is the built agent binary; empty skips the agent case.
	Cella string

	// Capabilities is what the environment declares: attach, files, dial,
	// display, input, snapshots. A case for a capability outside this set is
	// skipped, and a case for one inside it must hold.
	Capabilities []string
	// Known is the cases this server declares it fails, each with the reason.
	// A declared failure is reported apart and does not fail the run; a
	// declared case that passes does, so a declaration cannot outlive the
	// gap it describes.
	Known map[string]string
	// Skip is group or case names to skip by request.
	Skip []string
	// Timeout bounds one case. Zero is CaseTimeout.
	Timeout time.Duration
}

// Status is what happened to one case.
type Status string

const (
	// StatusPassed is a case that held.
	StatusPassed Status = "pass"
	// StatusFailed is a disagreement between the server and the spec.
	StatusFailed Status = "fail"
	// StatusSkipped is a case whose capability or input the configuration
	// does not carry.
	StatusSkipped Status = "skip"
	// StatusKnown is a failure the server declared, with its reason.
	StatusKnown Status = "known"
)

// Result is one case's outcome.
type Result struct {
	Group   string
	Name    string
	Status  Status
	Reason  string
	Err     error
	Elapsed time.Duration
	// Created is every id the case made, which the case's end deletes.
	Created []string
}

// Subtest is the name Run gives this case, <NNN>/<Name>.
func (r Result) Subtest() string { return subtestName(r.Name) }

// Marker identifies a run: the version the server reports and the suite's
// own. A report without both is a report that cannot be compared.
type Marker struct {
	Server string
	Suite  string
}

// Report is what a run produced.
type Report struct {
	Marker  Marker
	Results []Result
	// Passed, Failed, Skipped and Known are case names, and Reasons carries
	// why each skipped or declared case is in its bucket.
	Passed  []string
	Failed  []string
	Skipped []string
	Known   []string
	Reasons map[string]string
	// Created is every object id the run made and deleted.
	Created []string
	// Undeclared is every declared case that passed: a declaration that no
	// longer describes a gap, which fails the run like a failure does.
	Undeclared []string
}

// OK reports whether the run may be called green: no failure and no
// declaration that has outlived its gap.
func (r Report) OK() bool { return len(r.Failed) == 0 && len(r.Undeclared) == 0 }

// String is the report a person reads: the marker, one line per case, and
// the disagreement under each failure.
func (r Report) String() string {
	var b strings.Builder
	fmt.Fprintf(&b, "conformance suite %s against server %s\n", r.Marker.Suite, cmp.Or(r.Marker.Server, "unknown"))
	group := ""
	for _, res := range r.Results {
		if res.Group != group {
			group = res.Group
			fmt.Fprintf(&b, "\n%s\n", group)
		}
		fmt.Fprintf(&b, "  %-4s %-34s %s", res.Status, res.Subtest(), res.Elapsed.Round(time.Millisecond))
		if res.Reason != "" {
			fmt.Fprintf(&b, "  %s", res.Reason)
		}
		b.WriteString("\n")
		if res.Err != nil && res.Status != StatusSkipped {
			for line := range strings.SplitSeq(strings.TrimRight(res.Err.Error(), "\n"), "\n") {
				fmt.Fprintf(&b, "       %s\n", line)
			}
		}
	}
	fmt.Fprintf(&b, "\n%d passed, %d failed, %d skipped, %d declared gap(s), %d object(s) created and deleted\n",
		len(r.Passed), len(r.Failed), len(r.Skipped), len(r.Known), len(r.Created))
	for _, name := range r.Undeclared {
		fmt.Fprintf(&b, "declared as failing but passed: %s\n", name)
	}
	return b.String()
}

// Skip is the error a case returns for a capability or an input the
// configuration does not carry.
type Skip struct{ Reason string }

func (s *Skip) Error() string { return "skipped: " + s.Reason }

// skipf builds a skip.
func skipf(format string, args ...any) error { return &Skip{Reason: fmt.Sprintf(format, args...)} }

// Disagreement is one exchange where the server and the spec differ: what
// was asked, what the spec says, and what arrived. It is what a failed case
// prints, so a reader sees the request and the response side by side.
type Disagreement struct {
	Method string
	Path   string
	Want   string
	Got    string
	Body   string
}

func (d *Disagreement) Error() string {
	var b strings.Builder
	fmt.Fprintf(&b, "%s %s\n  want: %s\n  got:  %s", d.Method, d.Path, d.Want, d.Got)
	if body := strings.TrimSpace(d.Body); body != "" {
		if len(body) > 800 {
			body = body[:800] + "..."
		}
		fmt.Fprintf(&b, "\n  body: %s", strings.ReplaceAll(body, "\n", " "))
	}
	return b.String()
}

// Case is one acceptance criterion as a function. Name is case<NNN><Name>,
// where NNN is the spec whose criterion it proves.
type Case struct {
	Group string
	Name  string
	Run   func(ctx context.Context, e *Env) error
}

// Group is one group of the suite with its cases in order.
type Group struct {
	Name  string
	Cases []Case
}

// Groups is every group in the order a run executes them. It is exported so
// a caller can list what would run without running it.
func Groups() []Group {
	return []Group{
		{"decode", decodeCases()},
		{"resolve", resolveCases()},
		{"lifecycle", lifecycleCases()},
		{"identity", identityCases()},
		{"list", listCases()},
		{"streams", streamCases()},
		{"errors", errorCases()},
		{"events", eventCases()},
		{"secrets", secretCases()},
		{"volumes", volumeCases()},
		{"sets", setCases()},
		{"environments", environmentCases()},
		{"egress", egressCases()},
		{"spawn", spawnCases()},
		{"agent", agentCases()},
		{"computer use", computerUseCases()},
		{"indistinguishability", indistinguishabilityCases()},
		{"capability", capabilityCases()},
	}
}

// Names is every case name the suite holds, sorted. The marker test of
// design 015 reads it.
func Names() []string {
	var out []string
	for _, g := range Groups() {
		for _, c := range g.Cases {
			out = append(out, c.Name)
		}
	}
	sort.Strings(out)
	return out
}

// Execute runs every case the configuration allows and returns what
// happened. It takes no testing.T: a case is a function of the server, and
// what a test framework does with the result is the caller's business.
func Execute(ctx context.Context, cfg Config) (Report, error) {
	e, err := newEnv(ctx, cfg)
	if err != nil {
		return Report{}, err
	}
	defer e.cleanup(context.WithoutCancel(ctx))
	report := Report{Marker: Marker{Server: e.serverVersion(ctx), Suite: Version}, Reasons: map[string]string{}}
	for _, group := range Groups() {
		for _, c := range group.Cases {
			report.Results = append(report.Results, e.runCase(ctx, group, c))
		}
	}
	for i := range report.Results {
		res := &report.Results[i]
		switch res.Status {
		case StatusPassed:
			report.Passed = append(report.Passed, res.Name)
			if reason, declared := cfg.Known[res.Name]; declared {
				report.Undeclared = append(report.Undeclared, res.Name)
				report.Reasons[res.Name] = reason
			}
		case StatusFailed:
			report.Failed = append(report.Failed, res.Name)
		case StatusSkipped:
			report.Skipped = append(report.Skipped, res.Name)
			report.Reasons[res.Name] = res.Reason
		case StatusKnown:
			report.Known = append(report.Known, res.Name)
			report.Reasons[res.Name] = res.Reason
		}
	}
	report.Created = e.created()
	return report, nil
}

// runCase runs one case under its own deadline and classifies the result. A
// declared failure becomes StatusKnown, which the run does not fail on.
func (e *Env) runCase(ctx context.Context, group Group, c Case) Result {
	res := Result{Group: group.Name, Name: c.Name, Status: StatusPassed}
	if reason, ok := e.skipped(group.Name, c.Name); ok {
		res.Status, res.Reason = StatusSkipped, reason
		return res
	}
	timeout := cmp.Or(e.cfg.Timeout, CaseTimeout)
	caseCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	mark := e.mark()
	started := time.Now()
	err := c.Run(caseCtx, e)
	res.Elapsed = time.Since(started)
	// What the case made is deleted as it ends, so the run holds one case's
	// objects at a time and not the sum of every case's: a server with a
	// per-subject limit or a cluster sized for a few sandboxes sees what one
	// case needs.
	res.Created = e.release(context.WithoutCancel(ctx), mark)
	var skip *Skip
	switch {
	case err == nil:
	case errors.As(err, &skip):
		res.Status, res.Reason = StatusSkipped, skip.Reason
	default:
		res.Err = err
		res.Status = StatusFailed
		if reason, declared := e.cfg.Known[c.Name]; declared {
			res.Status, res.Reason = StatusKnown, reason
		}
	}
	return res
}

// skipped reports a group or case the caller asked to skip.
func (e *Env) skipped(group, name string) (string, bool) {
	if slices.Contains(e.cfg.Skip, group) {
		return "the group is skipped by request", true
	}
	if slices.Contains(e.cfg.Skip, name) {
		return "the case is skipped by request", true
	}
	return "", false
}

// subtestName is a case's name as a subtest, <NNN>/<Name>: case008ExecWait
// runs as 008/ExecWait.
func subtestName(name string) string {
	rest, ok := strings.CutPrefix(name, "case")
	if !ok || len(rest) < 3 {
		return name
	}
	return rest[:3] + "/" + rest[3:]
}
