// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package device

import (
	"encoding/json/jsontext"
	"encoding/json/v2"
)

// storedJSON writes what is kept on disk, in the keychain or digested: the
// same value always gives the same bytes.
var storedJSON = json.Deterministic(true)

// journalJSON writes an activity entry. Commands, paths and reasons carry
// whatever a person or a process gave, so a byte that is not UTF-8 becomes
// U+FFFD instead of losing the entry.
var journalJSON = json.JoinOptions(storedJSON, jsontext.AllowInvalidUTF8(true))
