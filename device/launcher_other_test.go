// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

//go:build !darwin && !linux

package device

// launcher has nothing to launch where the engine does not run.
func launcher() {}
