// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

//go:build race

package shell

// raceEnabled is a build with the race detector, which runs a line
// several times slower and drops what sync.Pool keeps, so regexp
// allocates a machine for every line grep matches.
const raceEnabled = true
