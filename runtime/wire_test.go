// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package runtime_test

import (
	"encoding/json/v2"
	"testing"
	"time"

	runtime "latere.ai/x/cella/runtime"
)

// TestLifecycleWire holds the durations of a lifecycle to integer
// nanoseconds, the form every reader of the wire decodes.
func TestLifecycleWire(t *testing.T) {
	l := runtime.Lifecycle{TTL: time.Hour, AutoStop: 15 * time.Minute}
	got, err := json.Marshal(l)
	if err != nil {
		t.Fatal(err)
	}
	if want := `{"ttl":3600000000000,"autoStop":900000000000}`; string(got) != want {
		t.Fatalf("lifecycle is %s, want %s", got, want)
	}
	var back runtime.Lifecycle
	if err := json.Unmarshal([]byte(`{"ttl":3600000000000,"autoDelete":5}`), &back); err != nil {
		t.Fatal(err)
	}
	if back.TTL != time.Hour || back.AutoDelete != 5 || back.AutoStop != 0 {
		t.Fatalf("lifecycle read back as %+v", back)
	}
	empty, err := json.Marshal(runtime.Lifecycle{})
	if err != nil || string(empty) != `{}` {
		t.Fatalf("a zero lifecycle is %s (%v), want {}", empty, err)
	}
}

// TestStateWire holds the two durations of a state to integer nanoseconds and
// leaves the other members as they were.
func TestStateWire(t *testing.T) {
	s := runtime.State{ID: "sbx_1", Phase: "Running", AutoStop: time.Minute, AutoDelete: 2 * time.Minute}
	got, err := json.Marshal(s)
	if err != nil {
		t.Fatal(err)
	}
	var members map[string]any
	if err := json.Unmarshal(got, &members); err != nil {
		t.Fatal(err)
	}
	if members["autoStop"] != float64(60e9) || members["autoDelete"] != float64(120e9) || members["id"] != "sbx_1" {
		t.Fatalf("state is %s", got)
	}
	if _, present := members["pool"]; present {
		t.Fatalf("a false pool flag is written: %s", got)
	}
	var back runtime.State
	if err := json.Unmarshal(got, &back); err != nil {
		t.Fatal(err)
	}
	if back.AutoStop != time.Minute || back.AutoDelete != 2*time.Minute || back.ID != "sbx_1" {
		t.Fatalf("state read back as %+v", back)
	}
	bare, err := json.Marshal(runtime.State{})
	if err != nil {
		t.Fatal(err)
	}
	var zero map[string]any
	if err := json.Unmarshal(bare, &zero); err != nil {
		t.Fatal(err)
	}
	if _, present := zero["autoStop"]; present {
		t.Fatalf("a zero autoStop is written: %s", bare)
	}
}

// TestCreateSpecCarriesLifecycle holds the lifecycle inside a spec to the same
// nanosecond form.
func TestCreateSpecCarriesLifecycle(t *testing.T) {
	got, err := json.Marshal(runtime.CreateSpec{ID: "sbx_1", Lifecycle: runtime.Lifecycle{TTL: time.Second}}, json.Deterministic(true))
	if err != nil {
		t.Fatal(err)
	}
	var back runtime.CreateSpec
	if err := json.Unmarshal(got, &back); err != nil {
		t.Fatal(err)
	}
	if back.Lifecycle.TTL != time.Second {
		t.Fatalf("spec %s read back with lifecycle %+v", got, back.Lifecycle)
	}
}
