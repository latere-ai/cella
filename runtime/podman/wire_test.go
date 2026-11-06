// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package podman

import (
	"encoding/json/v2"
	"testing"
)

// TestRequestsToTheEngineWriteNull holds what the engine's API reads: a nil
// list or map is null, and the members of a map come out in key order.
func TestRequestsToTheEngineWriteNull(t *testing.T) {
	got, err := json.Marshal(struct {
		Command []string          `json:"command"`
		Env     map[string]string `json:"env"`
		Labels  map[string]string `json:"labels"`
	}{Labels: map[string]string{"b": "2", "a": "1"}}, outboundJSON)
	if err != nil {
		t.Fatal(err)
	}
	if want := `{"command":null,"env":null,"labels":{"a":"1","b":"2"}}`; string(got) != want {
		t.Fatalf("request is %s, want %s", got, want)
	}
	if got := labelKeyFilter("k"); got != `{"label":["k"]}` {
		t.Fatalf("filter is %s", got)
	}
}

// TestRepliesOfTheEngineReadLeniently holds the reading of what the engine
// answers: any letter case in a member name, a repeated member and a byte that
// is not UTF-8 are all read.
func TestRepliesOfTheEngineReadLeniently(t *testing.T) {
	var out struct {
		Digest string `json:"Digest"`
		Name   string `json:"name"`
	}
	body := `{"digest":"sha256:1","DIGEST":"sha256:2","name":"a` + "\xff" + `"}`
	if err := json.Unmarshal([]byte(body), &out, foreignJSON); err != nil {
		t.Fatal(err)
	}
	if out.Digest != "sha256:2" || out.Name != "a�" {
		t.Fatalf("reply read as %+v", out)
	}
}
