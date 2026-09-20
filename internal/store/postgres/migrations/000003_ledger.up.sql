-- SPDX-FileCopyrightText: 2026 Latere AI
-- SPDX-License-Identifier: Apache-2.0
--
-- The spawn ledger of design 022. One row per sandbox that has created a
-- child: how many it has created in total. The budget is desired state and
-- not a column, so a root narrowed after its children exist takes effect at
-- the next debit with no second write, and the debit carries the budget it is
-- conditional on.

create table ledger (
    sandbox_id text primary key,
    used       integer     not null default 0 check (used >= 0),
    updated_at timestamptz not null default now()
);
