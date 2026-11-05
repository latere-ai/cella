// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

// Package smoke holds the test for release.sh, the live check the release
// pipeline runs against cellad after a rollout and an operator runs by hand
// after an upgrade. The script is shell so it can be read and run without a
// Go toolchain; this package keeps it honest.
package smoke
