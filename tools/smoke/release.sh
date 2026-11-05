#!/usr/bin/env bash
# SPDX-FileCopyrightText: 2026 Latere AI
# SPDX-License-Identifier: Apache-2.0
#
# The live check after a deploy of cellad. The release pipeline runs it once
# the rollout is done and attaches its Markdown evidence to the release; an
# operator can run it by hand against their own installation.
#
# It sends unauthenticated, read-only requests and holds no wait loop: it
# fails at once and the caller runs it again.
#
# BASE_URL is cellad's public listener including its base path, with no
# default: production is https://api.latere.ai/v1/environments. The version
# route is GET $BASE_URL/version. cellad serves no public health route under
# a base (the orchestrator reads /livez and /readyz on the internal
# listener), so the health check here is the build line at $BASE_URL/.
#
# Required tools: curl, grep.
#
# Environment:
#   BASE_URL   the public origin and base path to check; required
#   TAG        the release being proved; when set, the served version must
#              equal it
#   COMMIT     the release commit, for the evidence only
#   DEPLOY_URL the deploying run, for the evidence only
#   OUTPUT_MD  where to write the Markdown evidence; nowhere when unset

set -euo pipefail

BASE_URL="${BASE_URL:-}"
BASE_URL="${BASE_URL%/}"
TAG="${TAG:-}"
COMMIT="${COMMIT:-unknown}"
DEPLOY_URL="${DEPLOY_URL:-}"
OUTPUT_MD="${OUTPUT_MD:-}"

pass() { printf 'ok: %s\n' "$*"; }
fail() { printf 'FAIL: %s\n' "$*" >&2; exit 1; }

[ -n "$BASE_URL" ] || fail "BASE_URL is required; this check has no default target"
for cmd in curl grep; do
  command -v "$cmd" >/dev/null || fail "$cmd is required"
done

tmp="$(mktemp -d)"
trap 'rm -rf "$tmp"' EXIT

check_status() {
  local label="$1" path="$2" out="$3" code
  code=$(curl -sS --max-time 20 -o "$out" -w '%{http_code}' "${BASE_URL}${path}") || fail "$label: curl failed"
  [ "$code" = "200" ] || fail "$label: expected 200, got $code (body: $(head -c 200 "$out" 2>/dev/null || true))"
  pass "$label (200)"
}

check_status "GET /" "/" "$tmp/root"
grep -q '^cellad ' "$tmp/root" || fail "GET /: not the cellad build line ($(head -c 200 "$tmp/root"))"

check_status "GET /version" "/version" "$tmp/version"
served=$(grep -o '"version":"[^"]*"' "$tmp/version" | head -1 | cut -d'"' -f4 || true)
[ -n "$served" ] || fail "GET /version: no version in $(head -c 200 "$tmp/version")"
if [ -n "$TAG" ]; then
  [ "$served" = "$TAG" ] || fail "version mismatch: served $served, expected $TAG"
  pass "served version matches the tag ($served)"
else
  pass "served version recorded ($served)"
fi

if [ -n "$OUTPUT_MD" ]; then
  {
    echo "<!-- release-evidence -->"
    echo
    echo "## Release evidence"
    echo
    echo "- Tag: \`${TAG:-unknown}\`"
    echo "- Commit: \`${COMMIT}\`"
    [ -n "$DEPLOY_URL" ] && echo "- Deploy: ${DEPLOY_URL}"
    echo "- Origin: ${BASE_URL}"
    echo "- Served version: \`${served}\`"
    echo "- Smoke: \`/\` answered the cellad build line and \`/version\` answered 200"
  } > "$OUTPUT_MD"
fi

printf '\nrelease smoke passed\n'
