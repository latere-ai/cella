// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package k8s

import (
	"encoding/json/jsontext"
	"encoding/json/v2"
)

// outboundJSON writes what the cluster's API reads. Nil slices and maps are
// null, as the API's own clients write them, the bytes of one value do not
// vary between runs, and text with bytes that are not UTF-8 is written with
// U+FFFD in their place. Strings escape <, >, &, U+2028 and U+2029 as the
// API server's own encoder does: a JSON patch's test operation compares its
// value with the server's encoding of the field byte for byte, so a record
// holding "a > b" passes its guard only when written as "a \u003e b".
var outboundJSON = json.JoinOptions(
	json.Deterministic(true),
	json.FormatNilSliceAsNull(true),
	json.FormatNilMapAsNull(true),
	jsontext.AllowInvalidUTF8(true),
	jsontext.EscapeForHTML(true),
	jsontext.EscapeForJS(true),
)

// storedJSON writes a record kept in the cluster, whose bytes are the same
// for the same value. Text with bytes that are not UTF-8 is written with
// U+FFFD in their place.
var storedJSON = json.JoinOptions(json.Deterministic(true), jsontext.AllowInvalidUTF8(true))

// foreignJSON reads what the cluster's API answers with. It accepts
// duplicate members, invalid UTF-8 and any letter case in a member name.
var foreignJSON = json.JoinOptions(
	jsontext.AllowDuplicateNames(true),
	jsontext.AllowInvalidUTF8(true),
	json.MatchCaseInsensitiveNames(true),
)
