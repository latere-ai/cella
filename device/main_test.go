// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package device

import (
	"os"
	"testing"

	"github.com/zalando/go-keyring"

	"latere.ai/x/pkg/hostsandbox"
)

func TestMain(m *testing.M) {
	// The test binary is also the launcher of a sandboxed command with a
	// network allowlist, as an embedder's own binary is, and calling Main
	// is what makes the local driver confine with Seatbelt on macOS.
	hostsandbox.Main()
	// Every key a test keeps lands in an in-memory keychain, never the
	// developer's own.
	keyring.MockInit()
	os.Exit(m.Run())
}
