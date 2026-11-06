// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package egress

import (
	"strings"
	"testing"
)

// A record whose path carries a byte that is not UTF-8 is delivered with
// U+FFFD in its place and does not fail the frame.
func TestEncodeRecordInvalidUTF8(t *testing.T) {
	message, err := Encode(Frame{Type: FrameRecord, Record: &Record{Principal: "sbx_1", Path: "/a/\xff"}})
	if err != nil {
		t.Fatalf("the frame was refused: %v", err)
	}
	if !strings.Contains(string(message), `"/a/�"`) && !strings.Contains(string(message), "/a/�") {
		t.Fatalf("frame is %s", message)
	}
}
