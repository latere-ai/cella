---
title: "Rolling replicas: one writer lease gates the controller, every replica answers the API by forwarding to the writer, the lease is handed off on SIGTERM, and readiness means can serve"
status: drafted
track: core
depends_on:
  - specs/005-lifecycle-controller.md
  - specs/008-api.md
  - specs/010-state.md
  - specs/014-release-and-installation.md
  - specs/017-observability.md
  - specs/018-egress-and-secrets.md
  - specs/021-data-plane-workers.md
  - specs/.archive/043-postgres-store.md
  - specs/.archive/072-create-answers-at-once.md
affects: [cmd/cellad/, controller/, internal/store/, internal/api/, internal/egressd/, internal/worker/, internal/config/, deploy/, docs/kubernetes.md, docs/configuration.md]
effort: large
created: 2026-09-26
updated: 2026-09-26
author: changkun
---

# Rolling replicas

## Overview

A plane that runs `cellad serve` on Kubernetes with the Postgres store runs
one replica with `strategy: Recreate`, because two processes would each
drive the same environments. Every rollout therefore leaves no control
plane for the length of a process start: callers get connection refused,
a proxy in front answers 502, and every terminal, screen and log stream
ends. This spec designs running two or more replicas with a rolling
update in which no unary request fails.

The design follows from what the code does today, so that comes first.

### What the leases guard

Design 010 names the leases `reaper`, `journal`, `scheduler`,
`environments` and `pool:<environment>`. Each is taken by one background
loop, once per pass, and guards that loop alone:

| Lease | Taken by | Guards |
|---|---|---|
| `reaper` | `Controller.tick`, `controller/reaper.go:154-155` | the lifecycle rules, the lost rule and recovery, the token rotation, the revocation sweep and journal retention |
| `scheduler` | `Controller.scheduleTick`, `controller/scheduler.go:217` | placing queued sandboxes and `realize`, steps 3 onward of a create ([[072-create-answers-at-once]]) |
| `environments` | `Controller.environmentTick`, `controller/environment.go:395-396` | writing each environment's phase |
| `pool:<environment>` | `Controller.poolTick`, `controller/pool.go:67` | the prewarmed pool's refill |
| `journal` | `Deliverer.Pass`, `internal/events/deliver.go:168` | posting journal records to the sink |

`LocalLease` grants every lease (`controller/reaper.go:89`). The Postgres
lease renews at a third of its 15 second term and is released only in
`Store.Close` (`internal/store/postgres/postgres.go:283-302`), which
`cellad serve` defers to its very end (`cmd/cellad/main.go:276`), after
the 3 second drain delay and the server shutdown of up to 60 seconds
(`cmd/cellad/main.go:59-60`, `:599-609`). The loops keep renewing through
the whole drain.

### What the API handlers drive

The handlers drive environments directly. Only the plain create is
behind a loop:

| Handler path | What it does on the request | Where |
|---|---|---|
| start, stop | `d.Start` or `d.Stop` under the controller's mutex | `controller/controller.go:1137`, `:1146` |
| delete | writes `Deleting`, cascades, then `d.Delete`, which on Kubernetes waits for the Pod and the claim | `controller/controller.go:1155`, `:1186` |
| create that adopts a pool entry | pushes the boundary, mints the identity and calls `d.Update` with the adoption before answering | `controller/controller.go:683`, `:718` |
| create otherwise | writes desired state and wakes the scheduler loop in the same process; the loop's `realize` calls `d.Create` | `controller/controller.go:669-670`, `:812` |
| apply of a sandbox | pushes the new boundary to the gateways this process holds | `controller/controller.go:1337` |
| secret create, update, delete | `repushMounts` pushes every mounting sandbox's map to this process's gateways | `controller/secret.go:250` |
| read, list | `d.Inspect` per sandbox, in `refresh` | `controller/controller.go:1019`, `:1033` |
| exec, attach, files, logs, dial, ports, display, screenshot, screen, input | the driver's data plane call per request | `controller/controller.go:1231`, `controller/operations.go`, `controller/display.go` |

### Why the lease is not the blocker

The controller is authoritative in memory and treats the store as a
write-through record. `Open` reads every sandbox once
(`controller/controller.go:250`), every secret once (`:310`) and every
environment once (`:351`), and every read and every check after that is
of the process's own maps: `Get` and `List` (`:986`, `:1009`),
`ListSecrets` and the name lookup (`controller/secret.go:152`, `:167`),
the owner's count and the name check of a create, the queue, capacity,
`mountedBy`, the spawn tree. The store adapter caches each row's version
(`internal/store/controller.go:36`) and makes every write conditional on
it (`:155`). Two replicas that each opened a controller would not see
each other's creates, would answer lists that disagree, would refuse
each other's rows with a version conflict, and could hold different loop
leases, the reaper on one and the scheduler on the other, each loop
reading its own stale map. A second replica today is unsafe whatever the
lease does. Design 010 promises replicas that share one set of desired
state; the read-once model is the gap.

### Process-local state

What a replica holds that another does not see:

| State | Where | Held by the writer alone in this design |
|---|---|---|
| sandboxes, secrets, environments and their drivers | `controller/controller.go:156-158`, `:186` | yes; a standby opens no controller |
| row versions | `internal/store/controller.go:36` | yes; reloaded at promotion |
| the lost rule's grace, attempts and retry | `controller/controller.go:173-175` | yes; restart semantics, as today |
| activity coalescing | `controller/controller.go:166` | yes; the first touch after a promotion stamps at once |
| each environment's last answer | `controller/controller.go:217` | yes; the offline window restarts, as after a restart |
| creates in flight and their callers' contexts | `controller/controller.go:222`, `:228` | yes; a create placed and not realized is taken by the next writer as after a restart, its start recorded as the control plane's own act |
| the write notification and the scheduler's wake | `controller/controller.go:202`, `:232` | yes |
| the egress hub: the maps, the gateway connections, the ring of connection records, the authority learned from the first gateway | `internal/api/egress.go:54-57`, `:119`, `:126` | yes; gateways reconnect to the new writer and are sent the snapshot; the records ring starts empty |
| the worker hub's streams | `runtime/remote/hub.go` | yes; workers reconnect |
| the journal's subscriptions for a following feed | `internal/store/store.go:74`, in this process only | yes; every feed is served by the writer |

Store-backed and shared already: the revocation list, the environment key
registry, the spawn ledger, the journal, secret values, and the token
signing key, which every replica loads from the same configuration. The
authorizer's decision cache is per process and bounded by its TTL.

## Design

### Two roles and one writer lease

A process is the **writer** or a **standby**. The writer holds a new lease,
`writer`, and is the only process that opens a controller: it runs the
loops, holds the egress and worker hubs, and answers every API request. A
standby opens no controller, runs no loop, and answers every API request
by forwarding it to the writer. Every replica accepts every request; what
differs is where it is executed.

The loop leases stay as they are, taken per pass by loops that now run only
on the writer. They are subordinate to `writer` and keep their meaning for
a deployment that runs one process. `journal` stays independent: delivery
reads and acknowledges rows in the store, holds no process state, and can
run on any replica.

The lease row gains a nullable `address` column, written by the writer on
acquire and on every renewal: `CELLA_ADVERTISE_URL`, the URL of this
process's public listener as other replicas reach it (on Kubernetes,
`http://` and the Pod's IP from the downward API, and the public port). A
process without `CELLA_ADVERTISE_URL` and with a durable store runs as the
writer once it holds the lease and never forwards, which is today's
single-replica deployment. A process whose store is not durable is always
the writer, as today: two such processes cannot detect each other (design
010).

### The standby

A standby serves locally what needs no controller: the probes, `/metrics`,
`/version`, the API document and the key set. Every other request on the
public listener, the environment key routes and every WebSocket included,
goes through a reverse proxy to the writer's `address`:

- The request is forwarded as it arrived: its method, path under
  `CELLA_BASE_PATH`, query, body, `Authorization`, `X-Request-Id` and
  `traceparent`. The standby verifies nothing; the writer does, so a
  forwarded request is authorized exactly as a direct one. `X-Forwarded-For`
  is appended. The authorizer is handed the connection's address, which is
  already a proxy's for a request that came through an ingress, so the
  standby's address in its place loses nothing.
- WebSocket upgrades are proxied as a byte stream after the upgrade.
- The writer's address is read from the `writer` row and cached until the
  row's expiry, and read again when a forward fails to connect.
- When no process holds `writer`, or the forward cannot connect, the
  request is held, not failed: its body is not read, and it is forwarded
  once a writer's row appears, for at most `CELLA_FORWARD_HOLD` (default
  15s). A forward that failed to connect is retried; one that reached the
  writer is never retried, since the writer may have acted on it. At the
  bound the answer is 503. Whether that is `driver_unavailable` or a new
  code, `control_plane_unavailable` with the sentence "The control plane is
  unavailable; retry shortly.", is a decision for review: a new code is
  one more row in design 008's table and the conformance error table, and
  says what happened.
- A standby tries to take `writer` every second. It reads the row first
  and attempts the conditional upsert only when the row is absent or its
  term lapsed, so a standby's poll takes no row lock while a writer holds
  the lease.

### Promotion

A standby that wins `writer` runs today's start path from the point the
store is open: `controller.Open` (which loads every row and its version),
`hub.Seed`, the loops, `api.New`, and then swaps the public handler from
the proxy to the API. The pieces before it, the store, telemetry, identity,
the runtime driver and its preflight, the hubs' construction, run at
process start in both roles, so a promotion costs the loads and nothing
else. Requests the process was holding as a standby are then served
locally. Promotion reuses the path every restart takes, which is the path
the tests already hold.

### Handoff on SIGTERM

The writer that receives SIGTERM hands off before it drains, and keeps
answering by forwarding while it drains:

1. The draining flag is set, so `/readyz` answers 503 and the Service stops
   sending it new connections. For `drainDelay` (3s) it keeps serving as
   the writer, and standbys keep forwarding to it, while the endpoint
   removal propagates.
2. It stops executing new requests: from here a request that reaches it,
   directly or forwarded, is held as a standby holds one.
3. It ends what would outlast the handoff: a held create (`?wait=1`,
   `awaitStart`) answers the sandbox as it stands, every following feed
   ends as it does at shutdown today, and every WebSocket it terminates
   is closed with 1001, going away.
4. It waits up to `CELLA_HANDOFF_TIMEOUT` (default 10s) for the unary
   requests in flight, then cancels the rest.
5. It stops the loops. `realize` already leaves a create it was cut out of
   placed and unrealized for the next writer, and the next writer's settle
   ends the identity this one minted (`controller/controller.go:752-830`).
6. It closes the controller, which closes the store adapter and releases
   every lease it holds, `writer` included.
7. A standby takes `writer` within its one second poll and promotes. The
   old process forwards the requests it holds to the new writer, finishes
   the server shutdown within `gracePeriod`, and exits.

The pause a caller sees is steps 2 to 7: the in-flight wait of step 4,
normally well under a second, the poll interval and the promotion. Unary
requests in the pause are held and answered, not refused.

A synchronous delete that step 4 cancels leaves its row `Deleting`, and
today nothing finishes such a row until a caller deletes again: the lost
rule skips it (`controller/recovery.go:77`) and so does token rotation
(`controller/reaper.go:314`). The reaper finishes every `Deleting` row at
its next tick: where the driver still has the object it deletes it again,
and where the driver no longer has it, because the cut came after the
driver's delete and before the row was forgotten, it ends the identity and
the ledger row and forgets the row. A crash mid-delete needs this as much
as a handoff does.

### Fencing

A writer that lost its lease without knowing it, across a network
partition or a pause longer than the term, must not write. Every
transaction of the controller's store adapter (`internal/store/controller.go`)
first reads the `writer` row `for share`, where `holder` is this process
and the term has not lapsed, and fails with `ErrNotWriter` when there is
no such row. The share lock holds a takeover's upsert until the write
commits, and a takeover that committed first fails the write, so no write
of a demoted writer lands after a successor's promotion read the rows.
The cost is one primary key read per write transaction on the same
connection. The fence covers the controller's writes: desired state,
status, secrets, environments, the observed index, the ledger and the
records written with them. The revocation list and the key registry are
not fenced: a revocation is monotonic, so a late one ends a token that
was meant to end, and a key minted by a writer that was just demoted is a
key an administrator asked for. A request that meets `ErrNotWriter` is
answered as a standby answers at the bound, and its caller's retry reaches
the new writer.

### Demotion is an exit

A writer that loses the lease exits with a non-zero status, and Kubernetes
restarts it as a standby. Tearing a controller down in place would have to
undo everything promotion built, while an exit reuses the restart path;
the fence covers the moment between the loss and the exit. The lease is
lost when the store reports another holder, or when no renewal has
succeeded for the term. One failed renewal statement is not a loss: the
renewal loop forgets a lease on one failure
(`internal/store/postgres/postgres.go:327-337`), and the next pass takes
it back while this process is still the holder.

### Readiness means can serve

`/readyz` stays the draining flag, the disk write test, the runtime's
check and the store's (`cmd/cellad/main.go:514`), in both roles. It never
reads the lease: a rolling update starts a new Pod and waits for it to be
ready before it stops an old one, and the old writer holds `writer` until
it stops, so a new Pod whose readiness waited for the lease would never be
ready and the rollout would never proceed. It does not read whether a
writer exists either: at a cold start nobody holds `writer` yet, and the
standby's bounded hold covers the seconds before one does.

### Why forwarding

The three ways to keep concurrent replicas safe, with what each costs here:

| Approach | What it takes | Cost |
|---|---|---|
| Every replica acts, with a lock per object | every read of the controller moves from its maps to the store or to a cache invalidated across replicas; name and count checks move into the store's unique index and `Count` inside the transaction; a per-object lock held across each driver call | the controller rewritten; a lock held across a driver call pins a database connection for the call's length, a Kubernetes delete included; cross-replica invalidation needs `LISTEN`, which a transaction pooler does not carry, or polling |
| Every replica acts, with start, stop and delete behind the reconciler | the same coherent reads as the row above, and the verbs written as desired state for a loop to apply, as the plain create already is | the same rewrite, and start and stop no longer answer the state they produced without a held answer |
| Forward to the writer | one lease, a proxy on standbys, promotion by the start path, a fence in the write transaction | one in-cluster hop for a request that lands on a standby; all execution on one process, which is today's capacity; a pause of about a second per handoff |

Forwarding is chosen: the load one process carries is what this control
plane serves today, the problem to solve is the gap during a rollout, and
forwarding solves it without changing what any route answers. The
reconciler row is the direction for a later spec that needs more than
one process's capacity. A narrower step before that is local data plane
serving on standbys, described under Not in this spec.

The store maps a unique violation to `store.ErrNameTaken`
(`internal/store/postgres/statements.go:658-661`) and a moved row to
`store.ErrVersionConflict`, and the sandbox path of the controller's
adapter maps neither to the controller's errors, so either answers 503.
With one writer neither is reachable across replicas; a design where
every replica writes has to map both first.

### Postgres connections

The worst case during a rollout is
`(replicas + maxSurge) x CELLA_DB_MAX_CONNS`, plus one direct connection
per starting process while its migrations run, since migrations run over
`CELLA_DB_URL` with a session lock whatever `CELLA_DB_POOL_URL` says. The
pool opens connections on demand and closes one idle for a minute
(`internal/store/postgres/postgres.go:45`), so a standby holds one: its
lease poll every second and its readiness statement share it. The writer
holds what it uses; its writes are serialized by the controller's mutex,
and the rest is the lease renewal, the readiness statement, delivery,
revocation reads on token verification, and the following feeds' reads.

Worked example: two replicas, a surge of one and `CELLA_DB_MAX_CONNS=3`
is at most nine during a rollout and one more for a migration, and about
four in steady state: three for the writer at its busiest and one for the
standby. An operator sizes `CELLA_DB_MAX_CONNS` against the database's
ceiling with this formula, not with the default of four.

Everything this design asks of the database is single-transaction: the
fence, the address lookup, the lease upsert. It works through a
transaction-mode pooler on `CELLA_DB_POOL_URL`, which is why the standby
polls rather than listening for a notification and why the fence is a
row lock and not a session advisory lock.

### Long-lived connections

A stream ends when the process that carries it goes, and a stream a
standby proxies ends when either the standby or the writer goes. A
rollout of two replicas therefore ends every stream at least once. What
each client does:

| Stream | At a handoff | How the client recovers |
|---|---|---|
| exec and attach WebSockets | closed with 1001 by the writer, or dropped where a standby proxied it | reconnect with backoff and attach again; the new attach is a new terminal session, and the old session's process and scrollback are gone |
| the screen WebSocket | same | reconnect; frames are stateless |
| dial and the port proxy | same | reconnect; a TCP connection inside is not carried over |
| `GET .../logs?follow=1` | ended | read again with `tail` or `since` |
| `GET /v1/events?follow=1` and an object's following feed | ended as at shutdown | resume from the newest `seq` held, which loses nothing inside retention ([[066-events-follow]]) |
| `?wait=1` create | answered with the sandbox as it stands | read the sandbox, or wait on its events |
| `?wait=1` exec | cut after `CELLA_HANDOFF_TIMEOUT` | the command's outcome is not reported; run it again or read what it wrote |
| the gateway's stream | dropped | the gateway keeps enforcing the maps it holds, reconnects, and is sent the snapshot on its hello |
| a worker's stream | dropped | the worker reconnects and registers; an operation in flight on the dropped stream fails |

Two changes keep the gateway's gap short, and are part of this design:

- The gateway's and the worker's reconnect loops never reset their backoff
  after a connection that lasted (`internal/egressd/sync.go:78-88`,
  `internal/worker/worker.go:123-139`). A gateway whose stream has dropped
  a few times in its life waits up to 30 seconds before it dials again, and
  every create that needs a gateway is refused with
  `egress_gateway_unavailable` in that time (`controller/controller.go:503`).
  The backoff resets after a connection that completed its hello.
- For `CELLA_EGRESS_ACK_TIMEOUT` after a promotion, a create that needs a
  gateway while none is connected waits for one to connect instead of
  being refused at once, since the gateways of the previous writer are
  reconnecting.

### Migrations in a rolling window

The new process runs its migrations at start while the old writer still
serves on the old binary, so every migration must leave the schema usable
by the previous release: add a table, a nullable column or an index; drop
or rename only in a later release, once no binary that reads the old
shape runs. `leases.address` is such an addition: the old binary's
statements name their columns. A test holds each embedded migration to
that rule, with `000004_drop_queue` as its one allowed exception from
before the rule.

### The first rollout

A binary before this design runs no standby, publishes no address, has no
fence and takes the loop leases, not `writer`. A new process started beside
it would take `writer` and open a second controller. The first rollout onto
this design is therefore a Recreate, and every rollout after it is rolling.

### What a plane sets

- `replicas: 2` or more, `strategy: RollingUpdate` with `maxUnavailable: 0`
  and `maxSurge: 1`, and a PodDisruptionBudget with `minAvailable: 1`.
- `CELLA_ADVERTISE_URL` from the Pod's IP and the public port, and a
  network policy that lets each replica reach the others' public listener.
- `terminationGracePeriodSeconds` above `drainDelay +
  CELLA_HANDOFF_TIMEOUT + gracePeriod` with a margin. The Kubernetes
  default of 30 seconds is below today's own drain of up to 63.
- `CELLA_DB_MAX_CONNS` from the formula above.
- Optionally `controller.kubernetes.io/pod-deletion-cost` written by the
  writer on its own Pod, so a scale-down removes a standby first. Without
  it the ReplicaSet may remove the writer first and then the standby it
  handed off to, which is two handoffs in one rollout. Writing it needs
  the Pod's name from the downward API and `patch` on Pods in the
  control plane's namespace.

### Configuration and observation

New variables, entered in spec 002's table when built:
`CELLA_ADVERTISE_URL`, `CELLA_FORWARD_HOLD` and `CELLA_HANDOFF_TIMEOUT`.
Design 017 gains `writer` as a value of the `cella_lease_held` gauge's
lease label, a count of forwarded requests by outcome (forwarded, held
past the bound, failed after connecting) and the duration of each
promotion, with an alert when no replica has held `writer` for a minute.
`cellad check` reports the role and, for a standby, the writer's address.

## Open risks

| Risk | What bounds it |
|---|---|
| A handoff cuts a synchronous delete and the caller sees an error | the row stays `Deleting`; the reaper finishes it (above) and a retried delete is accepted in every phase |
| The in-flight wait holds every new request for up to `CELLA_HANDOFF_TIMEOUT` when a long request is in flight | the bound; most requests take under a second, and a caller holding a long one sees it cut |
| All execution stays on one process | the capacity is today's; the reconciler row of the table above is the way past it |
| A rollout hands off twice | one extra pause of about a second; pod deletion cost removes it |
| A proxied stream ends when either of two processes goes | reconnection; local data plane serving on standbys halves it |
| A standby cannot reach the writer's address while the writer holds the lease, for a network policy that omits replica-to-replica traffic | requests are held and then refused at the bound, and the forwarded-request count shows it; the plane requirement above |
| A migration that is not expand-only reaches a rolling window | the migration test |
| The first rollout onto this design is rolling instead of Recreate | the release note and the plane's own procedure say Recreate |
| The hold is a window in which a request waits instead of failing fast | `CELLA_FORWARD_HOLD` is configurable, and zero refuses at once |

## Not in this spec

| Item | Why |
|---|---|
| Local data plane serving on standbys: exec, attach, files, logs, dial, the port proxy and the display routes of an environment whose driver runs in process, with the object read from the store by id | removes the second drop point of a proxied stream; needs the object read by id or name from the store, activity stamps from two processes, and operation records journaled from two processes, whose per-object sequence must then take concurrent appends |
| Every replica executing mutations | the rewrite in the table above |
| Workers and gateways that connect to every replica | a stream per replica per data plane; unneeded while the writer holds the hubs |
| Rolling replicas on the file store | two processes on one snapshot cannot detect each other (design 010) |

## Acceptance criteria

| Criterion | Test that proves it | State |
|---|---|---|
| A standby forwards every API route to the writer, the environment key routes and every WebSocket included, and serves the probes, `/metrics`, `/version`, the document and the key set itself | `TestAStandbyForwardsEveryRoute` | not built |
| A request that reaches a standby with no writer is held and answered once one is promoted; one held past `CELLA_FORWARD_HOLD` is refused with 503; a forward that reached the writer is not retried | `TestAStandbyHoldsUntilAWriterIsPromoted`, `TestAHoldPastTheBoundIsRefused` | not built |
| Promotion loads what the previous writer wrote and serves it | `TestPromotionReadsThePreviousWritersRows` | not built |
| On SIGTERM the writer stops executing, ends holds and streams, waits the bound, stops the loops and releases `writer` before its server shutdown ends; a standby promotes within its poll | `TestSIGTERMHandsOffBeforeTheDrain` | not built |
| A writer that lost `writer` cannot write, and a takeover waits for a write in flight | `TestADemotedWriterCannotWrite` | not built |
| A writer that loses the lease to another holder, or renews nothing for the term, exits non-zero; one failed renewal does not | `TestLosingTheWriterLeaseExits` | not built |
| Readiness is the same in both roles and does not read the lease | `TestReadinessDoesNotReadTheLease` | not built |
| The reaper finishes a `Deleting` row: it deletes an object the driver still has, and forgets a row whose object the driver no longer has | `TestTheReaperFinishesAnInterruptedDelete` | not built |
| A gateway and a worker whose long-lived stream dropped dial again after the minimum backoff | `TestAGatewayRedialsAtOnceAfterALongStream`, `TestAWorkerRedialsAtOnceAfterALongStream` | not built |
| A create that needs a gateway right after a promotion waits for one to connect | `TestACreateWaitsForAGatewayAfterPromotion` | not built |
| Every migration is expand-only but the one allowed exception | `TestMigrationsAreExpandOnly` | not built |
| Two `cellad` processes over one database, one SIGTERMed while a probe loop reads and writes through both, answer every unary request with a success | `TestARollingHandoffAnswersEveryRequest` | not built |
