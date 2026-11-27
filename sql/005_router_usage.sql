-- M4: router usage accounting. Forward-only and idempotent.
-- Apply with: psql "$DATABASE_URL" -f sql/005_router_usage.sql
--
-- The router uses a separate insert-only role. No prompt, header value, key
-- or base URL is stored here: only counters and opaque task/session IDs.

do $$
begin
  if not exists (
    select 1 from information_schema.columns
    where table_schema='agent' and table_name='llm_usage' and column_name='cost_usd_micros'
  ) then
    alter table agent.llm_usage add column cost_usd_micros bigint not null default 0;
  end if;
  if not exists (
    select 1 from information_schema.columns
    where table_schema='agent' and table_name='llm_usage' and column_name='quota_tokens'
  ) then
    alter table agent.llm_usage add column quota_tokens integer not null default 0;
  end if;
end
$$;

create index if not exists llm_usage_alias_ts_idx on agent.llm_usage (alias, ts desc);
create index if not exists llm_usage_provider_ts_idx on agent.llm_usage (provider, ts desc);
create index if not exists llm_usage_session_ts_idx on agent.llm_usage (session_id, ts desc);

alter table agent.llm_usage enable row level security;

-- Zero policies for anon and authenticated. RLS with no policy denies all
-- access, including reads through the Supabase REST API.
do $$
begin
  if exists (select 1 from pg_roles where rolname = 'anon') then
    execute 'revoke all on agent.llm_usage from anon, authenticated';
  end if;
end
$$;

grant usage on schema agent to agent_router;
grant insert on agent.llm_usage to agent_router;
grant usage on sequence agent.llm_usage_id_seq to agent_router;
