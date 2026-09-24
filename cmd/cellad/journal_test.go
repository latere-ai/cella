// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"bufio"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"latere.ai/x/pkg/authkit/issuertest"
)

// TestJournalRetentionEndToEnd is the memory journal's bound on a running
// node: with CELLA_JOURNAL_CAP at three, a sandbox that produced more records
// than that reads back its newest three through the feed, numbered as they
// were written.
func TestJournalRetentionEndToEnd(t *testing.T) {
	issuer := issuertest.New(t)
	base, _, _, stop := startServeWithLog(t, map[string]string{
		"CELLA_OIDC_ISSUERS":  issuer.URL(),
		"CELLA_OIDC_AUDIENCE": "cella,platform.example",
		"CELLA_DATA_DIR":      t.TempDir(),
		"CELLA_JOURNAL_CAP":   "3",
	})
	defer func() { stop() }()
	alice := issuer.Mint(issuertest.Claims{Sub: "alice", Aud: issuertest.StringList{"platform.example"}})
	request := func(method, path, body string, want int) []byte {
		t.Helper()
		req, err := http.NewRequestWithContext(t.Context(), method, base+path, strings.NewReader(body))
		if err != nil {
			t.Fatal(err)
		}
		req.Header.Set("Authorization", "Bearer "+alice)
		req.Header.Set("Content-Type", "application/json")
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = resp.Body.Close() }()
		data, err := io.ReadAll(resp.Body)
		if err != nil {
			t.Fatal(err)
		}
		if resp.StatusCode != want {
			t.Fatalf("%s %s: %d %s, want %d", method, path, resp.StatusCode, data, want)
		}
		return data
	}
	obj := create(t, base, alice, `{"apiVersion":"cella.latere.ai/v1beta1","kind":"Sandbox","metadata":{"name":"busy"},"spec":{}}`)
	for range 5 {
		request("POST", "/v1/sandboxes/"+obj.Status.ID+"/exec?wait=1", `{"command":["true"]}`, http.StatusOK)
	}
	var page struct {
		Items []struct {
			Seq  int64  `json:"seq"`
			Type string `json:"type"`
		} `json:"items"`
	}
	if err := json.Unmarshal(request("GET", "/v1/events?object="+obj.Status.ID+"&limit=50", "", http.StatusOK), &page); err != nil {
		t.Fatal(err)
	}
	if len(page.Items) != 3 {
		t.Fatalf("the feed holds %d records with a cap of three: %+v", len(page.Items), page.Items)
	}
	if page.Items[0].Seq <= 3 || page.Items[0].Seq-page.Items[2].Seq != 2 {
		t.Fatalf("the ring keeps %+v, want the three newest in sequence", page.Items)
	}
}

// TestFollowedFeedEndToEnd is design 009's following feed on a running node:
// a feed opened on a sandbox carries the record of an exec as it commits, and
// stopping the node ends the feed and the node without waiting out the grace
// period for a stream that would never end on its own.
func TestFollowedFeedEndToEnd(t *testing.T) {
	issuer := issuertest.New(t)
	base, _, _, stop := startServeWithLog(t, map[string]string{
		"CELLA_OIDC_ISSUERS":  issuer.URL(),
		"CELLA_OIDC_AUDIENCE": "cella,platform.example",
		"CELLA_DATA_DIR":      t.TempDir(),
	})
	stopped := false
	defer func() {
		if !stopped {
			stop()
		}
	}()
	alice := issuer.Mint(issuertest.Claims{Sub: "alice", Aud: issuertest.StringList{"platform.example"}})
	obj := create(t, base, alice, `{"apiVersion":"cella.latere.ai/v1beta1","kind":"Sandbox","metadata":{"name":"followed"},"spec":{}}`)

	req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, base+"/v1/events?follow=1&object="+obj.Status.ID, nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+alice)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK || resp.Header.Get("Content-Type") != "application/x-ndjson" {
		t.Fatalf("the follow answered %d %s", resp.StatusCode, resp.Header.Get("Content-Type"))
	}
	exec, err := http.NewRequestWithContext(t.Context(), http.MethodPost,
		base+"/v1/sandboxes/"+obj.Status.ID+"/exec?wait=1", strings.NewReader(`{"command":["true"]}`))
	if err != nil {
		t.Fatal(err)
	}
	exec.Header.Set("Authorization", "Bearer "+alice)
	exec.Header.Set("Content-Type", "application/json")
	answer, err := http.DefaultClient.Do(exec)
	if err != nil {
		t.Fatal(err)
	}
	_ = answer.Body.Close()
	if answer.StatusCode != http.StatusOK {
		t.Fatalf("the exec answered %d", answer.StatusCode)
	}
	lines := bufio.NewReader(resp.Body)
	for {
		line, err := lines.ReadString('\n')
		if err != nil {
			t.Fatalf("the feed ended before the exec's record: %v", err)
		}
		var record struct {
			Type   string `json:"type"`
			Object struct {
				ID string `json:"id"`
			} `json:"object"`
		}
		if strings.TrimSpace(line) == "" || json.Unmarshal([]byte(line), &record) != nil {
			continue
		}
		if record.Object.ID != obj.Status.ID {
			t.Fatalf("the feed of %s carries %s", obj.Status.ID, line)
		}
		if record.Type == "sandbox.exec" {
			break
		}
	}

	began := time.Now()
	code := make(chan int, 1)
	go func() { code <- stop() }()
	stopped = true
	rest, err := io.ReadAll(lines)
	if err != nil {
		t.Fatalf("the feed failed rather than ending at the stop: %v", err)
	}
	if strings.Contains(string(rest), `"error"`) {
		t.Errorf("the feed ended at the stop with %s", rest)
	}
	if got := <-code; got != 0 {
		t.Fatalf("the node stopped with %d", got)
	}
	if took := time.Since(began); took >= gracePeriod {
		t.Fatalf("the node took %s to stop with a feed open, which is the grace period of %s", took, gracePeriod)
	}
}
