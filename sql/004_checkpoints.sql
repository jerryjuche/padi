-- M3b: container-side checkpoint refs recorded by the gateway, plus RLS.
-- Forward-only and idempotent: safe to apply more than once.
-- Apply with: psql "$DATABASE_URL" -f sql/004_checkpoints.sql

-- Telegram approval routing columns and the agreed task contract snapshot.
-- Keeping them next to the checkpoint migration avoids a second forward-only
-- file before M10 shifts numbering.
do $$
begin
  if not exists (
    select 1 from information_schema.columns
    where table_schema='agent' and table_name='approvals' and column_name='chat_id'
  ) then
    alter table agent.approvals add column chat_id bigint;
  end if;
  if not exists (
    select 1 from information_schema.columns
    where table_schema='agent' and table_name='approvals' and column_name='session_id'
  ) then
    alter table agent.approvals add column session_id uuid references agent.sessions(id);
  end if;
  if not exists (
    select 1 from information_schema.columns
    where table_schema='agent' and table_name='tasks' and column_name='contract'
  ) then
    alter table agent.tasks add column contract jsonb;
  end if;
  if not exists (
    select 1 from information_schema.columns
    where table_schema='agent' and table_name='tasks' and column_name='budget_used'
  ) then
    alter table agent.tasks add column budget_used jsonb not null default '{}'::jsonb;
  end if;
end
$$;

create table if not exists agent.session_checkpoints (
  id uuid primary key default gen_random_uuid(),
  task_id uuid not null references agent.tasks(id) on delete cascade,
  session_id uuid not null references agent.sessions(id) on delete cascade,
  n integer not null check (n >= 0),
  ref text not null,
  tree text not null,
  created_at timestamptz not null default now(),
  restored_at timestamptz,
  unique (task_id, n)
);

create index if not exists session_checkpoints_task_n_idx
  on agent.session_checkpoints (task_id, n desc);
create index if not exists session_checkpoints_session_idx
  on agent.session_checkpoints (session_id);

alter table agent.session_checkpoints enable row level security;

-- anon and authenticated get zero policies; only the gateway role may read or
-- write checkpoint records.
do $$
begin
  if exists (select 1 from pg_roles where rolname = 'anon') then
    execute 'revoke all on agent.session_checkpoints from anon, authenticated';
  end if;
end
$$;

grant usage on schema agent to agent_gateway;
grant select, insert, update, delete on agent.session_checkpoints to agent_gateway;
