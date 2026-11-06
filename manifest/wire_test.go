// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package manifest

import (
	"errors"
	"fmt"
	"testing"
)

// TestDecodeReadsHandWrittenJSON holds a JSON manifest a person wrote to the
// reading it has always had: a repeated member takes the last value and a byte
// that is not UTF-8 reads as U+FFFD.
func TestDecodeReadsHandWrittenJSON(t *testing.T) {
	body := []byte(`{"apiVersion":"cella.latere.ai/v1beta1","kind":"Sandbox",` +
		`"metadata":{"name":"first","name":"second","labels":{"note":"caf` + "\xe9" + `"}},"spec":{"environment":"default"}}`)
	got, err := Decode(body, MediaJSON)
	if err != nil {
		t.Fatalf("Decode: %v", err)
	}
	if got.Metadata.Name != "second" {
		t.Errorf("the repeated name read as %q, want the last", got.Metadata.Name)
	}
	if got.Metadata.Labels["note"] != "caf�" {
		t.Errorf("the invalid byte read as %q", got.Metadata.Labels["note"])
	}
}

// TestDecodeNamesAnUnknownMember holds the refusal of a member the schema
// does not know, whichever arm of the decoder finds it.
func TestDecodeNamesAnUnknownMember(t *testing.T) {
	body := []byte(`{"apiVersion":"cella.latere.ai/v1beta1","kind":"Sandbox","metadata":{"name":"a"},"spec":{"environment":"default","bogus":1}}`)
	_, err := Decode(body, MediaJSON)
	var refusal *Error
	if !errors.As(err, &refusal) || refusal.Code != "unknown_field" {
		t.Fatalf("an unknown member gave %v, want unknown_field", err)
	}
}

// TestCapacityTakesTheObjectOrTheWord holds the self-decoding capacity to
// both of its forms and to its leniency about members it does not know.
func TestCapacityTakesTheObjectOrTheWord(t *testing.T) {
	base := `{"apiVersion":"cella.latere.ai/v1beta1","kind":"Environment","metadata":{"name":"e"},"spec":{"mode":"worker","isolation":"process","capacity":%s}}`
	for _, c := range []struct {
		name, capacity string
		auto           bool
		sandboxes      int
		refused        bool
	}{
		{"word", `"auto"`, true, 0, false},
		{"object", `{"sandboxes":4}`, false, 4, false},
		{"object with a member of no schema", `{"sandboxes":4,"extra":1}`, false, 4, false},
		{"other word", `"huge"`, false, 0, true},
	} {
		t.Run(c.name, func(t *testing.T) {
			env, err := DecodeEnvironment([]byte(fmt.Sprintf(base, c.capacity)), MediaJSON)
			if c.refused {
				var refusal *Error
				if !errors.As(err, &refusal) || refusal.Code != "invalid_field" {
					t.Fatalf("got %v, want invalid_field", err)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if env.Spec.Capacity.Auto != c.auto || env.Spec.Capacity.Sandboxes != c.sandboxes {
				t.Errorf("capacity read as %+v", env.Spec.Capacity)
			}
		})
	}
}
