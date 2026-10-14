// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package postgres

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"latere.ai/x/cella/internal/store"
)

// testKey is a 32 byte key for the cases that need the envelope, and no
// database.
var testKey = []byte("0123456789abcdef0123456789abcdef")

func envelope(t *testing.T, key []byte) store.Envelope {
	t.Helper()
	env, err := store.NewEnvelope(key)
	if err != nil {
		t.Fatal(err)
	}
	return env
}

// sameDocument fails the test where two JSON documents differ as values.
func sameDocument(t *testing.T, got, want []byte, what string) {
	t.Helper()
	var g, w any
	if err := json.Unmarshal(got, &g); err != nil {
		t.Fatalf("%s is not JSON: %q", what, got)
	}
	if err := json.Unmarshal(want, &w); err != nil {
		t.Fatalf("the expected %s is not JSON: %q", what, want)
	}
	left, _ := json.Marshal(g)
	right, _ := json.Marshal(w)
	if string(left) != string(right) {
		t.Errorf("%s = %s, want %s", what, left, right)
	}
}

// TestSealPassesWhatItDoesNotSeal: an object of another kind, and a sandbox on
// a store without a key, are written as given; a sandbox with neither value
// is written as given and seals nothing.
func TestSealPassesWhatItDoesNotSeal(t *testing.T) {
	data := []byte(`{"spec":{"env":{"K":"v"}}}`)
	status := []byte(`{"egressState":{"credential":"c"}}`)
	for _, tc := range []struct {
		name string
		env  store.Envelope
		kind string
	}{
		{"another kind", envelope(t, testKey), store.KindSecret},
		{"no key", envelope(t, nil), store.KindSandbox},
	} {
		gotData, gotStatus, wrapped, sealed, err := seal(tc.env, store.Object{Kind: tc.kind, Data: data, Status: status})
		if err != nil || string(gotData) != string(data) || string(gotStatus) != string(status) || wrapped != nil || sealed != nil {
			t.Errorf("%s: seal = %s %s %v %v %v", tc.name, gotData, gotStatus, wrapped, sealed, err)
		}
	}
	bare := []byte(`{"spec":{"image":"img","env":{}}}`)
	gotData, _, wrapped, sealed, err := seal(envelope(t, testKey), store.Object{Kind: store.KindSandbox, Data: bare, Status: []byte(`{}`)})
	if err != nil || string(gotData) != string(bare) || wrapped != nil || sealed != nil {
		t.Errorf("a sandbox with no values: %s %v %v %v", gotData, wrapped, sealed, err)
	}
	if _, _, _, _, err := seal(envelope(t, testKey), store.Object{Kind: store.KindSandbox, ID: "sbx_a",
		Data: []byte(`{"spec":{"env":"sealed"}}`)}); err == nil || !strings.Contains(err.Error(), "sbx_a") {
		t.Errorf("sealing data that is already marked: %v", err)
	}
}

// TestUnsealRefusesWhatItCannotOpen: a marked row with no sealed value, a
// store without a key, another key, and sealed bytes that are not the
// document are each an error naming the sandbox, and a row that is not marked
// is left alone.
func TestUnsealRefusesWhatItCannotOpen(t *testing.T) {
	key := envelope(t, testKey)
	marked := []byte(`{"spec":{"env":"sealed"}}`)
	wrapped, sealed, err := key.Seal([]byte(`{"env":{"K":"v"}}`))
	if err != nil {
		t.Fatal(err)
	}
	junkWrapped, junk, err := key.Seal([]byte(`not json`))
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name            string
		env             store.Envelope
		wrapped, sealed []byte
		want            error
	}{
		{"nothing sealed", key, nil, nil, nil},
		{"no key", envelope(t, nil), wrapped, sealed, store.ErrNoSecretKey},
		{"another key", envelope(t, []byte("fedcba9876543210fedcba9876543210")), wrapped, sealed, nil},
		{"not the document", key, junkWrapped, junk, nil},
	} {
		obj := store.Object{Kind: store.KindSandbox, ID: "sbx_a", Data: marked}
		err := unseal(tc.env, &obj, tc.wrapped, tc.sealed)
		if err == nil || !strings.Contains(err.Error(), "sbx_a") {
			t.Errorf("%s: unseal = %v, want an error naming the sandbox", tc.name, err)
		}
		if tc.want != nil && !errors.Is(err, tc.want) {
			t.Errorf("%s: unseal = %v, want %v", tc.name, err, tc.want)
		}
	}
	for _, obj := range []store.Object{
		{Kind: store.KindSecret, Data: marked},
		{Kind: store.KindSandbox, Data: []byte(`{"spec":{"env":{"K":"v"}}}`)},
	} {
		before := string(obj.Data)
		if err := unseal(key, &obj, wrapped, sealed); err != nil || string(obj.Data) != before {
			t.Errorf("an object unseal leaves alone became %s, %v", obj.Data, err)
		}
	}
	// A sealed credential is put back into a status that has none, and an
	// environment that was sealed empty leaves no member behind.
	credWrapped, credSealed, err := key.Seal([]byte(`{"credential":"c"}`))
	if err != nil {
		t.Fatal(err)
	}
	obj := store.Object{Kind: store.KindSandbox, ID: "sbx_a", Data: marked}
	if err := unseal(key, &obj, credWrapped, credSealed); err != nil {
		t.Fatal(err)
	}
	sameDocument(t, obj.Data, []byte(`{"spec":{}}`), "the data")
	sameDocument(t, obj.Status, []byte(`{"egressState":{"credential":"c"}}`), "the status")
}

// TestTheDocumentHelpers holds take, put, marked and blank to the shapes a
// row can hold, including the ones a sandbox never writes.
func TestTheDocumentHelpers(t *testing.T) {
	for _, tc := range []struct {
		name, document string
		value          json.RawMessage
		want           string
		fails          bool
	}{
		{"into nothing", ``, json.RawMessage(`1`), `{"spec":{"env":1}}`, false},
		{"into null", `null`, json.RawMessage(`1`), `{"spec":{"env":1}}`, false},
		{"into a null object", `{"spec":null}`, json.RawMessage(`1`), `{"spec":{"env":1}}`, false},
		{"over a member", `{"spec":{"env":0,"image":"i"}}`, json.RawMessage(`1`), `{"spec":{"env":1,"image":"i"}}`, false},
		{"a removal", `{"spec":{"env":0,"image":"i"}}`, nil, `{"spec":{"image":"i"}}`, false},
		{"a removal of nothing", `{"spec":{"image":"i"}}`, nil, `{"spec":{"image":"i"}}`, false},
		{"into an array", `[1]`, json.RawMessage(`1`), ``, true},
		{"into a scalar object", `{"spec":3}`, json.RawMessage(`1`), ``, true},
	} {
		got, err := put([]byte(tc.document), "spec", "env", tc.value)
		switch {
		case tc.fails && err == nil:
			t.Errorf("put %s = %s, want a refusal", tc.name, got)
		case !tc.fails && err != nil:
			t.Errorf("put %s: %v", tc.name, err)
		case !tc.fails:
			sameDocument(t, got, []byte(tc.want), "put "+tc.name)
		}
	}
	for _, document := range []string{``, `null`, `[1]`, `{"spec":[1]}`, `{"spec":{"env":null}}`, `{"spec":{"env":""}}`, `{"spec":{"env":{}}}`} {
		out, value, err := take([]byte(document), "spec", "env")
		if err != nil || value != nil || string(out) != document {
			t.Errorf("take from %q = %s, %s, %v, want nothing taken", document, out, value, err)
		}
	}
	for _, tc := range []struct {
		document string
		want     bool
	}{
		{`{"spec":{"env":"sealed"}}`, true},
		{`{"spec":{"env":"other"}}`, false},
		{`{"spec":{"env":{"K":"v"}}}`, false},
		{`{"spec":"sealed"}`, false},
		{`[]`, false},
	} {
		if got := marked([]byte(tc.document)); got != tc.want {
			t.Errorf("marked(%s) = %v, want %v", tc.document, got, tc.want)
		}
	}
	for _, tc := range []struct {
		raw  string
		want bool
	}{
		{``, true}, {`null`, true}, {`""`, true}, {`{}`, true},
		{`"v"`, false}, {`{"K":"v"}`, false}, {`3`, false}, {`[`, false},
	} {
		if got := blank(json.RawMessage(tc.raw)); got != tc.want {
			t.Errorf("blank(%s) = %v, want %v", tc.raw, got, tc.want)
		}
	}
	// A credential with no data to mark is refused rather than sealed
	// beside a row that would not say so.
	if _, _, _, err := split([]byte(`[1]`), []byte(`{"egressState":{"credential":"c"}}`)); err == nil {
		t.Error("split marked data that is not an object")
	}
}
