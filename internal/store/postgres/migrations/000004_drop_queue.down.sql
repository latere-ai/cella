-- SPDX-FileCopyrightText: 2026 Latere AI
-- SPDX-License-Identifier: Apache-2.0

create table queue (
    sandbox_id  text primary key,
    environment text        not null,
    queue       text        not null,
    priority    integer     not null default 0,
    subject     text        not null default '',
    enqueued_at timestamptz not null default now()
);

create index queue_order on queue (environment, queue, priority desc, enqueued_at);
