// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package k8s

import (
	"encoding/json/v2"
	"testing"

	corev1 "k8s.io/api/core/v1"

	driver "latere.ai/x/cella/runtime"
)

// TestPatchesToTheClusterWriteNull holds what the cluster's API reads: a nil
// list or map is null, and the members of a map come out in key order.
func TestPatchesToTheClusterWriteNull(t *testing.T) {
	got, err := json.Marshal(map[string]any{
		"spec": map[string]any{"containers": []string(nil), "labels": map[string]string(nil)},
		"b":    1, "a": 2,
	}, outboundJSON)
	if err != nil {
		t.Fatal(err)
	}
	if want := `{"a":2,"b":1,"spec":{"containers":null,"labels":null}}`; string(got) != want {
		t.Fatalf("patch is %s, want %s", got, want)
	}
	ops, err := json.Marshal([]patchOp{{Op: "remove", Path: "/metadata/annotations/x"}, {Op: "add", Path: "/a", Value: map[string]string{}}}, outboundJSON)
	if err != nil {
		t.Fatal(err)
	}
	if want := `[{"op":"remove","path":"/metadata/annotations/x"},{"op":"add","path":"/a","value":{}}]`; string(ops) != want {
		t.Fatalf("ops are %s, want %s", ops, want)
	}
}

// TestTheRecordIsTheSameBytesForTheSameSpec holds the spec annotation to one
// encoding of one value, whatever order its maps are visited in.
func TestTheRecordIsTheSameBytesForTheSameSpec(t *testing.T) {
	spec := driver.CreateSpec{ID: "sbx_1", Labels: map[string]string{"z": "1", "a": "2", "m": "3"}, Env: map[string]string{"k": "v"}}
	first, err := encodeRecord(spec)
	if err != nil {
		t.Fatal(err)
	}
	for range 16 {
		again, err := encodeRecord(spec)
		if err != nil {
			t.Fatal(err)
		}
		if again != first {
			t.Fatalf("record differs between encodings:\n%s\n%s", first, again)
		}
	}
	back, err := specOf(&corev1.PersistentVolumeClaim{Name: "c", Annotations: map[string]string{annSpec: first}})
	if err != nil {
		t.Fatal(err)
	}
	if back.ID != "sbx_1" || len(back.Env) != 0 {
		t.Fatalf("record read back as %+v", back)
	}
}

// TestRepliesOfTheClusterReadLeniently holds the reading of what the cluster
// answers: a member name in another letter case and a repeated member read.
func TestRepliesOfTheClusterReadLeniently(t *testing.T) {
	raw := []byte(`{"CONTAINERS":[{"name":"main","usage":{"memory":"1Ki"}}],"containers":[{"name":"main","usage":{"memory":"2Ki"}}],"timestamp":"2026-11-05T10:00:00Z"}`)
	used, _, err := readWorkingSet(raw, "main")
	if err != nil {
		t.Fatal(err)
	}
	if used != 2048 {
		t.Fatalf("working set read as %d, want the last repeated member's 2048", used)
	}
}

// TestAGuardedPatchPassesForARecordWithHTMLCharacters: the patch that writes
// a sandbox's record is guarded by a test operation on the record as read,
// and the cluster compares that value with its own encoding of the field,
// which escapes <, > and &. A sandbox whose command holds a redirection is
// stopped and started again; each write of its record is accepted.
func TestAGuardedPatchPassesForARecordWithHTMLCharacters(t *testing.T) {
	h := newHarness(t)
	const id = "sbx_redirect"
	s := spec(id)
	s.Command = []string{"/bin/sh", "-c", "echo a > b && cat < b"}
	h.created(t, s)
	if err := h.Stop(t.Context(), id); err != nil {
		t.Fatalf("stop: %v", err)
	}
	if err := h.Start(t.Context(), id); err != nil {
		t.Fatalf("start: %v", err)
	}
	ops, err := json.Marshal([]patchOp{{Op: "test", Path: "/a", Value: "a > b && cat < b"}}, outboundJSON)
	if err != nil {
		t.Fatal(err)
	}
	if want := `[{"op":"test","path":"/a","value":"a \u003e b \u0026\u0026 cat \u003c b"}]`; string(ops) != want {
		t.Fatalf("ops are %s, want %s", ops, want)
	}
}
