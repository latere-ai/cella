// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package podman

import (
	"encoding/json/jsontext"
	"encoding/json/v2"
)

// outboundJSON writes what the engine's API reads. Nil slices and maps are
// null, which the engine accepts where it rejects an empty list, the
// bytes of one value do not vary between runs, and text that carries bytes
// which are not UTF-8 is written with U+FFFD in their place.
var outboundJSON = json.JoinOptions(
	json.Deterministic(true),
	json.FormatNilSliceAsNull(true),
	json.FormatNilMapAsNull(true),
	jsontext.AllowInvalidUTF8(true),
)

// foreignJSON reads what the engine answers with. It accepts duplicate
// members, invalid UTF-8 and any letter case in a member name, which engine
// versions differ on.
var foreignJSON = json.JoinOptions(
	jsontext.AllowDuplicateNames(true),
	jsontext.AllowInvalidUTF8(true),
	json.MatchCaseInsensitiveNames(true),
)
