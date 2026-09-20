# SPDX-FileCopyrightText: 2026 Latere AI
# SPDX-License-Identifier: Apache-2.0

GO ?= go

.PHONY: build check clean fmt hooks run test tier-podman tier-kind

# The whole bar. Every gate lives in latere.ai/x/ci-gate, pinned as a tool
# in go.mod and configured in .lateregate.yaml, so this target is a name for
# `go tool lateregate` and nothing else. One gate at a time: `go tool
# lateregate cover`. The plan: `go tool lateregate list`.
check:
	@$(GO) tool lateregate

.DEFAULT_GOAL := check

OUT_DIR := out
SERVICE := cellad
MODULE := $(shell $(GO) list -m)

# Build metadata, deferred so the git and date calls run only for a build. A
# dirty tree marks the commit, because a binary built from uncommitted
# changes cannot be reproduced from its commit.
VERSION ?= $(shell git describe --tags --always 2>/dev/null || echo dev)
COMMIT ?= $(shell git rev-parse --short HEAD 2>/dev/null || echo none)
DIRTY = $(shell test -n "$$(git status --porcelain 2>/dev/null)" && echo -dirty)
BUILD_DATE ?= $(shell date -u +%Y-%m-%dT%H:%M:%SZ)
VERSION_PKG = $(MODULE)/internal/version
LDFLAGS = -X $(VERSION_PKG).Version=$(VERSION) \
          -X $(VERSION_PKG).Commit=$(COMMIT)$(DIRTY) \
          -X $(VERSION_PKG).Date=$(BUILD_DATE)

build:
	@mkdir -p $(OUT_DIR)
	CGO_ENABLED=0 $(GO) build -trimpath -ldflags '$(LDFLAGS)' \
		-o $(OUT_DIR)/$(SERVICE) ./cmd/$(SERVICE)
	@echo "built $(OUT_DIR)/$(SERVICE)"

# The development stack: cella-stubs and `cellad serve` on loopback, wired
# to each other, with the command that mints a caller token printed at the
# end. It needs no issuer of your own: the stub issuer is one of the four
# roles. The bootstrap is tools/run/up.sh and this target is its name.
#
# The state lives under out/run/ and `make clean` removes it. A second
# clone runs beside this one with CELLA_RUN_PORT=8090 make run.
run:
	@tools/run/up.sh

# The tiers of spec 012. The unit tier is the gate's own suite; the other
# two need a substrate beside them and say what they need when it is not
# there.
# The test gate alone; `make check` runs the whole bar.
test:
	@$(GO) tool lateregate test

# The container driver's suite against a rootless engine. It skips where no
# socket answers, which is why the release pipeline reads the log for the
# pass rather than the exit code.
tier-podman:
	$(GO) test -count=1 -v -run '^TestPodman' ./runtime/podman/...

# The kind stack: the overlay with the stubs beside cellad, the lifecycle
# through the API, and the check Job. It brings the cluster up and takes it
# down; against a cluster somebody else brought up, set CELLA_TEST_URL and
# CELLA_TEST_TOKEN instead.
tier-kind:
	CELLA_TEST_KIND=1 $(GO) test -tags=e2e -count=1 -v -timeout 30m -run '^TestCluster' ./test/kind/...

fmt:
	gofmt -w $$(git ls-files '*.go')

# Point git at the delegating hooks. Per clone, so it is a target.
hooks:
	chmod +x .githooks/*
	git config core.hooksPath .githooks

# Remove the build output and the local state. Local state is disposable.
clean:
	rm -rf $(OUT_DIR)
