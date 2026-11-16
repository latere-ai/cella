// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package device

import (
	"fmt"
	"sync"
	"sync/atomic"
	"testing"

	"latere.ai/x/pkg/statemachine"
	"latere.ai/x/pkg/statemachine/statemachinetest"
)

// TestEngineStateWriter drives Engine.setState, the one writer of the
// engine's state, through EngineTable: a step the table has lands, any other
// is refused and leaves the state as it was, and two writers racing on one
// engine end in a state the table's steps explain.
func TestEngineStateWriter(t *testing.T) {
	var (
		mu      sync.Mutex
		engines = map[string]*Engine{}
		next    atomic.Int64
	)
	engine := func(key string) *Engine {
		mu.Lock()
		defer mu.Unlock()
		return engines[key]
	}
	d := statemachinetest.Driver[EngineState]{
		Enter: func(t *testing.T, s EngineState) string {
			e := New(Config{})
			// The engine starts Starting; every other state is reached along
			// the table's own steps.
			for _, step := range pathTo(s)[1:] {
				if !e.setState(step, "") {
					t.Fatalf("entering %s: the step to %s was refused", s, step)
				}
			}
			key := fmt.Sprint(next.Add(1))
			mu.Lock()
			engines[key] = e
			mu.Unlock()
			return key
		},
		Move: func(key string, to EngineState) error {
			if !engine(key).setState(to, "") {
				return fmt.Errorf("the engine did not take %s: %w", to, statemachine.ErrIllegalTransition)
			}
			return nil
		},
		State: func(t *testing.T, key string) EngineState { return engine(key).Status().State },
	}
	statemachinetest.Exhaustive(t, EngineTable, d)
	statemachinetest.Race(t, EngineTable, d)
}

// pathTo is the shortest walk along EngineTable from the state the engine
// starts in to s.
func pathTo(to EngineState) []EngineState {
	queue := [][]EngineState{{StateStarting}}
	seen := map[EngineState]bool{StateStarting: true}
	for len(queue) > 0 {
		walk := queue[0]
		queue = queue[1:]
		last := walk[len(walk)-1]
		if last == to {
			return walk
		}
		for _, next := range EngineTable.Next(last) {
			if !seen[next] {
				seen[next] = true
				queue = append(queue, append(append([]EngineState{}, walk...), next))
			}
		}
	}
	return nil
}

// TestEnginePausedHoldsItsState: under a pause no state but Paused is taken,
// whatever the table allows.
func TestEnginePausedHoldsItsState(t *testing.T) {
	e := New(Config{})
	e.mu.Lock()
	e.paused = true
	e.mu.Unlock()
	if e.setState(StateOnline, "") || e.Status().State != StateStarting {
		t.Fatalf("a paused engine took Online: %s", e.Status().State)
	}
}
