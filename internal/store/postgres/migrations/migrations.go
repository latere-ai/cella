// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

// Package migrations embeds the schema of design 010 as golang-migrate files.
// They are numbered from 000001 and applied in order at start-up; a released
// migration is never edited, because the databases that ran it already have.
package migrations

import "embed"

//go:embed *.sql
var FS embed.FS

// SealedCredentials is the migration that adds the columns a sandbox row's
// sealed values live in. A schema below it holds nothing sealed, and it is
// the schema a rollback to a release that does not seal migrates down past.
const SealedCredentials uint = 7
