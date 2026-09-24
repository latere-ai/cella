# Following events

Every change to an object and every operation on a sandbox is one record:
what happened, to which object, who asked, and when. `GET /v1/events`
reads them two ways. A page is one object's history, newest first. A
following feed stays open and hands you each record as it happens, so a
client that waits for a sandbox to start, or a console that shows your
sandboxes changing, does not poll.

## One object's history

```sh
curl -s -H "Authorization: Bearer $TOKEN" \
  "$CELLA/v1/events?object=$SANDBOX&limit=50"
```

The answer is `{"items": [...], "next": "..."}`, newest first. Each record
carries `seq`, which counts that object's records from 1 without a gap.
Pass `next` back as `cursor` for the next older page; an empty `next` is
the end.

## Following one object

```sh
curl -sN -H "Authorization: Bearer $TOKEN" \
  "$CELLA/v1/events?object=$SANDBOX&follow=1&cursor=$SEQ"
```

`cursor` is the newest `seq` you already hold. The feed sends every record
after it, oldest first, then each new record as it is committed, and stays
open. Nothing is sent twice and nothing is skipped between what you read
before and what the feed sends. Without `cursor` the feed starts from now.
`cursor=0` replays everything the server still keeps.

A page's `next` points backwards, so resume from `items[0].seq`, the
newest record of the page you read, and not from `next`.

The feed ends after the object's delete record (`sandbox.deleted`,
`secret.deleted` or `environment.deleted`).

### Waiting for a sandbox to start

Read one page, then follow from its newest record, and stop at the record
you are waiting for:

```sh
SEQ=$(curl -s -H "Authorization: Bearer $TOKEN" \
  "$CELLA/v1/events?object=$SANDBOX&limit=1" | jq '.items[0].seq')
curl -sN -H "Authorization: Bearer $TOKEN" \
  "$CELLA/v1/events?object=$SANDBOX&follow=1&cursor=$SEQ" |
  jq -c --unbuffered 'select(.type == "sandbox.started" or .type == "sandbox.failed")' |
  head -n 1
```

Following from the page's newest record rather than from now closes the
window between the read and the follow: a start that happened in between
is in the feed.

## Following everything you can see

```sh
curl -sN -H "Authorization: Bearer $TOKEN" "$CELLA/v1/events?follow=1"
```

Without `object`, the feed carries every record you are allowed to read,
of every object, from the moment it opens. It is filtered one record at a
time the way listing your sandboxes is: under the built-in policy you see
your own sandboxes and secrets. A sandbox's own token sees that sandbox's
records; it follows a child by naming the child with `object`.

This feed has no `cursor`, because `seq` counts within one object and
there is no order across objects to resume from. To rebuild a view after a
reconnect, list what you have again and then follow.

On an installation that runs more than one replica of the server against
one database, this feed carries the records of the replica that serves it.
Following one object is complete on every installation.

## What a feed looks like

`Content-Type: application/x-ndjson`, one line at a time:

| Line | Meaning |
|---|---|
| a JSON object with `id`, `seq` and `type` | one record, exactly as a page carries it |
| an empty line | nothing happened for 15 seconds; the feed is still open |
| a JSON object with an `error` member | the feed ended on a failure; it is the last line |

## When a feed ends

| What you see | Why | What to do |
|---|---|---|
| the connection closes with no error line | the server is shutting down or restarting | reconnect with the newest `seq` you hold |
| `unauthenticated` as the last line | your token expired while the feed was open | get a fresh token and reconnect with the newest `seq` you hold |
| 410 `cursor_expired`, or it as the last line | the server no longer keeps the records after your position | read a page again and continue from its newest record; the records in between are gone |
| 429 `rate_limited` | the server holds as many feeds open as it serves | wait and retry |
| 400 `invalid_field` on `cursor` | the cursor is not a number, is above the object's newest `seq`, or was sent without `object` | send the newest `seq` you hold, with `object` |

## How long records are kept

Records older than `CELLA_JOURNAL_RETENTION` (default 30 days) are
removed once they are delivered. Without a database, each object also
keeps at most its newest `CELLA_JOURNAL_CAP` records (default 1000), and
the records do not survive a restart: every object's `seq` starts again
at 1, and a cursor above an object's newest `seq` is refused. Each
object's newest record is always kept, so while the server runs a `seq`
you hold names the same record.

## Behind a proxy

A proxy or load balancer in front of the server must pass the feed through
as it arrives and must not close it while it is idle:

- turn response buffering off for `/v1/events`. The server sends
  `X-Accel-Buffering: no`, which nginx honors; other proxies need their
  own setting.
- allow an idle read of more than 15 seconds, the interval of the empty
  lines. Most defaults, 60 seconds, are enough.
