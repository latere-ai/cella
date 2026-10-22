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
// Answer, ProposeConnect, Connect, PutPolicy, AllowSessionHost, SaveHost,
// Remove, Pause and Resume are the in-process API; ServeControl answers the
// same over the control socket, so an application can attach to an engine
// a terminal started, and the other way round.
//
// The network sessions reach is the host policy's: an allowlist, none, or
// any public host but those it blocks, which macOS's own sandbox alone
// enforces and which lets everything readable in a granted folder be sent
// to any public host. Each connection a command is refused reaches every
// subscriber as an EventNetworkRefused with its host, port, session, agent
// and sandbox, and the activity log as a line; AllowSessionHost lets the
// session's later commands reach the host without changing the policy, and
// SaveHost writes it into the policy. A command keeps the network it
// started with.
//
// DefaultPaths are the files every embedder shares, so a computer
// connected from one embedder and served by another is one device.
//
// The package names no hosted plane. The embedder supplies what one
// decides: the label keys its sessions carry (Config.Labels), the audience
// of its platform token and how a token's claims read
// (CredentialsOptions), and the platform's part of a connect (Platform).
//
// The sentences and hints of an Error, and the facts a connect shows, are
// the latere command's words, kept as defaults so a terminal reads the
// same whichever embedder answers. An application that shows its own
// words keys them on the Error's Code and on each Fact's Kind, which are
// stable.
package device
