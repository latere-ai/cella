// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

//go:build !darwin && !linux

package native

import "os"

// ptySupported is false where the driver has no pseudo-terminal ioctls, so it
// declares no Attach capability and refuses a session, a TTY and a stdin.
const ptySupported = false

// ptyReader stands in for the controlling end of a pair that cannot be opened.
type ptyReader struct{ f *os.File }

func openPTY() (controlling, other *os.File, err error) { return nil, nil, errNoPTY }
func setWinsize(_ *os.File, _, _ int) error             { return errNoPTY }
func (r *ptyReader) Read(_ []byte) (int, error)         { return 0, errNoPTY }
func (r *ptyReader) Close() error                       { return nil }
