#!/usr/bin/env bash
# SPDX-FileCopyrightText: 2026 Latere AI
# SPDX-License-Identifier: Apache-2.0
#
# The development stack of spec 049: cella-stubs and `cellad serve` on
# loopback, wired to each other, with the one command that mints a caller
# token printed at the end. `make run` is this script's name.
#
#   tools/run/up.sh          serve until interrupted
#   tools/run/up.sh -smoke   bring the stack up, create one sandbox, stop
#
# The state lives under out/run/ and is kept: the signing key and the sink's
# secret are generated once, so a restart does not invalidate the tokens of
# the last one. `make clean` removes it.
#
# Two clones run side by side by naming a port: CELLA_RUN_PORT=8090 make run
# serves the API on 8090 and the probes on 8091. CELLA_RUN_PORT=0 leaves both
# to the kernel, which is what a test wants: a port picked before cellad
# starts can be taken by another process before cellad binds it, and one the
# kernel hands cellad at the bind cannot. The stubs always bind whatever the
# kernel gives them. Every process prints the addresses it bound, and this
# script writes them to stubs.addrs and cellad.addrs in the state directory,
# one role and its address per line.
set -euo pipefail

smoke=0
case "${1:-}" in
  -smoke) smoke=1 ;;
  "") ;;
  *) echo "usage: $0 [-smoke]" >&2; exit 2 ;;
esac

root=$(cd "$(dirname "$0")/../.." && pwd)
out=${CELLA_RUN_DIR:-$root/out/run}
port=${CELLA_RUN_PORT:-8080}
if [ "$port" = 0 ]; then
  public_addr=127.0.0.1:0 internal_addr=127.0.0.1:0
  # CELLA_PUBLIC_URL is read before the port is known. It is the iss of the
  # tokens cellad mints and nothing in this stack dials it, so the
  # placeholder stands in for the address cellad reports below.
  public_url=http://127.0.0.1:0
else
  public_addr=127.0.0.1:$port internal_addr=127.0.0.1:$((port + 1))
  public_url=http://127.0.0.1:$port
fi
mkdir -p "$out"

# 1. Both binaries, from this checkout.
(cd "$root" && go build -o "$out/cellad" ./cmd/cellad && go build -o "$out/cella-stubs" ./cmd/cella-stubs)

# 2. The three credentials this installation holds, generated once. The
# signing key is what cellad signs every sandbox's identity with; the events
# secret is what the sink verifies each delivery against; the secret key is
# what a stored Secret's value is wrapped under, without which this stack
# takes no Secret at all.
[ -s "$out/token.pem" ] || openssl genrsa -out "$out/token.pem" 2048 2>/dev/null
[ -s "$out/events.secret" ] || openssl rand -hex 32 > "$out/events.secret"
[ -s "$out/secret.key" ] || openssl rand -base64 32 > "$out/secret.key"

pids=()
cleanup() {
  for pid in "${pids[@]:-}"; do
    [ -n "$pid" ] && kill "$pid" 2>/dev/null || true
  done
  wait 2>/dev/null || true
}
trap cleanup EXIT INT TERM

# 3. The stubs, on ports the kernel chooses, printed one role per line. Each
# file a process prints its addresses to is emptied here as well as by its
# redirect, which runs in the child: a loop that read the file first would
# find the last run's addresses.
: > "$out/stubs.addrs"
"$out/cella-stubs" -issuer 127.0.0.1:0 -authorizer 127.0.0.1:0 \
  -admission 127.0.0.1:0 -sink 127.0.0.1:0 \
  -sink-secret "$(cat "$out/events.secret")" > "$out/stubs.addrs" 2>"$out/stubs.log" &
pids+=($!)

role() { sed -n "s#^$1 ##p" "$out/stubs.addrs" | head -1; }
for _ in $(seq 1 100); do
  [ -s "$out/stubs.addrs" ] && [ "$(wc -l < "$out/stubs.addrs")" -ge 4 ] && break
  sleep 0.1
done
issuer=$(role issuer)
authorizer=$(role authorizer)
admission=$(role admission)
sink=$(role sink)
[ -n "$issuer" ] || { echo "the stubs did not start:"; cat "$out/stubs.log"; exit 1; }

# 4. The issuer answers before cellad asks: the verifier of spec 006 reads
# the discovery document and the key set at start and refuses to start
# without them.
for _ in $(seq 1 100); do
  curl -fsS "$issuer/.well-known/openid-configuration" > /dev/null 2>&1 && break
  sleep 0.1
done

# 5. The control plane. The native backend needs no cluster and no
# container engine, and it has no isolation, which is why spec 002 makes
# the consent a variable of its own and why this line writes it out.
: > "$out/cellad.out"
rm -f "$out/cellad.addrs"
CELLA_DATA_DIR="$out" \
CELLA_RUNTIME=native CELLA_ALLOW_UNSAFE_NATIVE=true \
CELLA_PUBLIC_ADDR=$public_addr CELLA_INTERNAL_ADDR=$internal_addr \
CELLA_PUBLIC_URL="$public_url" \
CELLA_TOKEN_KEY="$(cat "$out/token.pem")" \
CELLA_SECRET_KEY="$(cat "$out/secret.key")" \
CELLA_OIDC_ISSUERS="$issuer" \
CELLA_ADMIN_SUBJECTS="$issuer|dev" \
CELLA_AUTHORIZER_URL="$authorizer" CELLA_AUTHORIZER_TOKEN=stub-authorizer-token \
CELLA_EVENTS_URL="$sink" CELLA_EVENTS_SECRET="$(cat "$out/events.secret")" \
  "$out/cellad" serve > "$out/cellad.out" &
cellad=$!
pids+=("$cellad")

# 6. Where cellad listens: the line it prints once both listeners are bound,
# which names the addresses it holds and not the ones it was given.
listening=
for _ in $(seq 1 200); do
  listening=$(sed -n 's/.* listening public=\([^ ]*\) internal=\([^ ]*\) .*/\1 \2/p' "$out/cellad.out")
  [ -n "$listening" ] && break
  kill -0 "$cellad" 2>/dev/null || break
  sleep 0.1
done
[ -n "$listening" ] || { echo "cellad did not report its listeners" >&2; exit 1; }
cat "$out/cellad.out"
public=${listening% *}
internal=${listening#* }
url=http://$public
printf 'public %s\ninternal %s\n' "$public" "$internal" > "$out/cellad.addrs"

for _ in $(seq 1 200); do
  curl -fsS "http://$internal/readyz" > /dev/null 2>&1 && break
  sleep 0.1
done
curl -fsS "http://$internal/readyz" > /dev/null || {
  echo "cellad did not become ready" >&2; exit 1; }

mint="curl -fsS -X POST $issuer/mint -H 'content-type: application/json' -d '{\"sub\":\"dev\"}' | sed -e 's/.*\"token\":\"//' -e 's/\".*//'"

cat <<EOF

The stack is up.

  export CELLA_URL=$url
  export CELLA_TOKEN=\$($mint)

One sandbox:

  curl -fsS -X POST \$CELLA_URL/v1/sandboxes -H "Authorization: Bearer \$CELLA_TOKEN" \\
    -H 'Content-Type: application/json' \\
    -d '{"apiVersion":"cella.latere.ai/v1beta1","kind":"Sandbox","metadata":{"name":"first"},
         "spec":{"command":["sleep","3600"]}}'

The backend is native, which runs a command on this host and no image, so
the manifest names none. docs/native.md is what it can and cannot do.

The stubs: the issuer at $issuer, the authorizer at $authorizer, the sink
at $sink with its records at $sink/events, and the admission endpoint at
$admission, which no binary dials yet: the client and its variables are
spec 047's.

EOF

if [ "$smoke" -eq 1 ]; then
  token=$(eval "$mint")
  [ -n "$token" ] || { echo "the issuer minted nothing" >&2; exit 1; }
  code=$(curl -sS -o "$out/sandbox.json" -w '%{http_code}' -X POST "$url/v1/sandboxes?wait=1" \
    -H "Authorization: Bearer $token" -H 'Content-Type: application/json' \
    -d '{"apiVersion":"cella.latere.ai/v1beta1","kind":"Sandbox","metadata":{"name":"smoke"},
         "spec":{"command":["sleep","300"]}}')
  case "$code" in
    2*) ;;
    *) echo "the create answered $code: $(cat "$out/sandbox.json")" >&2; exit 1 ;;
  esac
  curl -fsS -X DELETE "$url/v1/sandboxes/smoke" -H "Authorization: Bearer $token" > /dev/null
  echo "the stack created and deleted one sandbox"
  exit 0
fi

echo "Interrupt to stop both."
wait
