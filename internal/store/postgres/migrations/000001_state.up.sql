-- SPDX-FileCopyrightText: 2026 Latere AI
-- SPDX-License-Identifier: Apache-2.0
--
-- The state of design 010. Every table is the control plane's own; nothing
-- here belongs to a product built on it.

-- objects holds desired state: one row per object of any kind, the resolved
-- manifest in data, the controller's status beside it, and a version every
-- write advances so a second replica cannot overwrite a row it did not read.
create table objects (
    id           text primary key,
    kind         text        not null,
    owner        text        not null,
    name         text        not null,
    environment  text        not null default '',
    phase        text        not null default '',
    labels       jsonb       not null default '{}'::jsonb,
    data         jsonb,
    status       jsonb,
    last_applied jsonb,
    version      bigint      not null default 1,
    created_at   timestamptz not null default now(),
    updated_at   timestamptz not null default now(),
    deleted_at   timestamptz
);

-- A name is unique among live rows of one kind and owner, which is what the
-- API's name_taken answers, and free again once the row is deleted.
create unique index objects_live_name on objects (kind, owner, name) where deleted_at is null;
create index objects_owner_phase on objects (kind, owner, phase);
create index objects_environment on objects (kind, environment);
create index objects_labels on objects using gin (labels);

-- observed is the index of what a driver reported, one row per sandbox per
-- environment. Every row is rebuildable from a driver's list, so losing this
-- table costs one rebuild and no intent.
create table observed (
    id          text primary key,
    environment text        not null,
    owner       text        not null default '',
    phase       text        not null default '',
    labels      jsonb       not null default '{}'::jsonb,
    state       jsonb       not null,
    updated_at  timestamptz not null default now()
);

create index observed_environment on observed (environment);

-- events is the journal: one row per mutation, sequenced within its object,
-- so a reader of one object sees its history in the order it happened. The
-- delivery columns are design 009's and no statement reads them yet.
create table events (
    id              text primary key,
    object_id       text        not null,
    seq             bigint      not null,
    type            text        not null,
    at              timestamptz not null default now(),
    payload         jsonb,
    attempts        integer     not null default 0,
    next_attempt_at timestamptz,
    acked_at        timestamptz,
    unique (object_id, seq)
);

create index events_pending on events (next_attempt_at) where acked_at is null;

-- secret_values holds ciphertext only. The wrapped data key and the value are
-- separate columns so rotating the store's key rewrites the first and leaves
-- every byte of the second alone.
create table secret_values (
    secret_id   text primary key,
    version     integer     not null default 1,
    wrapped_key bytea       not null,
    ciphertext  bytea       not null,
    created_at  timestamptz not null default now(),
    updated_at  timestamptz not null default now()
);

-- leases are the single-writer seam: one holder per name until the term
-- lapses, decided by this server's clock rather than any replica's.
create table leases (
    name       text primary key,
    holder     text        not null,
    expires_at timestamptz not null
);

-- revocations is the seam slice 045 fills: the jti of a token revoked before
-- it expired, forgotten once its exp passed.
create table revocations (
    jti text primary key,
    exp timestamptz not null
);

create index revocations_exp on revocations (exp);

-- queue is the seam slice 038 fills: one row per sandbox waiting for capacity
-- on a queued environment. Design 020 owns the order it is read in.
create table queue (
    sandbox_id  text primary key,
    environment text        not null,
    queue       text        not null,
    priority    integer     not null default 0,
    subject     text        not null default '',
    enqueued_at timestamptz not null default now()
);

create index queue_order on queue (environment, queue, priority desc, enqueued_at);

-- operations and workers are the seam design 021 fills: the work a data plane
-- worker claims, and the registrations an environment's phase is computed
-- from.
create table operations (
    id          text primary key,
    environment text        not null,
    sandbox_id  text        not null default '',
    type        text        not null,
    payload     jsonb,
    state       text        not null default 'queued',
    claimed_by  text,
    claimed_at  timestamptz,
    attempts    integer     not null default 0,
    result      jsonb,
    created_at  timestamptz not null default now()
);

create index operations_claim on operations (environment, state, created_at);

create table workers (
    environment    text        not null,
    worker         text        not null,
    replica        text        not null default '',
    last_heartbeat timestamptz not null default now(),
    primary key (environment, worker)
);

create index workers_heartbeat on workers (environment, last_heartbeat);
