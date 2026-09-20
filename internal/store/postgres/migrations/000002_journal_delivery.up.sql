-- SPDX-FileCopyrightText: 2026 Latere AI
-- SPDX-License-Identifier: Apache-2.0
--
-- The delivery half of the journal, design 009. Migration 000001 left
-- attempts, next_attempt_at and acked_at with no statement reading them; this
-- one adds the column that tells a record the sink took from one the retry
-- window ended, which design 010's retention needs to keep apart.

alter table events add column dropped_at timestamptz;

-- The delivery scan reads the unfinished rows of each object in sequence
-- order. Dropped rows are finished, so they leave the index with the
-- acknowledged ones.
drop index events_pending;
create index events_pending on events (object_id, seq) where acked_at is null and dropped_at is null;
