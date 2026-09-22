// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package events

import "testing"

// TestOperationRecordsCarryNoContent holds the computer-use records and the
// dial session's of design 009 to what they may say. A screenshot names a
// size, an input batch a count, a screen or dial session a duration and two
// byte counts, and none of them can hold a frame, a key, a character of typed
// text or a byte of a connection: the shapes have no field for one.
func TestOperationRecordsCarryNoContent(t *testing.T) {
	obj := sandbox()
	for _, tc := range []struct {
		name string
		kind Type
		data any
		want string
	}{
		{
			"screenshot", TypeScreenshot, Screenshot{Width: 1280, Height: 800, Format: "png"},
			`{"width":1280,"height":800,"format":"png"}`,
		},
		{
			"input", TypeInput, Input{Events: 12},
			`{"events":12}`,
		},
		{
			"screen", TypeScreen, Screen{DurationMS: 4200, BytesIn: 8, BytesOut: 1 << 20},
			`{"durationMs":4200,"bytesIn":8,"bytesOut":1048576}`,
		},
		{
			"dial", TypeDial, Dial{DurationMS: 900, BytesIn: 64, BytesOut: 512},
			`{"durationMs":900,"bytesIn":64,"bytesOut":512}`,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			record, err := Operation(tc.kind, OfSandbox(obj), tc.data, person, at)
			if err != nil {
				t.Fatalf("building the record: %v", err)
			}
			if !Deliverable(record.Type) {
				t.Errorf("%s is journaled and never delivered", record.Type)
			}
			if record.Sandbox == nil {
				t.Error("an operation record names no sandbox in context")
			}
			// The comparison is exact, so a field added later that could
			// carry content fails here before it reaches a sink.
			sameJSON(t, record.Data, []byte(tc.want))
		})
	}
}
