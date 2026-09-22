-- SPDX-FileCopyrightText: 2026 Latere AI
-- SPDX-License-Identifier: Apache-2.0
--
-- The scheduler's queue is the desired sandboxes in the Queued phase, ordered
-- by the controller on each tick (spec 057). A table beside them would be a
-- second record of one fact, written in step with the desired row on every
-- create, placement, deadline and delete, and reconciled after a restart.

drop table queue;
