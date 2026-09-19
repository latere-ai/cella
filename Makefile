# SPDX-FileCopyrightText: 2026 Latere AI
# SPDX-License-Identifier: Apache-2.0

GO ?= go

.PHONY: build check clean fmt hooks run

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

# The server on loopback with its state under out/, the native backend
# selected because it needs no cluster and no container engine. It has no
# isolation and runs only trusted development commands.
RUN_DIR = $(CURDIR)/$(OUT_DIR)/run
# Run the server on loopback. cellad verifies a token from an issuer you
# list, so CELLA_OIDC_ISSUERS names one; the stub issuer that makes this
# self-contained is the test stubs spec's and is not built. The signing
# key is generated once under out/run/ and kept, so a restart does not
# invalidate the tokens of the last one.
run: build
	@mkdir -p $(RUN_DIR)
	@test -s $(RUN_DIR)/token.pem || openssl genrsa -out $(RUN_DIR)/token.pem 2048 2>/dev/null
	@test -n "$(CELLA_OIDC_ISSUERS)" || { \
		echo "make run needs CELLA_OIDC_ISSUERS=<issuer url>: cellad verifies every caller"; \
		echo "and there is no anonymous access. An http:// issuer off loopback also needs"; \
		echo "CELLA_OIDC_INSECURE_ISSUERS."; exit 1; }
	CELLA_DATA_DIR=$(RUN_DIR) CELLA_RUNTIME=native CELLA_ALLOW_UNSAFE_NATIVE=true \
	CELLA_PUBLIC_ADDR=127.0.0.1:8080 CELLA_INTERNAL_ADDR=127.0.0.1:8081 \
	CELLA_PUBLIC_URL=http://127.0.0.1:8080 \
	CELLA_TOKEN_KEY="$$(cat $(RUN_DIR)/token.pem)" \
		$(OUT_DIR)/$(SERVICE)

fmt:
	gofmt -w $$(git ls-files '*.go')

# Point git at the delegating hooks. Per clone, so it is a target.
hooks:
	chmod +x .githooks/*
	git config core.hooksPath .githooks

# Remove the build output and the local state. Local state is disposable.
clean:
	rm -rf $(OUT_DIR)
