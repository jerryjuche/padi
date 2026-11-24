-- M3a: task contracts, budgets, repo settings and owner-approved egress.
-- Forward-only and idempotent: safe to apply more than once.
-- Apply to Supabase with: psql "$DATABASE_URL" -f sql/003_harness.sql

do $$
begin
  if not exists (
    select 1 from information_schema.columns
    where table_schema='agent' and table_name='tasks' and column_name='kind'
  ) then
    alter table agent.tasks add column kind text not null default 'code';
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
    alter table agent.tasks add column budget_used jsonb not null default '{}';
  end if;
  if not exists (
    select 1 from information_schema.columns
    where table_schema='agent' and table_name='repos' and column_name='lsp_enabled'
  ) then
    alter table agent.repos add column lsp_enabled boolean not null default false;
  end if;
  if not exists (
    select 1 from information_schema.columns
    where table_schema='agent' and table_name='repos' and column_name='clone_mode'
  ) then
    alter table agent.repos add column clone_mode text not null default 'full';
  end if;
  if not exists (
    select 1 from information_schema.columns
    where table_schema='agent' and table_name='repos' and column_name='rules_md'
  ) then
    alter table agent.repos add column rules_md text;
  end if;
  if not exists (
    select 1 from information_schema.columns
    where table_schema='agent' and table_name='repos' and column_name='contract_defaults'
  ) then
    alter table agent.repos add column contract_defaults jsonb;
  end if;
  if not exists (
    select 1 from information_schema.columns
    where table_schema='agent' and table_name='repos' and column_name='onboarded_at'
  ) then
    alter table agent.repos add column onboarded_at timestamptz;
  end if;
  if not exists (
    select 1 from information_schema.columns
    where table_schema='agent' and table_name='llm_usage' and column_name='task_id'
  ) then
    alter table agent.llm_usage add column task_id uuid;
  end if;
end
$$;

-- Constraints are added separately so a partially applied earlier run converges.
do $$
begin
  alter table agent.tasks add constraint tasks_kind_check
    check (kind in ('code','onboard','browse'));
exception when duplicate_object then null;
end
$$;

do $$
begin
  alter table agent.repos add constraint repos_clone_mode_check
    check (clone_mode in ('full','partial'));
exception when duplicate_object then null;
end
$$;

create table if not exists agent.egress_allow (
  domain text not null,
  task_id uuid not null references agent.tasks(id),
  approved_at timestamptz not null default now(),
  expires_at timestamptz not null,
  primary key (domain, task_id)
);

alter table agent.egress_allow enable row level security;

create index if not exists egress_allow_expires_at_idx on agent.egress_allow (expires_at);
create index if not exists llm_usage_task_id_idx on agent.llm_usage (task_id);

-- anon and authenticated have zero policies on egress_allow; only the gateway
-- role may insert or read owner approvals.
do $$
begin
  if exists (select 1 from pg_roles where rolname = 'anon') then
    execute 'revoke all on agent.egress_allow from anon, authenticated';
  end if;
end
$$;

grant usage on schema agent to agent_gateway;
grant select, insert, update, delete on agent.egress_allow to agent_gateway;
