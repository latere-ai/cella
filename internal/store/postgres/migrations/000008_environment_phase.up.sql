-- SPDX-FileCopyrightText: 2026 Latere AI
-- SPDX-License-Identifier: Apache-2.0
--
-- An environment's phase moves through its status write, which until now left
-- the phase column as the row was created with, so a list that filters
-- environments by phase read a stale one. The column is brought to the phase
-- the status holds; every later status write keeps it there.

update objects set phase = status->>'phase'
where kind = 'Environment' and status->>'phase' is not null and phase <> status->>'phase';
