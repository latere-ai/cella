---
title: "Worker stream credit: the per sub-stream window, its negotiation in the hello, and Watch across the seam"
status: complete
track: core
depends_on:
  - specs/021-data-plane-workers.md
  - specs/004-runtime-contract.md
  - specs/031-hosted-sandbox-consolidation.md
  - specs/.archive/051-environments-and-workers.md
affects: [runtime/remote/, internal/worker/, docs/workers.md, CHANGELOG.md]
effort: medium
created: 2026-09-23
updated: 2026-09-23
author: changkun
---

# Worker stream credit

## Overview

One worker stream carries every operation of one worker: lifecycle
calls, exec output, terminal bytes, archives and file bodies, each on
its own sub-stream of its operation. [[021-data-plane-workers]] bounds
what one side may send on a sub-stream by a `credit` control message,
so that a reader that stops reading holds its own sub-stream and
nothing else. [[051-environments-and-workers]] reserved the message and
left it open.

This slice builds the credit window, negotiates it in the hello so a
worker and a control plane of different releases keep working, and
carries `Watch` across the seam with the `relist` that makes the
control plane issue a `List`.

## Current state

| Piece | Where it is | What is missing |
|---|---|---|
| The frame and its sub-streams | `runtime/remote/protocol.go` | the `credit` message, a window, and the `event` message |
| Incoming sub-streams | `Channel.write` in `runtime/remote/link.go` writes each frame into an `io.Pipe` on the read pump | an `io.Pipe` blocks until its reader takes the bytes, so one sub-stream whose reader stopped blocks the read pump, and with it every other operation, every result and the heartbeat. Over the WebSocket the far side's write then passes its 10 second deadline and the whole stream drops |
| Outgoing sub-streams | `substream.Write` queues frames onto the link's 256 frame out queue | nothing bounds what one sub-stream queues but the queue itself, 256 MiB |
| The answer before the bytes | `open` in `runtime/remote/execute.go` answers first and copies in a goroutine after the operation returned, held by `TestReadAnswersBeforeItStreams` | the copy outlives the operation's channel, so nothing the control plane sends about that operation reaches the copy |
| A cancel | `Channel.cancel` closes a channel the executor's context watches | a driver blocked reading a body or stdin from the sub-stream is not released until the connection ends |
| A resize | `Link.route` sends it into a channel of 8 on the read pump | a ninth resize before the terminal loop takes one blocks the read pump |
| The control plane's heartbeat | `Hub.beat` sends one every 15 seconds | the worker's `Server.onMessage` logs it as a message that belongs the other way, once per heartbeat |
| `Watch` | [[004-runtime-contract]] declares it on `Driver`, with the `Event` of `added`, `modified`, `deleted`, `lost` and `relist` | `runtime/driver.go` declares neither `Watch` nor `Event`, and no driver implements one. The worker reports its whole list every 15 seconds and after each change instead |

## Design

### The window

Every sub-stream but control is credited on its own. A side may have at
most the window of bytes of one sub-stream sent and not yet credited
back, and the receiver holds at most that much of it. Control messages
are not credited: they are small, they carry the credits themselves,
and a result or a cancel must never wait behind bytes.

The window is per sub-stream rather than per operation, which is a
change to 021's row. An exec's stdout and stderr share an operation,
and a caller that reads one to its end before it reads the other is
common: under one window per operation, the output it has not started
reading would fill the window and stop the output it is reading. Per
sub-stream, the two are as independent as the two pipes of a process
on the same host.

The window is 8 MiB, eight frames, which is 021's figure. A sender
keeps up to eight frames in flight, so one sub-stream moves at most
the window per round trip: 80 MiB/s at 100 ms, which an archive
between regions needs, and more at shorter distances. The bound on
memory is the window per sub-stream a reader has stopped reading: an
exec whose caller reads neither output holds 16 MiB on the control
plane and nothing more. The window is what each side announces in its
hello rather than a constant both assume, so a later release changes it
without a protocol change.

The receiver credits bytes back as its reader takes them, in grants of
at least the smaller of half the window and one frame, so a reader
taking a few bytes at a time sends one message per megabyte rather than
one per read. A sender with no credit left waits, and never blocks
anything but its own writer.

### The wire

| Message | Direction | Fields | Meaning |
|---|---|---|---|
| `hello` | up, first | `worker`, `window`, `watch` | the registration, the window this worker grants per sub-stream, and whether its driver watches |
| `hello` | down, in answer | `window` | the window the control plane grants, sent only to a worker that announced one; it is what turns credit on for the connection |
| `credit` | both | the frame's operation, `stream`, `bytes` | the receiver took `bytes` more of that sub-stream and the sender may send as many more |
| `event` | up | the frame's operation, `event {type, state}` | one change the worker's driver observed, on the long-lived `Watch` operation |

The hello down is queued before the worker's registration is bound to
the connection, and no operation is sent to a worker before it is
bound, so the worker has credit on before the first frame of any
operation reaches it.

### A side that breaks the window

A peer is closed with `ErrFrame`, like every frame this protocol cannot
read, when on a credited connection it sends more of one sub-stream than
the window allows past what was credited, grants zero or fewer bytes,
grants credit for control, grants more than the window it announced
could ever leave outstanding, or sends a second hello. Each of these is
a defect on the far side, and a receiver that tolerated one could no
longer say what it holds.

### Releasing a waiting writer

A writer waiting for credit returns when credit arrives, and also when
the operation can no longer use its bytes, so that nothing waits on a
grant that will never come:

| What happened | What the writer returns |
|---|---|
| the caller closed the operation, or the far side cancelled it | `io.ErrClosedPipe` |
| the far side answered the operation, which ends it there | `io.ErrClosedPipe` |
| the connection ended | `ErrLinkClosed` with the reason |

A write whose body the worker refused part way, which is `ErrTooLarge`,
is the second row: the worker answers and stops reading, and the
control plane's copy of the body ends at once and reads the refusal.

A cancel from the control plane also ends every sub-stream coming into
that operation on the worker, so a driver blocked reading a body or a
terminal's input reads the cancellation rather than waiting until the
connection ends.

### What moves with the window

- A file read keeps its operation until the bytes are sent. It still
  answers before the first byte, so a caller learns the size and the
  mode first; the copy now runs inside the operation rather than after
  it, because credit for those bytes arrives on the operation's channel.
- A resize that finds the terminal's queue full replaces the oldest
  window it holds, since only the newest window is the terminal's, and
  never blocks the read pump.
- The worker takes the control plane's heartbeat as a heartbeat.

### Two releases on one stream

The subprotocol stays `cella.worker.v1`. Its messages only grow, and an
unknown field is ignored by the JSON decoder on both sides, so the
window is agreed in the hello rather than by a new subprotocol that
would refuse every worker the moment its control plane was upgraded:
the control plane and its workers are often operated by different
parties and upgraded on their own schedules.

| Worker | Control plane | What happens |
|---|---|---|
| this release | this release | credit on, `Watch` carried when the worker's driver watches |
| this release | an earlier release | the control plane ignores the new hello fields and sends no hello down, so the worker never turns credit on and never sends a `credit` or an `event`, which that control plane would read as a frame that belongs the other way and close on. The stream behaves as it did before this slice |
| an earlier release | this release | the worker announces no window, so the control plane never turns credit on for it and never opens a `Watch`. The stream behaves as it did before this slice, except that the receiver now holds up to the window of a sub-stream before its read pump waits, where it held none |

Without credit the receiver keeps the previous back pressure: a
sub-stream whose buffer is at the window holds the read pump until its
reader takes bytes, which is the pipe of before with room for one
window. Nothing else about that stream changes.

### Watch across the seam

[[004-runtime-contract]] puts `Watch(ctx) (<-chan Event, error)` on
`Driver`, but the contract in `runtime/driver.go` does not declare it,
and no driver implements it. This slice declares the seam's half in
`runtime/remote`: `Watcher`, the optional interface a worker's driver
implements, and `Event` with 004's five types. The remote driver
implements `Watcher` itself, so a consumer on the control plane reads
one environment's events as it would read an in-process driver's. When
the contract declares `Watch`, `Event` becomes an alias of the
contract's type and nothing on the wire changes.

- A worker whose driver implements `Watcher` says so in its hello, and
  the control plane opens one `Watch` operation on that connection. It
  is not a row of the operations table: it is the connection's own, it
  is never redelivered, and it ends with the connection.
- The worker relays each event as an `event` message. When its driver's
  channel closes, it sends a `relist`, which is 004's rule that a
  `relist` follows a closed channel, and watches again.
- The control plane applies `added`, `modified` and `lost` to the state
  it holds for the environment, and `deleted` removes the sandbox, so
  `Inspect` and `List` answer from what the events said.
- A `relist` makes the control plane issue a `List` on the connection
  that sent it and replace what it holds with the answer. The `relist`
  is delivered to the consumers only after that, so a consumer that
  lists on `relist`, which 005 says it does, reads the rebuilt state.
  Relists that arrive while a `List` is in flight are coalesced into one
  more.
- A consumer that falls behind is not waited for. Past 256 undelivered
  events its queue is replaced by one `relist`, so the read pump never
  holds and the consumer misses nothing it does not re-read.
- A `Watch` operation that ends, because the connection ended or the
  worker's driver failed, delivers a `relist` to every consumer, since
  events may have been missed until the next one opens. One that ended
  while the connection stayed up is the worker's driver failing to watch,
  and the control plane opens it again after one second, doubling to
  thirty while it keeps failing at once.

## Not in this slice

The dial and screen streams of 021's `TestRemoteStreams` row: the
remote driver implements neither `Dialer` nor `DisplayDriver`, and the
`Dialer` of the drivers is landing in a parallel slice. Both ride
`bytes` sub-streams, which this slice credits like any other.

The controller consuming `Watch` ([[005-lifecycle-controller]]), and
`Watch` on the in-process drivers and in the conformance suite
([[004-runtime-contract]]); neither has a driver to consume yet.

A metric of the bytes each stream holds. The high water mark this
slice measures is read by its tests.

## Acceptance criteria

| Criterion | Test that proves it | State |
|---|---|---|
| The `credit` message, the hello's `window` and `watch`, and the `event` message encode and decode both ways | `TestCreditMessages` | built |
| A peer that breaks the window, grants what it cannot, credits control, or says hello twice is closed with `ErrFrame` | `TestAMisbehavingPeerIsClosed` | built |
| One sub-stream whose reader stalls holds only itself: another operation on the same connection completes while it is stalled, and the stalled one completes once read | `TestAStalledReaderHoldsOnlyItsOwnStream` | built |
| A caller that stalls past a frame's write deadline keeps the worker's WebSocket, and another exec runs on it meanwhile | `TestAStalledCallerKeepsTheStream` | built |
| An exec of 64 MiB output, an attach with resize, a tar both ways, and a file read past the window stream through one connection under credit, and no sub-stream on either side ever holds more than the window, measured | `TestRemoteStreams` | built |
| A writer waiting for credit is released by the caller closing, by the far side answering, and by the connection ending; a body the worker refused part way ends the copy at once | `TestAWaitingWriterIsReleased` | built |
| A cancel releases a driver reading a body; a resize past the queue replaces the oldest; the worker takes the control plane's heartbeat without a warning | `TestACancelReleasesTheBodyReader`, `TestResizesNeverHoldThePump`, `TestTheWorkerTakesTheHeartbeat` | built |
| A worker and a control plane of different releases keep the stream: no credit, no `event`, and the previous back pressure | `TestAPeerWithoutCreditKeepsThePreviousStream` | built |
| An operation issued as its worker's stream ends is refused rather than reaching the released connection | `TestAnOperationRacesItsWorkerLeaving` | built |
| `Watch` events cross the seam in order and update what `Inspect` and `List` answer, and a `relist` makes the control plane issue a `List` whose answer is in place before the `relist` is delivered | `TestRemoteWatch` | built |
| A consumer that falls behind receives a `relist`; a closed driver channel crosses as a `relist`, and a Watch the driver fails is opened again; a worker whose driver does not watch is never asked | `TestAWatchThatFallsBehindRelists`, `TestAClosedWatchRelists`, `TestAWorkerThatDoesNotWatchIsNotAsked` | built |
| No file this slice adds names a Latere host, image, pool or namespace | `TestNoLatereCoordinates` | built |

## Outcome

The credit window of [[021-data-plane-workers]] is built, agreed in the
hello, and `Watch` crosses the seam. What landed:

| Piece | Where |
|---|---|
| `DefaultWindow`, the `credit` and `event` messages, the hello's `window` and `watch`, `OpWatch` | `runtime/remote/protocol.go` |
| The per sub-stream receive buffer, the send credit and its wait, the agreement, the resize queue, the cancel that ends incoming sub-streams, the first reason as what `Run` returns | `runtime/remote/link.go` |
| `Event`, `Watcher` and the consumer's bounded queue | `runtime/remote/watch.go` |
| The file read kept inside its operation, and the worker's Watch loop | `runtime/remote/execute.go` |
| The hello that announces the window and the watch, the answer that turns credit on, the heartbeat | `runtime/remote/serve.go` |
| The hello's answer, the connection's Watch reopened with backoff, events into the observed state, the relist's `List`, the consumers, `Release` under the lock, `Open` reading the link under the lock | `runtime/remote/hub.go` |
| `Transport.Watch` and `Driver.Watch` | `runtime/remote/remote.go` |
| The frame write deadline the socket tests shorten | `internal/worker/socket.go` |
| The operator's section on long streams and upgrades | `docs/workers.md` |

Coverage on `go test -cover`: `runtime/remote` 92.3%, `internal/worker`
91.3%. `go test -race` is clean over both. The high water mark
`TestRemoteStreams` measured was 8,388,608 bytes on the control plane,
exactly the window, while the 64 MiB exec's reader stalled, and
8,355,840 on the worker while the archive and the file body came down.

Five of the bug fix tests were run against the tree before this slice
and fail there: `TestAStalledReaderHoldsOnlyItsOwnStream` hangs,
`TestAStalledCallerKeepsTheStream` drops the stream with a broken pipe,
`TestResizesNeverHoldThePump` never reads the result behind the
resizes, `TestTheWorkerTakesTheHeartbeat` finds the warning, and
`TestACancelReleasesTheBodyReader` finds the staged write still held.
`TestAnOperationRacesItsWorkerLeaving` fails on the tree before its fix
with a data race and a nil pointer dereference in `transport.Open`.

### Divergences

| From | To | Why |
|---|---|---|
| 021's `credit {id, bytes}` per operation | `credit {id, stream, bytes}` per sub-stream, 021 rewritten | an exec's two outputs share an operation, and a caller that reads one to its end before the other would stop the one it reads under one window per operation |
| A window both sides assume | each side announces its own in the hello, 8 MiB by default | a later release changes it without a protocol change |
| A new subprotocol for a changed stream | `cella.worker.v1` kept, credit agreed in the hello | a control plane and its workers are often operated by different parties and upgraded apart; a new subprotocol would refuse every worker at the control plane's upgrade, and the API's upgrader offers one subprotocol |
| 004's `Watch` on `Driver` | `Watcher` and `Event` declared in `runtime/remote` | the contract declares no `Watch` and no driver implements one; the types move to `runtime` when one does |
| The relist's `List` through the operations table | issued on the connection that sent the relist, as the Watch is | neither is a caller's operation and neither is redelivered |

Two defects found on the way were fixed with their tests:
`transport.Open` read a worker's link outside the hub's lock, so an
operation issued as the worker's stream ended could reach a released
link and dereference nil, which crashes the control plane from a loop
that recovers nothing; and `Hub.Release` walked the environment's
workers after dropping the lock.

### What this leaves open

| Open | Why |
|---|---|
| The dial and the screen of 021's `TestRemoteStreams` row | the remote driver implements neither `Dialer` nor `DisplayDriver`; both ride `bytes` sub-streams, which are credited like any other |
| `Watch` on the in-process drivers, in the conformance suite, and consumed by the controller | no driver watches; [[004-runtime-contract]] and [[005-lifecycle-controller]] |
| A metric of what each stream holds | the high water mark is read by the tests alone |
