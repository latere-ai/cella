#!/usr/bin/env bash
# SPDX-FileCopyrightText: 2026 Latere AI
# SPDX-License-Identifier: Apache-2.0
#
# Runs the fenced `sh` blocks of a Markdown document in order, in one
# shell, so a document is a program and a command that stopped working
# fails a build rather than a reader (spec 014):
#
#	tools/docs/run-blocks.sh docs/install.md
#
# One shell for every block, so a variable a block exports is there for
# the next one, which is how the document reads. A block fenced with any
# other language, or with none, is prose and is not run. The extracted
# program is written to stderr before it runs, so a failed job shows what
# it executed and at which line of the document each command started.
set -euo pipefail

doc=${1:?usage: run-blocks.sh DOCUMENT.md}
program=$(mktemp)
trap 'rm -f "$program"' EXIT

awk -v doc="$doc" '
  /^```sh$/   { inblock = 1; printf "# %s:%d\n", doc, NR + 1; next }
  /^```/      { if (inblock) { inblock = 0; next } }
  inblock     { print }
' "$doc" > "$program"

if [ ! -s "$program" ]; then
  echo "run-blocks: $doc holds no fenced sh block" >&2
  exit 1
fi

echo "run-blocks: $doc" >&2
cat -n "$program" >&2
exec bash -euo pipefail "$program"
