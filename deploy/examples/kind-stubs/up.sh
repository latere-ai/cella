#!/usr/bin/env bash
# SPDX-FileCopyrightText: 2026 Latere AI
# SPDX-License-Identifier: Apache-2.0
#
# The kind stack of spec 049: a cluster, the three images, this overlay, and
# the URL and token to drive it with.
#
#   up.sh [-name <cluster>] [-manifests <deploy tree>]
#
# It prints two lines a caller reads:
#
#   CELLA_TEST_URL=http://localhost:30080
#   CELLA_TEST_TOKEN=<a token from the stub issuer>
#
# -manifests names the deploy tree to apply, which is this checkout's by
# default and the unpacked archive's when a release is being walked. The
# images are built from this checkout unless CELLA_KIND_BUILD=0, which is
# what a pipeline that pulled the published ones sets.
#
# down.sh deletes the cluster. Nothing outside the cluster and the three
# images is touched: the state is under the cluster and goes with it.
set -euo pipefail

here=$(cd "$(dirname "$0")" && pwd)
root=$(cd "$here/../../.." && pwd)
name=cella
manifests=$root/deploy
while [ $# -gt 0 ]; do
  case "$1" in
    -name) name=${2:?-name takes a cluster name}; shift 2 ;;
    -manifests) manifests=${2:?-manifests takes a directory}; shift 2 ;;
    *) echo "usage: $0 [-name <cluster>] [-manifests <deploy tree>]" >&2; exit 2 ;;
  esac
done
manifests=$(cd "$manifests" && pwd)
overlay=$manifests/examples/kind-stubs
docker=${CELLA_DOCKER:-docker}
work=$(mktemp -d)
trap 'rm -rf "$work"' EXIT

# 1. The three images under the tag this overlay names. The stubs image is
# a test image and is never part of an installation. The display image is
# the desktop the overlay's CELLA_K8S_DISPLAY_IMAGE names, so the
# computer-use case runs against the cluster rather than skipping.
if [ "${CELLA_KIND_BUILD:-1}" = "1" ]; then
  "$docker" build -t cellad:dev "$root" >&2
  "$docker" build -f "$root/Dockerfile.stubs" -t cella-stubs:dev "$root" >&2
  "$docker" build -f "$root/images/display/Dockerfile" -t cella-display:dev "$root" >&2
fi

# 2. The cluster, left alone where one of this name is already up.
if ! kind get clusters 2>/dev/null | grep -qx "$name"; then
  kind create cluster --name "$name" --config "$overlay/kind.yaml" >&2
fi
kind load docker-image cellad:dev cella-stubs:dev cella-display:dev --name "$name" >&2

# 3. The namespace, the one mandatory Secret, and the overlay.
kubectl apply -f "$manifests/bootstrap/namespace.yaml" >&2
openssl genrsa -out "$work/token.pem" 2048 2>/dev/null
kubectl -n cella create secret generic cellad-token \
  --from-file=CELLA_TOKEN_KEY="$work/token.pem" --dry-run=client -o yaml | kubectl apply -f - >&2
kubectl apply -k "$overlay" >&2
kubectl -n cella rollout status deploy/cellad --timeout=300s >&2

# 4. A caller token from the stub issuer, minted through the node port. The
# iss it carries is the loopback address the control plane verifies against,
# which is the whole point of the issuer running in that Pod.
# The node port is programmed a moment after the rollout reports done, so
# the first request retries a connection kube-proxy resets or refuses.
token=$(curl -fsS --retry 10 --retry-delay 1 --retry-all-errors -X POST http://localhost:30081/mint \
  -H 'content-type: application/json' -d '{"sub":"dev"}' \
  | sed -e 's/.*"token":"//' -e 's/".*//')
[ -n "$token" ] || { echo "the stub issuer minted nothing" >&2; exit 1; }

echo "CELLA_TEST_URL=http://localhost:30080"
echo "CELLA_TEST_TOKEN=$token"
