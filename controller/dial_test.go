// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"errors"
	"testing"

	driver "latere.ai/x/cella/runtime"
)

// noDialer is a driver with every method of the contract and no Dialer.
type noDialer struct{ driver.Driver }

// TestDialerIsTheEnvironmentsDriver: a sandbox's dial half is the driver that
// serves its environment, and an environment whose driver has none answers
// the unsupported operation the API turns into 422.
func TestDialerIsTheEnvironmentsDriver(t *testing.T) {
	c, o := newController(t)
	obj, err := realized(t.Context(), c, workspace(), "alice", 2)
	if err != nil {
		t.Fatal(err)
	}
	dialer, err := c.Dialer(obj.Status.ID)
	if err != nil {
		t.Fatalf("Dialer on a driver that dials: %v", err)
	}
	if dialer != o.Driver.(driver.Dialer) {
		t.Fatalf("Dialer is %T, want the environment's own driver", dialer)
	}
	c.setDriver(c.environment, noDialer{o.Driver})
	if _, err = c.Dialer(obj.Status.ID); !errors.Is(err, driver.ErrUnsupported) {
		t.Fatalf("Dialer on a driver with none: %v", err)
	}
}
