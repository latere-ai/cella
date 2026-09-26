// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package conformance

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"time"
)

// eventCases prove the feed of design 009: every act this run takes is in
// the object's own feed, the operator's sink receives the same records in
// sequence order, and no secret value is in either.
func eventCases() []Case {
	return []Case{
		{"events", "case009ObjectFeed", case009ObjectFeed},
		{"events", "case009FollowFeed", case009FollowFeed},
		{"events", "case009DeliveredInOrder", case009DeliveredInOrder},
		{"events", "case018CanarySecret", case018CanarySecret},
	}
}

// secretCases prove the Secret kind's one rule, that the value goes in and
// never comes back, and the owner selector of its list.
func secretCases() []Case {
	return []Case{
		{"secrets", "case018SecretWriteOnly", case018SecretWriteOnly},
		{"secrets", "case018SecretRotates", case018SecretRotates},
		{"secrets", "case018SecretDelete", case018SecretDelete},
		{"secrets", "case008SecretListOwner", case008SecretListOwner},
	}
}

// egressCases prove the record of what left a sandbox, and, where the
// environment declares egress, that nothing left it but through its
// gateway.
func egressCases() []Case {
	return []Case{
		{"egress", "case018EgressRecords", case018EgressRecords},
		{"egress", "case018EgressEnforced", case018EgressEnforced},
	}
}

// record is one event as design 009 puts it on the wire.
type record struct {
	ID     string `json:"id"`
	Seq    int64  `json:"seq"`
	Type   string `json:"type"`
	Object struct {
		ID   string `json:"id"`
		Kind string `json:"kind"`
	} `json:"object"`
}

// case009ObjectFeed: every mutation and operation the run performs is in the
// object's feed, newest first.
func case009ObjectFeed(ctx context.Context, e *Env) error {
	obj, err := e.sandbox(ctx, e.caller)
	if err != nil {
		return err
	}
	if _, _, err := e.exec(ctx, e.caller, obj.Status.ID, "/bin/sh", "-c", "true"); err != nil {
		return err
	}
	x, err := e.caller.get(ctx, "/v1/events?object="+obj.Status.ID+"&limit=50")
	if err != nil {
		return err
	}
	if err := x.status(http.StatusOK); err != nil {
		return err
	}
	var page struct {
		Items []record `json:"items"`
		Next  string   `json:"next"`
	}
	if err := json.Unmarshal(x.Body, &page); err != nil {
		return x.disagree("a page of records with items and next", err.Error())
	}
	if len(page.Items) == 0 {
		return x.disagree("the records of a sandbox that was created and exec'd", "an empty page")
	}
	for i := 1; i < len(page.Items); i++ {
		if page.Items[i-1].Seq < page.Items[i].Seq {
			return x.disagree("records newest first", fmt.Sprintf("seq %d before seq %d", page.Items[i-1].Seq, page.Items[i].Seq))
		}
	}
	for _, item := range page.Items {
		if item.Object.ID != obj.Status.ID {
			return x.disagree("only the records of the object the query names", "a record of "+item.Object.ID)
		}
	}
	return nil
}

// case009FollowFeed: follow=1 on an object answers newline-delimited JSON,
// replays the records after the cursor oldest first, and then carries the
// record of an exec committed after the feed opened, while it is still open.
// The cursor is the newest seq the caller holds, so a cursor one below the
// newest replays the newest.
func case009FollowFeed(ctx context.Context, e *Env) error {
	obj, err := e.sandbox(ctx, e.caller)
	if err != nil {
		return err
	}
	x, err := e.caller.get(ctx, "/v1/events?object="+obj.Status.ID+"&limit=1")
	if err != nil {
		return err
	}
	if err := x.status(http.StatusOK); err != nil {
		return err
	}
	var page struct {
		Items []record `json:"items"`
	}
	if err := json.Unmarshal(x.Body, &page); err != nil || len(page.Items) == 0 {
		return x.disagree("a page holding the newest record of a created sandbox", string(x.Body))
	}
	newest := page.Items[0].Seq
	follow, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	path := fmt.Sprintf("/v1/events?follow=1&object=%s&cursor=%d", obj.Status.ID, newest-1)
	resp, err := e.caller.open(follow, http.MethodGet, path, request{})
	if err != nil {
		return fmt.Errorf("following the feed: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("GET %s answered %d, want 200", path, resp.StatusCode)
	}
	if got := resp.Header.Get("Content-Type"); !strings.HasPrefix(got, "application/x-ndjson") {
		return fmt.Errorf("GET %s answered Content-Type %q, want application/x-ndjson", path, got)
	}
	lines := bufio.NewReader(resp.Body)
	next := func() (record, error) {
		for {
			line, err := lines.ReadString('\n')
			if err != nil {
				return record{}, fmt.Errorf("the followed feed ended before a record: %w", err)
			}
			if strings.TrimSpace(line) == "" {
				continue
			}
			var r record
			if err := json.Unmarshal([]byte(line), &r); err != nil || r.ID == "" {
				return record{}, fmt.Errorf("the followed feed wrote %q where a record was due", line)
			}
			if r.Object.ID != obj.Status.ID {
				return record{}, fmt.Errorf("the feed of %s carries a record of %s", obj.Status.ID, r.Object.ID)
			}
			return r, nil
		}
	}
	first, err := next()
	if err != nil {
		return err
	}
	if first.Seq != newest {
		return fmt.Errorf("a feed from cursor %d opened with seq %d, want the newest, %d", newest-1, first.Seq, newest)
	}
	if _, _, err := e.exec(ctx, e.caller, obj.Status.ID, "/bin/sh", "-c", "true"); err != nil {
		return err
	}
	last := first.Seq
	for {
		r, err := next()
		if err != nil {
			return err
		}
		if r.Seq <= last {
			return fmt.Errorf("the followed feed sent seq %d after %d", r.Seq, last)
		}
		last = r.Seq
		if r.Type == "sandbox.exec" {
			return nil
		}
	}
}

// case009DeliveredInOrder: the operator's sink receives the records of one
// object in sequence order, and the delivery carries the create before the
// operation that followed it.
func case009DeliveredInOrder(ctx context.Context, e *Env) error {
	if e.cfg.SinkControl == "" {
		return skipf("no sink control URL: set SinkControl to read what the server delivered")
	}
	obj, err := e.sandbox(ctx, e.caller)
	if err != nil {
		return err
	}
	if _, _, err := e.exec(ctx, e.caller, obj.Status.ID, "/bin/sh", "-c", "true"); err != nil {
		return err
	}
	sink := newClient(strings.TrimSuffix(e.cfg.SinkControl, "/"), "")
	deadline := time.Now().Add(30 * time.Second)
	for {
		x, err := sink.send(ctx, http.MethodGet, "/events?object="+obj.Status.ID, request{NoBearer: true})
		if err != nil {
			return err
		}
		if err := x.status(http.StatusOK); err != nil {
			return err
		}
		var records []record
		if err := json.Unmarshal(x.Body, &records); err != nil {
			return x.disagree("the records the sink holds", err.Error())
		}
		types := map[string]bool{}
		ordered := true
		for i, item := range records {
			types[item.Type] = true
			if i > 0 && records[i-1].Seq > item.Seq {
				ordered = false
			}
			if item.Seq == 0 {
				return x.disagree("a sequence number on every record", "a record with none: "+item.Type)
			}
		}
		if !ordered {
			return x.disagree("the records in sequence order", fmt.Sprintf("%d records out of order", len(records)))
		}
		if types["sandbox.created"] && types["sandbox.exec"] {
			return nil
		}
		if time.Now().After(deadline) {
			return x.disagree("the create and the exec of this run's sandbox", fmt.Sprintf("%v", keys(types)))
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(pollInterval):
		}
	}
}

// case018CanarySecret: a value written into a secret appears in no answer
// the API gives and in no record the sink receives.
func case018CanarySecret(ctx context.Context, e *Env) error {
	canary := "canary-" + e.run + "-value"
	name := e.name()
	if _, err := e.applySecret(ctx, name, canary); err != nil {
		return err
	}
	paths := []string{
		"/v1/secrets/" + name,
		"/v1/secrets?limit=200",
		"/v1/sandboxes?limit=200",
	}
	for _, path := range paths {
		x, err := e.caller.get(ctx, path)
		if err != nil {
			return err
		}
		if strings.Contains(string(x.Body), canary) {
			return x.disagree("no secret value in an answer", "the value this run wrote")
		}
	}
	if e.cfg.SinkControl == "" {
		return nil
	}
	sink := newClient(strings.TrimSuffix(e.cfg.SinkControl, "/"), "")
	x, err := sink.send(ctx, http.MethodGet, "/events", request{NoBearer: true})
	if err != nil {
		return err
	}
	if strings.Contains(string(x.Body), canary) {
		return x.disagree("no secret value in a delivered record", "the value this run wrote")
	}
	return nil
}

// case018SecretWriteOnly: a created secret answers without its value, and so
// does every read and every list.
func case018SecretWriteOnly(ctx context.Context, e *Env) error {
	value := "write-only-" + e.run
	name := e.name()
	x, err := e.applySecret(ctx, name, value)
	if err != nil {
		return err
	}
	if strings.Contains(string(x.Body), value) {
		return x.disagree("no value in the answer to an apply", "the value that was written")
	}
	for _, path := range []string{"/v1/secrets/" + name, "/v1/secrets?limit=200"} {
		read, err := e.caller.get(ctx, path)
		if err != nil {
			return err
		}
		if err := read.status(http.StatusOK); err != nil {
			return err
		}
		if strings.Contains(string(read.Body), value) {
			return read.disagree("no value in a read", "the value that was written")
		}
	}
	return nil
}

// case018SecretRotates: a second apply replaces the value in place, which is
// an update of the same object and not a second one.
func case018SecretRotates(ctx context.Context, e *Env) error {
	name := e.name()
	x, err := e.applySecret(ctx, name, "first-"+e.run)
	if err != nil {
		return err
	}
	before, err := x.object()
	if err != nil {
		return err
	}
	rotated := "second-" + e.run
	x, err = e.caller.put(ctx, "/v1/secrets/"+name, e.secret(name, rotated), "application/json")
	if err != nil {
		return err
	}
	if err := x.status(http.StatusOK); err != nil {
		return err
	}
	after, err := x.object()
	if err != nil {
		return err
	}
	if after.Status.ID != before.Status.ID {
		return x.disagree("the same object under a rotation, "+before.Status.ID, "the object "+after.Status.ID)
	}
	if strings.Contains(string(x.Body), rotated) {
		return x.disagree("no value in the answer to a rotation", "the value that was written")
	}
	return nil
}

// case018SecretDelete: a deleted secret is gone, and a read of it is the
// refusal every absent object gets.
func case018SecretDelete(ctx context.Context, e *Env) error {
	name := e.name()
	if _, err := e.applySecret(ctx, name, "deleted-"+e.run); err != nil {
		return err
	}
	x, err := e.caller.del(ctx, "/v1/secrets/"+name)
	if err != nil {
		return err
	}
	if x.Status != http.StatusOK && x.Status != http.StatusNoContent && x.Status != http.StatusAccepted {
		return x.disagree("200, 202 or 204 from a delete", fmt.Sprintf("status %d", x.Status))
	}
	read, err := e.caller.get(ctx, "/v1/secrets/"+name)
	if err != nil {
		return err
	}
	return read.refusal("not_found")
}

// case008SecretListOwner: ?owner= narrows the secret list to one owner and
// never widens it. Every row a selected page carries has that owner, the
// caller's own secret is under its own owner, an owner nobody is answers an
// empty page and not a refusal, and with a second subject the caller's page
// under that subject's owner holds its secret only where the caller's
// unselected list holds it too. Which of another subject's secrets a caller
// may read is the authorizer's, so the case asserts the intersection and not
// an answer.
func case008SecretListOwner(ctx context.Context, e *Env) error {
	x, err := e.applySecret(ctx, e.name(), "owned-"+e.run)
	if err != nil {
		return err
	}
	own, err := x.object()
	if err != nil {
		return err
	}
	if own.Status.Owner == "" {
		return x.disagree("status.owner on a created secret", "none")
	}
	mine, err := secretPages(ctx, e.caller, own.Status.Owner)
	if err != nil {
		return err
	}
	if !slices.ContainsFunc(mine, func(o object) bool { return o.Status.ID == own.Status.ID }) {
		return fmt.Errorf("the list under the caller's own owner %q does not hold %s, which this run created", own.Status.Owner, own.Status.ID)
	}
	none, err := secretPages(ctx, e.caller, "nobody-"+e.run)
	if err != nil {
		return err
	}
	if len(none) != 0 {
		return fmt.Errorf("the list under an owner nobody is holds %d secrets", len(none))
	}
	// The narrowing half holds without a second subject; the intersection
	// half needs one, so a run that mints none ends here with the half it
	// proved.
	other := e.other
	if other == nil {
		return nil
	}
	x, err = e.applySecretAs(ctx, other, e.name(), "theirs-"+e.run)
	if err != nil {
		return err
	}
	theirs, err := x.object()
	if err != nil {
		return err
	}
	selected, err := secretPages(ctx, e.caller, theirs.Status.Owner)
	if err != nil {
		return err
	}
	unselected, err := secretPages(ctx, e.caller, "")
	if err != nil {
		return err
	}
	holds := func(page []object) bool {
		return slices.ContainsFunc(page, func(o object) bool { return o.Status.ID == theirs.Status.ID })
	}
	if holds(selected) && !holds(unselected) {
		return fmt.Errorf("the list under another subject's owner holds %s, which the caller's own list does not", theirs.Status.ID)
	}
	return nil
}

// secretPages follows the secret list to its end under one owner, or under
// none when owner is empty, and fails on a row whose owner is not the one
// named. It stops at 50 pages, past which a list that never ends is the
// finding.
func secretPages(ctx context.Context, c *client, owner string) ([]object, error) {
	var out []object
	cursor := ""
	for range 50 {
		q := url.Values{"limit": {"200"}}
		if owner != "" {
			q.Set("owner", owner)
		}
		if cursor != "" {
			q.Set("cursor", cursor)
		}
		x, err := c.get(ctx, "/v1/secrets?"+q.Encode())
		if err != nil {
			return nil, err
		}
		if err := x.status(http.StatusOK); err != nil {
			return nil, err
		}
		var page listEnvelope
		if err := json.Unmarshal(x.Body, &page); err != nil {
			return nil, x.disagree("a list envelope with items and next", "a body that does not decode: "+err.Error())
		}
		for _, item := range page.Items {
			if owner != "" && item.Status.Owner != owner {
				return nil, x.disagree("only rows whose status.owner is "+owner, "a row of "+item.Status.Owner)
			}
		}
		out = append(out, page.Items...)
		if page.Next == "" {
			return out, nil
		}
		cursor = page.Next
	}
	return nil, fmt.Errorf("the secret list did not end within 50 pages")
}

// case018EgressRecords: the connections the gateway reported for a sandbox
// are a list a caller reads, empty where nothing left.
func case018EgressRecords(ctx context.Context, e *Env) error {
	obj, err := e.sandbox(ctx, e.caller)
	if err != nil {
		return err
	}
	x, err := e.caller.get(ctx, "/v1/sandboxes/"+obj.Status.ID+"/egress?limit=10")
	if err != nil {
		return err
	}
	if err := x.status(http.StatusOK); err != nil {
		return err
	}
	var page struct {
		Items []json.RawMessage `json:"items"`
	}
	if err := json.Unmarshal(x.Body, &page); err != nil {
		return x.disagree("a list of connection records", err.Error())
	}
	if page.Items == nil {
		return x.disagree("items as a list, empty where nothing left the sandbox", "no items member")
	}
	return nil
}

// deniedHost is the host the enforcement case asks for off the allow list.
// It is an example name: the gateway refuses it before any dial, so it needs
// no address.
const deniedHost = "denied.example.com"

// egressProbe runs inside the sandbox with the upstream as $1 and the denied
// host as $2, and prints three lines: the gateway's status line to a CONNECT
// through the sandbox's own proxy door for each host, and how many bytes a
// request sent straight to the upstream got back. The proxy door and the
// credential are read off HTTPS_PROXY, as any client that honors it reads
// them. It needs a shell, netcat and base64, which is what a busybox image
// has, and says which it lacks otherwise.
const egressProbe = `up=$1 denied=$2
for tool in nc base64; do
  command -v "$tool" >/dev/null 2>&1 || { echo "missing $tool"; exit 3; }
done
[ -n "$HTTPS_PROXY" ] || { echo "missing HTTPS_PROXY"; exit 3; }
door=${HTTPS_PROXY#*://}
auth=${door%@*}
door=${door##*@}
door=${door%/}
basic=$(printf %s "$auth" | base64 | tr -d '\n')
connect() {
  { printf 'CONNECT %s HTTP/1.1\r\nHost: %s\r\nProxy-Authorization: Basic %s\r\nConnection: close\r\n\r\n' "$1" "$1" "$basic"; sleep 2; } |
    nc -w 5 "${door%:*}" "${door##*:}" 2>/dev/null | head -n 1 | tr -d '\r'
}
case $up in *:*) host=${up%:*} port=${up##*:} ;; *) host=$up port=443 ;; esac
echo "allowed $(connect "$host:$port")"
echo "denied $(connect "$denied:443")"
echo "direct $({ printf 'GET / HTTP/1.0\r\n\r\n'; sleep 2; } | nc -w 5 "$host" "$port" 2>/dev/null | wc -c | tr -d ' ')"
`

// case018EgressEnforced: where the environment declares egress, a sandbox
// whose allow list names the upstream reports the boundary enforced; from
// inside it, a CONNECT through its own proxy door reaches the upstream, one
// to a host off the list is refused, a request sent straight to the upstream
// gets nothing back, and the refused connection is in its records.
func case018EgressEnforced(ctx context.Context, e *Env) error {
	if err := e.need("egress"); err != nil {
		return err
	}
	upstream := strings.TrimSpace(e.cfg.Upstream)
	if upstream == "" {
		return skipf("no upstream: set Upstream to a host:port the sandbox may reach")
	}
	host := upstream
	if h, _, err := net.SplitHostPort(upstream); err == nil {
		host = h
	}
	obj, err := e.sandbox(ctx, e.caller, func(body map[string]any) {
		spec, _ := body["spec"].(map[string]any)
		spec["network"] = map[string]any{"egress": map[string]any{"mode": "allowlist", "allowedHosts": []string{host}}}
	})
	if err != nil {
		return err
	}
	x, err := e.caller.get(ctx, "/v1/sandboxes/"+obj.Status.ID)
	if err != nil {
		return err
	}
	if err := x.status(http.StatusOK); err != nil {
		return err
	}
	var read struct {
		Status struct {
			Conditions []struct {
				Type   string `json:"type"`
				Status string `json:"status"`
				Reason string `json:"reason"`
			} `json:"conditions"`
		} `json:"status"`
	}
	if err := json.Unmarshal(x.Body, &read); err != nil {
		return x.disagree("a sandbox with its conditions", err.Error())
	}
	enforced := false
	for _, c := range read.Status.Conditions {
		enforced = enforced || (c.Type == "EgressEnforced" && c.Status == "True")
	}
	if !enforced {
		return x.disagree("EgressEnforced True on an environment that declares egress", "the conditions "+string(x.Body))
	}
	result, x, err := e.exec(ctx, e.caller, obj.Status.ID, "/bin/sh", "-c", egressProbe, "probe", upstream, deniedHost)
	if err != nil {
		return err
	}
	if strings.HasPrefix(result.Stdout, "missing ") {
		return skipf("the sandbox's image cannot run the probe: %s", strings.TrimSpace(result.Stdout))
	}
	answers := map[string]string{}
	for line := range strings.SplitSeq(result.Stdout, "\n") {
		if key, value, ok := strings.Cut(line, " "); ok {
			answers[key] = value
		}
	}
	switch {
	case !strings.Contains(answers["allowed"], " 200"):
		return x.disagree("200 from the gateway to a CONNECT toward "+upstream+", which the allow list names", "the answers "+result.Stdout)
	case !strings.Contains(answers["denied"], " 403"):
		return x.disagree("403 from the gateway to a CONNECT toward "+deniedHost+", which the allow list does not name", "the answers "+result.Stdout)
	case answers["direct"] != "0":
		return x.disagree("nothing back from a request sent to "+upstream+" around the gateway", "the answers "+result.Stdout)
	}
	return e.awaitRecord(ctx, obj.Status.ID, deniedHost, "denied")
}

// awaitRecord polls a sandbox's connection records until one names the host
// with the decision. The gateway reports on its own stream, so a record
// arrives a moment after the connection it describes.
func (e *Env) awaitRecord(ctx context.Context, id, host, decision string) error {
	for {
		x, err := e.caller.get(ctx, "/v1/sandboxes/"+id+"/egress?limit=50")
		if err != nil {
			return err
		}
		if err := x.status(http.StatusOK); err != nil {
			return err
		}
		var page struct {
			Items []struct {
				Host     string `json:"host"`
				Decision string `json:"decision"`
			} `json:"items"`
		}
		if err := json.Unmarshal(x.Body, &page); err != nil {
			return x.disagree("a list of connection records", err.Error())
		}
		for _, r := range page.Items {
			if r.Host == host && r.Decision == decision {
				return nil
			}
		}
		select {
		case <-ctx.Done():
			return x.disagree("a record of the connection to "+host+" as "+decision, "the records "+string(x.Body))
		case <-time.After(pollInterval):
		}
	}
}

// secret is the body of one Secret apply: the smallest manifest the kind
// takes, which names the hosts its value may be sent to.
func (e *Env) secret(name, value string) []byte {
	body := map[string]any{
		"apiVersion": APIVersion,
		"kind":       "Secret",
		"metadata":   map[string]any{"name": name},
		"spec": map[string]any{
			"scope": map[string]any{"hosts": []string{"api.example.com"}},
			"value": value,
		},
	}
	out, err := json.Marshal(body)
	if err != nil {
		panic("the suite built a secret it cannot encode: " + err.Error())
	}
	return out
}

// applySecret writes one secret and records it for the run's cleanup.
//
// A server that answers capability_unsupported is one whose installation
// holds no key to seal a value under, which design 018 allows: the case is
// skipped with what the server said, and not failed. Every other refusal is
// a disagreement.
func (e *Env) applySecret(ctx context.Context, name, value string) (*exchange, error) {
	return e.applySecretAs(ctx, e.caller, name, value)
}

// applySecretAs is applySecret as one identity of the run.
func (e *Env) applySecretAs(ctx context.Context, c *client, name, value string) (*exchange, error) {
	x, err := c.put(ctx, "/v1/secrets/"+name, e.secret(name, value), "application/json")
	if err != nil {
		return nil, err
	}
	if x.Status == http.StatusUnprocessableEntity {
		var env envelope
		if json.Unmarshal(x.Body, &env) == nil && env.Error.Code == "capability_unsupported" {
			detail, _ := env.Error.Details["detail"].(string)
			return x, skipf("this server stores no secret value: %s", cmpOr(detail, env.Error.Message))
		}
	}
	if x.Status != http.StatusCreated && x.Status != http.StatusOK {
		return x, x.disagree("201 from a secret that did not exist", fmt.Sprintf("status %d", x.Status))
	}
	obj, err := x.object()
	if err != nil {
		return x, err
	}
	if obj.Status.ID != "" {
		e.record(c, "/v1/secrets", obj.Status.ID)
	}
	return x, nil
}
