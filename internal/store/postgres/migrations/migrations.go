// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

// Package migrations embeds the schema of design 010 as golang-migrate files.
// They are numbered from 000001 and applied in order at start-up; a released
// migration is never edited, because the databases that ran it already have.
package migrations

import (
	"embed"
	"errors"
	"fmt"
	"io/fs"
	"strconv"
	"strings"
)

//go:embed *.sql
var FS embed.FS

// Highest is the newest migration this binary carries. The store refuses to
// start against a schema above it: the migrator would report no change and
// then serve statements against columns it does not know.
func Highest() (uint, error) { return highestOf(FS) }

// highestOf reads the number every migration file starts with.
func highestOf(files fs.FS) (uint, error) {
	entries, err := fs.ReadDir(files, ".")
	if err != nil {
		return 0, fmt.Errorf("reading the embedded migrations: %w", err)
	}
	var highest uint
	for _, e := range entries {
		number, _, found := strings.Cut(e.Name(), "_")
		if !found {
			continue
		}
		value, err := strconv.ParseUint(number, 10, 64)
		if err != nil {
			return 0, fmt.Errorf("the migration %q does not start with a number: %w", e.Name(), err)
		}
		if uint(value) > highest {
			highest = uint(value)
		}
	}
	if highest == 0 {
		return 0, errors.New("no migration is embedded")
	}
	return highest, nil
}
