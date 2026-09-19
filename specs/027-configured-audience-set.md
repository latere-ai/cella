---
title: Configured audience set
status: complete
track: foundations
depends_on: [specs/006-identity.md]
affects: [internal/config, internal/auth, cmd/cellad]
effort: small
created: 2026-09-19
updated: 2026-09-19
author: changkun
---

# Configured audience set

## Overview

An installation may accept its own audience and a platform audience through
the same OIDC verifier. The current comma-list refusal prevents this.

## Design

`CELLA_OIDC_AUDIENCE` accepts comma-separated, trimmed, distinct audiences.
An empty entry is a configuration error. The default remains `cella`.
The first entry is the single audience of workload and environment tokens
signed by this installation; external callers may address any configured
entry. Local tokens still require the first audience. No legacy endpoint
or token exchange is introduced.

## Acceptance criteria

| Criterion | Test |
|---|---|
| A configured set is parsed and invalid entries fail startup | `TestConfiguredAudienceSet` |
| External tokens for either configured audience verify; unrelated tokens fail | `TestAudienceSetIdentityEndToEnd` |
| Minted environment tokens keep the first audience and verify locally | `TestAudienceSetIdentityEndToEnd` |

## Outcome

Implemented. The configuration regression failed against the old list refusal
and passed after the change. Identity end-to-end tests accept either external
audience, reject unrelated audiences, and retain the local-token audience
boundary. The configuration, identity, and server suites pass.
