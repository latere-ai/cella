---
title: "Environments a subject owns: the owner policy and the authorizer deciding a subject's own worker environments on its owner, mode and worker limit, keys with a lifetime, `environment.serve` asked when a worker registers and again on a TTL, revoking a key or deleting an environment ending its live stream and its sandboxes, `spec.workers.max`, and a driver that enforces its own allowlist standing in for a gateway"
status: drafted
track: core
depends_on:
  - specs/006-identity.md
  - specs/018-egress-and-secrets.md
  - specs/020-scheduling-and-sets.md
  - specs/021-data-plane-workers.md
  - specs/082-the-worker-as-a-library.md
affects: [manifest/v1/, authorizer/, internal/auth/, internal/api/, runtime/remote/, controller/, internal/config/, api/openapi.yaml, docs/api.md, docs/workers.md, CHANGELOG.md]
effort: medium
created: 2026-10-18
updated: 2026-10-18
author: changkun
---

# Environments a subject owns

## Overview

Environments are an operator's today. Only an admin acts on one or mints
its key, every other subject uses the default one, and an environment
key lasts a year by default. That fits a fleet an operator runs. It does
not fit a person who connects their own computer: the environment is
theirs, the authorizer should decide it the way it decides their
sandboxes, and removing it must stop the computer from serving it at
once, even if its key was copied.

This spec lets a subject own and act on a worker environment, adds one
question the control plane asks about a worker while it serves, and
closes the gaps a person's worker exposes: a revoked key whose stream
stays open, a delete refused while sandboxes run, and a gateway that a
computer with its own enforcement does not have.

## Current state

- An environment is named by its creator: `metadata.name` is a DNS
  label, unique across the control plane, and becomes `status.id`
  (`controller/environment.go:267-268`, `manifest/validate.go:44`).
- `status.owner` is the creating subject, set at create
  (`internal/api/environments.go:114`, `:156`). The default environment's
  owner is `controller` (`controller/environment.go:188-193`).
- The authorizer's resource for an environment carries `name`, `owner`,
  `isolation` and `labels` (`internal/auth/resource.go:105-107`), and
  neither `mode` nor the worker limit. A key mint carries no lifetime.
- The owner policy ignores the owner: every `environment.*` action but
  `use` on the default environment is `ReasonAdminOnly`
  (`internal/auth/policy.go:122-130`).
- An environment key (`sub: environment:<id>`) reaches only its
  environment's registration, operations and egress routes, and the
  authorizer is not asked about it. It lasts `CELLA_ENVIRONMENT_KEY_TTL`,
  8760 hours by default.
- `DELETE /v1/environments/{id}/keys/{jti}` refuses the key's next
  request, and a stream it opened stays open.
- Any number of workers may register on one environment.
- Deleting an environment that has sandboxes placed on it is refused with
  `ErrEnvironmentInUse`, answered `phase_conflict`
  (`controller/environment.go:326-337`). An allowed delete releases the
  driver, which closes the environment's streams.
- `controller/egress.go:303-304` projects the one configured gateway's
  proxy and reverse addresses into every sandbox, whatever its
  environment. `needsGatewayFor` (`controller/egress.go:220`) makes the
  controller refuse any egress mode but `open` while no gateway is
  connected (`controller/controller.go:612`), and `egressCondition`
  (`controller/egress.go:316`) marks `EgressEnforced` `False` with
  `ReasonNoGateway` when no gateway holds the sandbox's map. The gateway
  hub serves the default environment alone (spec 021).

## Design

### An owner who acts

The owner already exists. What changes is who may act on it.

With an authorizer, `environment.create`, `read`, `update`, `delete`,
`key` and `use` stay its decisions, and the resource it receives gains
`mode` and `workers` (the value of `spec.workers.max`, 0 when unset), so
it can allow a subject to create a `mode: worker` environment of its own
with one worker and act on it.

The owner policy (no authorizer) keeps environments admin-only, unless
`CELLA_OWNED_ENVIRONMENTS=worker` is set: then any subject may create a
`mode: worker` environment, and acts on the environments it owns as it
acts on its sandboxes. `inprocess` environments stay admin-only. Spec
002's table lists the variable.

Naming is unchanged: the creator picks the name, and an authorizer that
wants names nobody can take first may require a form, such as a random
suffix.

### Keys with a lifetime

`POST /v1/environments/{id}/keys` takes an optional `ttl`, at most
`CELLA_ENVIRONMENT_KEY_TTL`. The `environment.key` question carries the
lifetime the key will get as a resource field `ttl` in seconds, the
default when none was asked, so an authorizer can refuse a long one. A
subject that owns the environment lists and revokes its keys.

### `environment.serve`

A new action, asked with the environment key's subject and claims:

- when a worker registers, where a refusal answers 403 `serve_refused`;
- again while its stream lives, every decision TTL, capped at five
  minutes, where a refusal closes the stream with 1008 and
  `serve_refused`.

When the authorizer gives no decision on a recheck, the stream is kept
for one more TTL and then closed. The owner policy allows it for a key of
the environment that is not revoked.

The resource is the environment (`id`, `name`, `owner`, `mode`,
`workers`, `labels`), and the request carries the worker's registration
facts (driver, isolation, `host`). The authorizer can therefore refuse a
removed environment, a closed account, or a worker whose isolation is
not what the owner chose.

### Revocation reaches the stream

The hub keeps the `jti` each stream registered with. Revoking that key
closes the stream within one heartbeat interval (15 seconds), with 1008
and `key_revoked`.

### Deleting a worker environment

A `mode: worker` environment that a subject owns is deleted even with
sandboxes placed on it: the delete revokes all its keys, closes its
streams with `serve_refused`, and deletes its sandboxes' records. The
worker, told why its stream closed, stops and deletes every sandbox its
driver lists before `Run` returns (spec 082), which ends their detached
stages too. A worker that was not connected finds none of them desired
at its next start and deletes what its driver lists. Other environments
keep `ErrEnvironmentInUse`.

### `spec.workers.max`

`Environment.spec.workers.max` limits the live workers. A registration
beyond it is refused with 409 `worker_limit`, except a registration with
the same key `jti` as a live stream whose host facts (OS, architecture,
grants, network) are the same: that one replaces the old stream, because
a worker reconnecting after a dropped connection must not wait 45
seconds. The old stream is closed with 1008 and `worker_replaced`, and
`environment.worker_replaced` is emitted with both registrations' facts.

A same-`jti` registration whose host facts differ is refused with
`worker_limit` while the live stream lasts, and emits the same event: a
copied key used from another computer cannot displace the one serving.
A copied key can still serve while the real computer is away. Binding
the key to a key held on the host closes that, and is not in this
spec.

### A driver that enforces its own allowlist

A driver that declares `EgressLocal` (spec 081) enforces an allowlist
itself. For a sandbox on an environment other than the default one,
whose capabilities declare it:

- `needsGatewayFor` is false for `none` and `allowlist`, so no connected
  gateway is required;
- `egressCondition` marks `EgressEnforced` `True` with reason
  `DriverEnforced`;
- no proxy variable, reverse address or gateway CA is projected.

A sandbox on any environment other than the default one gets no
projection from the default environment's gateway, whatever its driver,
since the hub serves the default environment alone. A manifest that
mounts a Secret on such an environment is refused with
`egress_gateway_unavailable`, as it is today with no gateway.

## Not in this spec

- Keys bound to a key held on the host (proof of possession).
- One gateway hub per environment (spec 021's open item).
- What a hosted plane decides: its own authorizer's.

## Acceptance criteria

1. `TestOwnedEnvironmentUnderOwnerPolicy`: with
   `CELLA_OWNED_ENVIRONMENTS=worker`, a subject creates a worker
   environment, mints and revokes its keys, and is refused another
   subject's environment and any `inprocess` one. Without the setting,
   both are admin-only.
2. `TestAuthorizerSeesModeWorkersAndTTL`: the authorizer's request for
   each `environment.*` action carries `mode` and `workers`, and a key
   mint carries `ttl`, through the JSON round trip.
3. `TestServeRefusedAtRegistration` and `TestServeRefusedOnRecheck`: a
   refusal answers `serve_refused`, and a live stream closes within one
   TTL once the authorizer starts refusing.
4. `TestRevokedKeyClosesItsStream`: revoking the key closes the stream
   within 15 seconds with `key_revoked`.
5. `TestDeleteWorkerEnvironmentWithSandboxes`: deleting an owned worker
   environment with a running sandbox succeeds, closes its stream,
   revokes its keys, and the worker's driver has no sandbox left when
   `Run` returns; an `inprocess` environment with a sandbox is still
   refused with `phase_conflict`.
6. `TestWorkersMax`: a second key's registration beyond the limit is
   refused with `worker_limit`; a same-`jti` registration with the same
   host facts replaces the stream, closing the old one with
   `worker_replaced`; one with different host facts is refused; and
   `environment.worker_replaced` is emitted in both cases.
7. `TestEgressLocalNeedsNoGateway`: with no gateway connected, a sandbox
   asking `allowlist` on an `EgressLocal` worker environment is created,
   is `EgressEnforced` `True` with `DriverEnforced`, and has no proxy
   variable, reverse address or gateway CA, while the default
   environment's sandboxes keep their projection and its `allowlist`
   sandboxes still need a gateway.
8. The conformance suite (spec 015) covers `serve_refused`,
   `worker_limit`, `worker_replaced` and `key_revoked`, and `docs/api.md`'s
   code table lists them.
