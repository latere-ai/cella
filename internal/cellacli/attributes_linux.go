// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package cellacli

import "syscall"

// The ioctls that read and write a terminal's mode on this operating
// system.
const (
	getAttributes = syscall.TCGETS
	setAttributes = syscall.TCSETS
)
