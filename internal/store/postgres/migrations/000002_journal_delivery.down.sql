-- SPDX-FileCopyrightText: 2026 Latere AI
-- SPDX-License-Identifier: Apache-2.0

drop index events_pending;
create index events_pending on events (next_attempt_at) where acked_at is null;
alter table events drop column dropped_at;
