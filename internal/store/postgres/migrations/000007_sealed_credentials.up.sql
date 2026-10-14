-- SPDX-FileCopyrightText: 2026 Latere AI
-- SPDX-License-Identifier: Apache-2.0
--
-- The values of a sandbox's environment and its gateway credential, sealed
-- under the envelope secret_values uses: a data key of the row's own wrapped
-- by CELLA_SECRET_KEY, and the two values under it as one JSON document. A
-- sandbox row that holds them names it in data, whose spec.env is the string
-- "sealed" in place of the values. Both columns are nullable: a row of
-- another kind, a sandbox with neither value, a deleted sandbox, a store
-- opened without a key, and every binary before these columns write rows
-- without them.

alter table objects add column sealed_key bytea;
alter table objects add column sealed bytea;
