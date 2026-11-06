// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package admission

import (
	"encoding/json/v2"
	"strings"
	"testing"

	"latere.ai/x/cella/manifest"
	v1 "latere.ai/x/cella/manifest/v1"
)

// TestEnvelopeBytesAreFixed: an endpoint this service does not own reads a
// fixed shape, so the absent members are null, the members of a map come in
// key order, and a retry sends the same bytes.
func TestEnvelopeBytesAreFixed(t *testing.T) {
	in := &v1.Sandbox{APIVersion: v1.APIVersion, Kind: v1.KindSandbox, Metadata: v1.Metadata{Name: "w"}}
	req := manifest.AdmitRequest{
		Action:    "create",
		RequestID: "req_1",
		Claims:    map[string]any{"z": 1, "a": []string{"x"}, "m": nil},
	}
	first, err := envelopeOf(in, req)
	if err != nil {
		t.Fatal(err)
	}
	again, err := envelopeOf(in, req)
	if err != nil {
		t.Fatal(err)
	}
	if string(first) != string(again) {
		t.Fatalf("two renderings differ:\n%s\n%s", first, again)
	}
	for _, want := range []string{
		`"claims":{"a":["x"],"m":null,"z":1}`,
		`"workload":null`, `"existing":null`, `"parent":null`, `"set":null`, `"volume":null`,
		`"environment":null`,
		`"request":{"id":"req_1"}`,
	} {
		if !strings.Contains(string(first), want) {
			t.Errorf("the envelope lacks %s:\n%s", want, first)
		}
	}
	if strings.Contains(string(first), "\n") {
		t.Errorf("the envelope is not one line: %q", first)
	}
}

// TestEnvelopeReplacesInvalidUTF8: a manifest string that is not valid UTF-8
// is written with the replacement character and the apply goes on.
func TestEnvelopeReplacesInvalidUTF8(t *testing.T) {
	in := &v1.Sandbox{APIVersion: v1.APIVersion, Kind: v1.KindSandbox, Metadata: v1.Metadata{Name: "w\xff"}}
	body, err := envelopeOf(in, manifest.AdmitRequest{Action: "create"})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(body), `"name":"w`+"\ufffd"+`"`) {
		t.Errorf("the name was not replaced:\n%s", body)
	}
}

// TestAnswerIsReadLeniently: an endpoint's answer with a duplicate member,
// a member in another letter case and a reason that is not valid UTF-8 is
// read the way it always was, the last duplicate winning.
func TestAnswerIsReadLeniently(t *testing.T) {
	raw := []byte(`{"Allow":true,"allow":false,"reason":"no ` + "\xff" + `","warnings":["w"]}`)
	var answer response
	if err := json.Unmarshal(raw, &answer, foreignJSON); err != nil {
		t.Fatal(err)
	}
	if answer.Allow == nil || *answer.Allow {
		t.Errorf("allow = %v, want false (the last duplicate)", answer.Allow)
	}
	if !strings.HasPrefix(answer.Reason, "no ") || len(answer.Warnings) != 1 {
		t.Errorf("answer = %+v", answer)
	}
}
