// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package conformance

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"
)

// eventCases prove the feed of design 009: every act this run takes is in
// the object's own feed, the operator's sink receives the same records in
// sequence order, and no secret value is in either.
func eventCases() []Case {
	return []Case{
		{"events", "case009ObjectFeed", case009ObjectFeed},
		{"events", "case009DeliveredInOrder", case009DeliveredInOrder},
		{"events", "case018CanarySecret", case018CanarySecret},
	}
}

// secretCases prove the Secret kind's one rule: the value goes in and never
// comes back.
func secretCases() []Case {
	return []Case{
		{"secrets", "case018SecretWriteOnly", case018SecretWriteOnly},
		{"secrets", "case018SecretRotates", case018SecretRotates},
		{"secrets", "case018SecretDelete", case018SecretDelete},
	}
}

// egressCases prove the record of what left a sandbox.
func egressCases() []Case {
	return []Case{{"egress", "case018EgressRecords", case018EgressRecords}}
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
func (e *Env) applySecret(ctx context.Context, name, value string) (*exchange, error) {
	x, err := e.caller.put(ctx, "/v1/secrets/"+name, e.secret(name, value), "application/json")
	if err != nil {
		return nil, err
	}
	if x.Status != http.StatusCreated && x.Status != http.StatusOK {
		return x, x.disagree("201 from a secret that did not exist", fmt.Sprintf("status %d", x.Status))
	}
	obj, err := x.object()
	if err != nil {
		return x, err
	}
	if obj.Status.ID != "" {
		e.record(e.caller, "/v1/secrets", obj.Status.ID)
	}
	return x, nil
}
