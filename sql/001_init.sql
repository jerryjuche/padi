-- M0: foundation schema. Forward-only and idempotent.
-- Apply to Supabase with: psql "$DATABASE_URL" -f sql/001_init.sql
-- The dedicated roles are created WITHOUT passwords; set them out of band with
--   alter role agent_gateway password :'gateway_password';
-- using a psql variable supplied from /srv/agent/secrets. Never commit a password.

create schema if not exists agent;

-- Supabase has anon/authenticated roles; plain Postgres may not. Guard both.
do $$
begin
  if exists (select 1 from pg_roles where rolname = 'anon') then
    execute 'revoke all on schema agent from anon, authenticated';
  end if;
end
$$;

-- Roles are shared across migrations, so create them only when absent.
do $$
begin
  if not exists (select 1 from pg_roles where rolname = 'agent_gateway') then
    create role agent_gateway login;
  end if;
  if not exists (select 1 from pg_roles where rolname = 'agent_router') then
    create role agent_router login;
  end if;
end
$$;

-- RLS with zero policies denies every non-owner role. The two dedicated
-- service roles are the only intended data path, so they bypass RLS; anon and
-- authenticated stay subject to it with zero policies and are revoked below.
-- This requires a superuser migration role (Supabase's postgres role).
alter role agent_gateway bypassrls;
alter role agent_router bypassrls;

create table if not exists agent.sessions (
  id uuid primary key default gen_random_uuid(),
  created_at timestamptz not null default now(),
  channel text not null check (channel in ('telegram','web')),
  title text,
  mode text not null default 'plan' check (mode in ('plan','build','research')),
  active_model text,
  last_seq bigint not null default 0
);

create table if not exists agent.repos (
  full_name text primary key,
  default_branch text not null,
  added_at timestamptz not null default now()
);

create table if not exists agent.tasks (
  id uuid primary key default gen_random_uuid(),
  session_id uuid not null references agent.sessions(id),
  repo text references agent.repos(full_name),
  branch text,
  state text not null,
  fail_reason text,
  prompt text not null,
  pr_url text,
  preview_url text,
  fix_loops int not null default 0,
  idempotency_key text unique,
  created_at timestamptz not null default now(),
  finished_at timestamptz
);

create table if not exists agent.events (
  session_id uuid not null references agent.sessions(id),
  seq bigint not null,
  task_id uuid,
  type text not null,
  data jsonb not null default '{}',
  ts timestamptz not null default now(),
  primary key (session_id, seq)
);

create table if not exists agent.approvals (
  id uuid primary key default gen_random_uuid(),
  task_id uuid not null references agent.tasks(id),
  action text not null,
  detail jsonb not null,
  status text not null default 'pending' check (status in ('pending','approved','rejected','expired')),
  decided_via text,
  expires_at timestamptz not null,
  created_at timestamptz not null default now(),
  decided_at timestamptz
);

create table if not exists agent.system_flags (
  key text primary key,
  value text not null,
  updated_at timestamptz not null default now()
);

create table if not exists agent.llm_usage (
  id bigserial primary key,
  ts timestamptz not null default now(),
  session_id uuid,
  alias text not null,
  provider text not null,
  model text not null,
  status int not null,
  fallback_depth int not null default 0,
  first_byte_ms int,
  total_ms int,
  tokens_in int,
  tokens_out int,
  user_key boolean not null default false
);

create table if not exists agent.audit_log (
  id bigserial primary key,
  ts timestamptz not null default now(),
  task_id uuid,
  actor text not null,
  action text not null,
  detail jsonb not null default '{}'
);

create table if not exists agent.provider_keys (
  provider text primary key,
  ciphertext bytea not null,
  nonce bytea not null,
  last4 text not null,
  created_at timestamptz not null default now()
);

create table if not exists agent.risk_cache (
  repo text not null,
  base_sha text not null,
  path text not null,
  signals jsonb not null,
  computed_at timestamptz not null default now(),
  primary key (repo, base_sha, path)
);

create index if not exists tasks_session_idx on agent.tasks (session_id);
create index if not exists tasks_state_idx on agent.tasks (state);
create index if not exists events_task_idx on agent.events (task_id);
create index if not exists approvals_task_status_idx on agent.approvals (task_id, status);
create index if not exists llm_usage_ts_idx on agent.llm_usage (ts);
create index if not exists audit_log_ts_idx on agent.audit_log (ts);

-- RLS on every table, with deliberately zero policies.
alter table agent.sessions enable row level security;
alter table agent.repos enable row level security;
alter table agent.tasks enable row level security;
alter table agent.events enable row level security;
alter table agent.approvals enable row level security;
alter table agent.system_flags enable row level security;
alter table agent.llm_usage enable row level security;
alter table agent.audit_log enable row level security;
alter table agent.provider_keys enable row level security;
alter table agent.risk_cache enable row level security;

do $$
begin
  if exists (select 1 from pg_roles where rolname = 'anon') then
    execute 'revoke all on all tables in schema agent from anon, authenticated';
  end if;
end
$$;

grant usage on schema agent to agent_gateway, agent_router;
grant select, insert, update, delete on all tables in schema agent to agent_gateway;
grant usage on all sequences in schema agent to agent_gateway;
grant insert on agent.llm_usage to agent_router;
grant usage on sequence agent.llm_usage_id_seq to agent_router;

-- Operator step (not expressible in SQL): keep schema `agent` OUT of
-- Supabase -> API Settings -> Exposed schemas. Only `public` should be exposed.
