#!/usr/bin/env bash
# SPDX-FileCopyrightText: 2026 Latere AI
# SPDX-License-Identifier: Apache-2.0
#
# The kind half of the end-to-end bar, the `kind-stack` suite of
# .lateregate.yaml: one cluster, the three images, the stub issuer, the
# authorizer and the sink beside the control plane, and every test that
# needs a real kubelet run against that same cluster.
#
#   tools/kind/e2e.sh
#
# kind, kubectl and a container engine must be on PATH; `lateregate e2e`
# puts the first two there at pinned versions. The cluster is brought up by
# the overlay's own up.sh, the kind tier, the Kubernetes driver's cluster
# cases and the contract of spec 015 run against it, and docs/install.md is
# walked last because its final block deletes the cluster. A named test that
# did not pass fails the script even when go test exited 0, which it does
# when a test skips.
#
# The kubeconfig is a file of this run's own, so a laptop's clusters are
# neither read nor written.
set -euo pipefail

root=$(cd "$(dirname "$0")/../.." && pwd)
cd "$root"

# The overlay's scripts and kind read the engine from CELLA_DOCKER and
# KIND_EXPERIMENTAL_PROVIDER. Docker is the default, and podman stands in
# where it is the only engine, as on a Mac.
if [ -z "${CELLA_DOCKER:-}" ]; then
  if command -v docker > /dev/null 2>&1; then
    CELLA_DOCKER=docker
  elif command -v podman > /dev/null 2>&1; then
    CELLA_DOCKER=podman
    export KIND_EXPERIMENTAL_PROVIDER=podman
  else
    echo "kind-stack: neither docker nor podman is on PATH" >&2
    exit 1
  fi
fi
export CELLA_DOCKER

cluster=cella
logs=$(mktemp -d)
export KUBECONFIG="$logs/kubeconfig"

# On any failure the cluster's state is printed first, since the cluster is
# deleted right after and the log is all that is left of it.
finish() {
  status=$?
  if [ "$status" -ne 0 ]; then
    kubectl -n cella get all || true
    kubectl -n cella describe pod -l app.kubernetes.io/name=cellad || true
    kubectl -n cella logs deploy/cellad --tail=200 || true
    kubectl -n cella logs deploy/cellad -c stubs --tail=200 || true
    kubectl -n cella logs deploy/cellad-egress --tail=200 || true
  fi
  kind delete cluster --name "$cluster" > /dev/null 2>&1 || true
  rm -rf "$logs"
  exit "$status"
}
trap finish EXIT

# passed fails unless the log of a go test run holds the line that reports
# the named test as passed.
passed() {
  if ! grep -q -- "--- PASS: $2" "$logs/$1.txt"; then
    echo "kind-stack: $2 did not pass" >&2
    exit 1
  fi
}

# The cluster, the stubs and the control plane. up.sh builds the three
# images, creates the cluster, loads the images and prints the URL and the
# token of the stack on stdout.
stack=$(bash deploy/examples/kind-stubs/up.sh -name "$cluster")
while IFS='=' read -r name value; do
  case "$name" in
    CELLA_TEST_URL | CELLA_TEST_TOKEN) export "$name=$value" ;;
  esac
done <<< "$stack"
: "${CELLA_TEST_URL:?up.sh printed no URL}"
: "${CELLA_TEST_TOKEN:?up.sh printed no token}"
export CELLA_TEST_SINK=http://localhost:30082
export CELLA_TEST_CLUSTER="$cluster"

# The tier runs before the document's walk, because the document ends by
# deleting the cluster: its last block is how an operator removes the
# installation, and the walk runs every block.
go test -tags=e2e -count=1 -v -timeout 30m -run '^TestCluster' ./test/kind/... 2>&1 | tee "$logs/kind.txt"
passed kind TestClusterLifecycle
passed kind TestClusterDial
passed kind TestClusterEgressBoundary
passed kind TestClusterNoLateralMovement
passed kind TestClusterMeshReachability
passed kind TestClusterWorkerNoInbound

# The Kubernetes driver's own cases against the same cluster, from here
# through the kubeconfig kind wrote. They run in a namespace of their own,
# because the suite's cleanup deletes every sandbox in the namespace it
# drove. The dial and port cases reach into a sandbox's ports; the home case
# proves the image's home made writable on a real kubelet (the fill, the
# claim's ownership under the non-root user, and a restart); the confined
# case reads the Pod baseline back from inside the running container (id,
# /proc/1/status, a refused mount and a refused write).
(
  export CELLA_TEST_KUBECONFIG="$KUBECONFIG"
  export CELLA_TEST_NAMESPACE=cella-driver
  export CELLA_TEST_IMAGE=docker.io/library/alpine:3.22
  go test -count=1 -v -timeout 15m -run '^TestClusterConformance$/^(DialReachesAPort|PortsReportListening)$' ./runtime/k8s 2>&1 | tee "$logs/driver-kind.txt"
  go test -count=1 -v -timeout 10m -run '^TestClusterTheImagesHomeIsWritable$' ./runtime/k8s 2>&1 | tee "$logs/driver-home.txt"
  go test -count=1 -v -timeout 10m -run '^TestClusterPodIsConfined$' ./runtime/k8s 2>&1 | tee "$logs/driver-confined.txt"
)
passed driver-kind TestClusterConformance/DialReachesAPort
passed driver-kind TestClusterConformance/PortsReportListening
passed driver-home TestClusterTheImagesHomeIsWritable
passed driver-confined TestClusterPodIsConfined

# The contract of spec 015 against the cluster: the same cases, on the
# Kubernetes environment, with the subjects minted at the stub issuer's node
# port and the capabilities that driver declares. The stack names a display
# image, so the desktop and its input are declared and the computer-use case
# runs on the cluster; it names the gateway's Pods, so egress is declared and
# the enforcement case runs against the stack's upstream.
go test -tags=e2e -count=1 -v -timeout 30m -run '^TestContract$' ./test/conformance -args \
  -url "${CELLA_TEST_URL}" -issuer http://localhost:30081 -sink "${CELLA_TEST_SINK}" \
  -image docker.io/library/alpine:3.22 -capabilities files,pool,mesh,attach,dial,display,input,egress \
  -display-image cella-display:dev -upstream cella-upstream.cella.svc:80 \
  -known known.json 2>&1 | tee "$logs/conformance-kind.txt"
passed conformance-kind TestContract

# The stack's check Job ran when up.sh applied the overlay, before the tiers
# above, and its time to live can end while the walk reads it. The walk
# applies the overlay as an operator's first install does, so it starts
# without that Job and waits on one of its own.
kubectl -n cella delete job cellad-check --ignore-not-found --wait

# The document is a program, and this is the run that proves it: the same
# blocks an operator pastes, against the cluster above, with the stub issuer
# as this installation's issuer.
CELLA_INSTALL_IMAGE=cellad:dev \
  CELLA_INSTALL_MANIFESTS=deploy \
  CELLA_INSTALL_OVERLAY=examples/kind-stubs \
  CELLA_INSTALL_ISSUER=http://127.0.0.1:9080 \
  CELLA_INSTALL_ADMIN='http://127.0.0.1:9080|dev' \
  CELLA_INSTALL_TOKEN="${CELLA_TEST_TOKEN}" \
  bash tools/docs/run-blocks.sh docs/install.md
