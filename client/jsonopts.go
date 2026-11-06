// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package client

import "encoding/json/jsontext"

// requestJSON writes a request that carries what a person typed: a command,
// an environment, a path. A byte that is not UTF-8 reaches the server as
// U+FFFD, where the request would otherwise not be sent at all.
var requestJSON = jsontext.AllowInvalidUTF8(true)
