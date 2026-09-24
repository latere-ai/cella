-- SPDX-FileCopyrightText: 2026 Latere AI
-- SPDX-License-Identifier: Apache-2.0
--
-- The registry of environment keys of design 021. One row per key the control
-- plane minted: who minted it, when, when it expires, and when it was revoked.
-- The token is never stored. A row is kept until the key expires, when the
-- sweep that forgets expired revocations forgets it too.

create table environment_keys (
    jti         text primary key,
    environment text        not null,
    subject     text        not null default '',
    minted_at   timestamptz not null,
    expires_at  timestamptz not null,
    revoked_at  timestamptz
);

create index environment_keys_by_environment on environment_keys (environment, jti);
create index environment_keys_expires_at on environment_keys (expires_at);
