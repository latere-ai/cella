// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package cellacli

import (
	"bytes"
	"encoding/json/v2"
	"testing"
)

// TestOutputIsOneLineInKeyOrder: --json output is read by people and scripts,
// so a composed value writes its members in key order on one line.
func TestOutputIsOneLineInKeyOrder(t *testing.T) {
	var out bytes.Buffer
	if err := writeValue(&out, map[string]any{"path": "p", "files": 2, "a": []string(nil)}); err != nil {
		t.Fatal(err)
	}
	if want := "{\"a\":[],\"files\":2,\"path\":\"p\"}\n"; out.String() != want {
		t.Errorf("writeValue wrote %q, want %q", out.String(), want)
	}
	out.Reset()
	if err := writeItems(&out, nil); err != nil {
		t.Fatal(err)
	}
	if want := "{\"items\":[],\"next\":\"\"}\n"; out.String() != want {
		t.Errorf("writeItems wrote %q, want %q", out.String(), want)
	}
}

// TestManifestFileIsReadLeniently: the manifest a person wrote may repeat a
// member, spell one in another case or carry bytes that are not UTF-8; the
// server decides what it refuses, so the command reads it as it always did.
func TestManifestFileIsReadLeniently(t *testing.T) {
	var header document
	raw := []byte(`{"kind":"Secret","Kind":"Sandbox","APIVERSION":"v","metadata":{"name":"n` + "\xff" + `"}}`)
	if err := json.Unmarshal(raw, &header, foreignJSON); err != nil {
		t.Fatal(err)
	}
	if header.Kind != "Sandbox" || header.APIVersion != "v" || header.Metadata.Name != "n�" {
		t.Errorf("header = %+v", header)
	}
}
