// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

//go:build !darwin && !linux

package cellacli

import "os"

// OSTerminal answers that this build has no terminal ioctls, so a session
// runs with the fixed window and the caller's line discipline. Every other
// command is unaffected.
func OSTerminal(*os.File) Terminal { return nil }
