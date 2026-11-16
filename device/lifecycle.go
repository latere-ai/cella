// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package device

import (
	"latere.ai/x/pkg/statemachine"
)

// EngineTable is the lifecycle of the engine's state, which Engine.setState
// is the one writer of however many places ask for a change. The engine
// starts Starting and is Online once the control plane accepts its
// registration. A connection that ends or is refused makes it Offline until
// it registers again, which is Online, or Starting where the worker is run
// again. A pause holds the engine Paused until a resume starts it, and every
// other state is left for Paused when it is asked for. A computer with no
// device connected, or whose device was removed, is NotConnected until a
// client connects one, which starts the engine.
var EngineTable = statemachine.Must(statemachine.New("Engine", []EngineState{StateStarting}, map[EngineState][]EngineState{
	StateStarting:     {StateOnline, StateOffline, StatePaused, StateNotConnected},
	StateOnline:       {StateStarting, StateOffline, StatePaused, StateNotConnected},
	StateOffline:      {StateOnline, StateStarting, StatePaused, StateNotConnected},
	StatePaused:       {StateStarting, StateNotConnected},
	StateNotConnected: {StateStarting},
}))
