-- SPDX-FileCopyrightText: 2026 Latere AI
-- SPDX-License-Identifier: Apache-2.0
--
-- A row that holds sealed values keeps its environment and its gateway
-- credential in these columns alone, and no statement can open them, so the
-- columns are dropped only from a table where no row holds one.

do $$
begin
    if exists (select 1 from objects where sealed is not null) then
        raise exception 'objects holds sealed sandbox credentials, which dropping the columns would lose';
    end if;
end
$$;

alter table objects drop column sealed;
alter table objects drop column sealed_key;
