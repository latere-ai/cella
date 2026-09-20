#!/usr/bin/env bash
# SPDX-FileCopyrightText: 2026 Latere AI
# SPDX-License-Identifier: Apache-2.0
#
# Delete the cluster up.sh created. Everything the stack held was in it.
#
#   down.sh [-name <cluster>]
set -euo pipefail

name=cella
case "${1:-}" in
  -name) name=${2:?-name takes a cluster name} ;;
  "") ;;
  *) echo "usage: $0 [-name <cluster>]" >&2; exit 2 ;;
esac

kind delete cluster --name "$name"
