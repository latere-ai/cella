// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package client_test

import (
	"encoding/json/jsontext"
	"encoding/json/v2"
	"testing"

	"latere.ai/x/cella/client"
)

// TestEventDataIsOmittedOnlyWhenAbsent holds the payload of a record to the
// writing it had: absent when there is none, written when it is an empty
// object or null.
func TestEventDataIsOmittedOnlyWhenAbsent(t *testing.T) {
	for _, c := range []struct {
		data    jsontext.Value
		present bool
		want    string
	}{
		{nil, false, ""},
		{jsontext.Value(`{}`), true, `{}`},
		{jsontext.Value(`null`), true, `null`},
	} {
		got, err := json.Marshal(client.Event{Type: "x", Data: c.data})
		if err != nil {
			t.Fatal(err)
		}
		var members map[string]jsontext.Value
		if err := json.Unmarshal(got, &members); err != nil {
			t.Fatal(err)
		}
		data, present := members["data"]
		if present != c.present || string(data) != c.want {
			t.Errorf("event with data %q is %s", c.data, got)
		}
	}
}
