-- SPDX-FileCopyrightText: 2026 Latere AI
-- SPDX-License-Identifier: Apache-2.0
--
-- The address a lease's holder is reached at, which the writer lease of
-- spec 076 carries so a standby knows where to forward. It is nullable: a
-- holder that advertises none, and every binary before this column, write
-- rows without it.

alter table leases add column address text;
