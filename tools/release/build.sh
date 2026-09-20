#!/usr/bin/env bash
# SPDX-FileCopyrightText: 2026 Latere AI
# SPDX-License-Identifier: Apache-2.0
#
# Builds what a release carries, from one checkout (spec 014): cellad and
# cella for the four os/arch pairs the project supports, with the -ldflags
# of spec 002 setting internal/version, and one archive each. The two Linux
# cellad binaries are also left unarchived under dist/, the very bytes the
# archives carry, for Dockerfile.ci to copy, so the image and the archive
# are one compilation:
#
#	tools/release/build.sh v0.2.0 [DIST]
#
# Every build is CGO_ENABLED=0 and -trimpath, so the bytes depend on the
# source and the toolchain and on nothing else.
set -euo pipefail

tag=${1:?usage: build.sh TAG [DIST]}
dist=${2:-dist}
module=$(go list -m)
commit=$(git rev-parse --short HEAD 2>/dev/null || echo none)
date=$(date -u +%Y-%m-%dT%H:%M:%SZ)
ldflags="-X $module/internal/version.Version=$tag -X $module/internal/version.Commit=$commit -X $module/internal/version.Date=$date"

mkdir -p "$dist"
for os in linux darwin; do
  for arch in amd64 arm64; do
    # cellad, the server: the image copies the Linux bytes, so they are
    # kept beside the archive that carries them.
    CGO_ENABLED=0 GOOS="$os" GOARCH="$arch" go build -trimpath -ldflags "$ldflags" -o "$dist/cellad" ./cmd/cellad
    if [ "$os" = linux ]; then
      cp "$dist/cellad" "$dist/cellad_linux_$arch"
    fi
    tar -C "$dist" -czf "$dist/cellad_${tag}_${os}_${arch}.tar.gz" cellad
    rm "$dist/cellad"
    echo "build: $dist/cellad_${tag}_${os}_${arch}.tar.gz"

    # cella, the client of spec 011: an archive per platform and nothing
    # unarchived, because no image carries it.
    CGO_ENABLED=0 GOOS="$os" GOARCH="$arch" go build -trimpath -ldflags "$ldflags" -o "$dist/cella" ./cmd/cella
    tar -C "$dist" -czf "$dist/cella_${tag}_${os}_${arch}.tar.gz" cella
    rm "$dist/cella"
    echo "build: $dist/cella_${tag}_${os}_${arch}.tar.gz"
  done
done
