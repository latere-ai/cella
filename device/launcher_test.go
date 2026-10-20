// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

//go:build darwin || linux

package device

import "latere.ai/x/pkg/hostsandbox"

// launcher makes the test binary also the launcher of a sandboxed command
// with a network allowlist, as an embedder's own binary is; calling Main is
// what makes the local driver confine with Seatbelt on macOS.
func launcher() { hostsandbox.Main() }
