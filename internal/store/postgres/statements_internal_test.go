// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package postgres

import (
	"encoding/json/v2"
	"testing"

	"latere.ai/x/cella/runtime"
)

// TestObservedStateTakesForeignText: a state whose text came from a
// container's labels or a driver's error is stored with the bytes that are
// not valid UTF-8 replaced, and its durations stay integer nanoseconds.
func TestObservedStateTakesForeignText(t *testing.T) {
	s := runtime.State{ID: "sbx_1", Reason: "bad\xff", Labels: map[string]string{"k\xff": "v\xfe"}, AutoStop: 5}
	got, err := json.Marshal(validUTF8State(s), json.Deterministic(true))
	if err != nil {
		t.Fatal(err)
	}
	var back runtime.State
	if err := json.Unmarshal(got, &back); err != nil {
		t.Fatal(err)
	}
	if back.Reason != "bad�" || back.Labels["k�"] != "v�" || back.AutoStop != 5 {
		t.Errorf("the state came back as %+v from %s", back, got)
	}
}

// TestLabelsTextTakesForeignText: the labels column is written for a label
// that is not valid UTF-8, with the bad bytes replaced.
func TestLabelsTextTakesForeignText(t *testing.T) {
	got, err := labelsText(map[string]string{"b": "2", "a": "x\xff"})
	if err != nil {
		t.Fatal(err)
	}
	if want := "{\"a\":\"x�\",\"b\":\"2\"}"; got != want {
		t.Errorf("labels = %s, want %s", got, want)
	}
}
