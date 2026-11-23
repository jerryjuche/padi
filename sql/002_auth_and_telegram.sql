-- M0: authentication and Telegram state. Forward-only and idempotent.
-- Apply after sql/001_init.sql.

create table if not exists agent.login_tokens (
  token_hash bytea primary key,
  expires_at timestamptz not null,
  used boolean not null default false,
  attempts int not null default 0,
  created_at timestamptz not null default now()
);

create table if not exists agent.web_sessions (
  session_hash bytea primary key,
  created_at timestamptz not null default now(),
  last_seen_at timestamptz not null default now(),
  expires_at timestamptz not null,
  revoked boolean not null default false,
  user_agent text
);

create table if not exists agent.telegram_state (
  id int primary key default 1 check (id = 1),
  last_update_id bigint not null default 0
);

create table if not exists agent.telegram_messages (
  session_id uuid not null references agent.sessions(id),
  task_id uuid,
  chat_id bigint not null,
  message_id bigint not null,
  created_at timestamptz not null default now(),
  primary key (chat_id, message_id)
);

create index if not exists login_tokens_expires_idx on agent.login_tokens (expires_at);
create index if not exists web_sessions_expires_idx on agent.web_sessions (expires_at);
create index if not exists telegram_messages_session_idx on agent.telegram_messages (session_id);

alter table agent.login_tokens enable row level security;
alter table agent.web_sessions enable row level security;
alter table agent.telegram_state enable row level security;
alter table agent.telegram_messages enable row level security;

do $$
begin
  if exists (select 1 from pg_roles where rolname = 'anon') then
    execute 'revoke all on all tables in schema agent from anon, authenticated';
  end if;
end
$$;

grant select, insert, update, delete on all tables in schema agent to agent_gateway;

insert into agent.telegram_state (id, last_update_id) values (1, 0)
  on conflict (id) do nothing;
