// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

// Package device serves a person's own computer as one of their Cella
// environments: the engine that runs Cella's embedded worker on the local
// driver, with what the computer keeps to do it safely.
//
// The engine holds the host policy file (the folders sessions may use, the
// network they may reach, how a session is let in), the environment key in
// the keychain and its rotation, the person's approval of each session,
// the activity log of every operation, the lock that keeps one engine per
// computer, and a local control socket through which another process on
// the same account watches and drives it.
//
// It is embedded, not run on its own. The latere command runs it in a
// terminal (`latere devices connect` and `serve`), and a desktop
// application runs it inside its own process. Each embedder builds an
// Engine with New, supplies an Account (and, for connecting from the
// embedder, an Enroller, usually an Enrollment over Cella with a Platform
// for the parts a hosted plane decides), and calls Run. Status, Subscribe,
// Answer, ProposeConnect, Connect, PutPolicy, Remove, Pause and Resume are
// the in-process API; ServeControl answers the same over the control
// socket, so an application can attach to an engine a terminal started,
// and the other way round.
//
// DefaultPaths are the files every embedder shares, so a computer
// connected from one embedder and served by another is one device.
package device
