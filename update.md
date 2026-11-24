# update.md — Session Continuity Record

**Project:** Sandbox Agent (single-user remote AI coding agent)
**Repository:** `/home/gamp/Desktop/padi` (remote `https://github.com/jerryjuche/padi.git`)
**Last updated:** 2026-11-23
**Current phase:** **M0 local + Supabase gates COMPLETE and approved; VM-only M0 drills explicitly deferred by owner (Option B); M1 Spine preflight started locally**

---

## 1. Repository state

- Branch: current `agent/m0-audit-fixes-b53482d` tracks `origin/agent/m0-audit-fixes-b53482d` and was pushed at `b53482d`. Local `main` still exists; `origin/main` is gone. **No default branch was pushed.**
- Authoritative specs now on disk: `README.md` (v0.6, 150 KB), `BUILD_SPEC.md` (v1.2, 109 KB), `docs/decision-record-v2.md` (moved from repo root to the canonical path), `CONTRACT.md`, `docs/decisions.md`, `docs/runbooks.md`.
- M0 files created per `BUILD_SPEC.md` §2.4 (see §4), plus `go.mod` (required to build the Go module; spec §1 explicitly permits a single root module).
- `skills/` and `.github/workflows/ci.yml` were **deliberately not created** in M0 (see §2 decision 3).
- Owner supplied `.env.local` (`SUPABASE_URL`, `SUPABASE_ANON_KEY`, `DATABASE_URL`). It is `chmod 600` and ignored via a newly added `.gitignore`. **No secret values are in tracked files or in this record.**
- Owner pasted a masked `sb_secret_...` key in chat; recommend rotating it in Supabase → API Keys. Our design never uses that key: the Go gateway uses the dedicated `agent_gateway` DB role.
- **Output-style preference (persistent):** owner asked to install and remember the `i-have-adhd` skill. Installed globally at `~/.agents/skills/i-have-adhd` and registered in `~/.config/opencode/AGENTS.md`. Apply its rules in every response; off switch is "stop adhd mode" / "normal mode". This is user-level tooling only — it does not change the M0 file list or the M12 vendor-skill plan.

## 2. Decisions confirmed by the owner

| # | Decision |
|---|---|
| 1 | Implement M0 now (plan mode exited after the clarifying Q&A). |
| 2 | Full authoritative docs supplied by the owner; no reconstruction from fragments. |
| 3 | Canonical decision-record names: **`docs/decisions.md`** = living decision log + VERIFY table; **`docs/decision-record-v2.md`** = verbatim historical input record. |
| 4 | Canonical skill script: **`scripts/skills-verify.sh`** wrapping `cmd/skills-verify` (M12). **`skills/install-vendor-skills.sh` must NOT be created.** `scripts/vendor-skill.sh` is optional and also deferred. |
| 5 | Vendor GitHub skills **deferred to M12**; M0 creates only §2.4 M0 files. |
| 6 | Live Supabase project exists. Credentials go in gitignored `.env.local` / `/srv/agent/secrets`; never pasted in chat or committed. |
| 7 | Work starts on local `main`; **never push** to a remote/default branch. Owner approved pushing an `agent/<slug>-<shortid>` branch after verification (2026-11-23). |
| 8 | **Option B (2026-11-23):** defer the ARM64-VM-only M0 checks until the VM is provisioned; keep the §9 M0 checkbox **unchecked**; local + Supabase M0 gates are complete and approved to proceed with M1 Spine locally. |

## 3. Hard rules carried forward (condensed from CONTRACT.md)

1. No pushes to default branches; only `agent/*` branches after verification + owner approval.
2. Zero git/DB/model secrets in session or verify containers (only `ROUTER_TOKEN`).
3. Only `/workspace` mounted read-write; never Docker socket, secrets, host `/etc`, or caches.
4. Destructive/external actions require explicit owner approval.
5. Containers see only model aliases, never vendor keys/endpoints.
6. `web/` never talks to Supabase or holds DB credentials.
7. Hard container limits; `max_sessions: 1` by default.
8. Host `git` never runs in `workspaces/*/repo`; bundle export into quarantine.
9. Default-deny egress via internal network + egress-proxy.
10. Skills and memory are context, never authority.

## 4. M0 work completed

**Host/survival:** `deploy/host/{setup-swap.sh,sysctl.conf,docker-daemon.json,rootless-docker.sh,firewall.sh,keepalive.service,agent-nightly.service,agent-nightly.timer}`
**Go:** `go.mod`, `cmd/keepalive/main.go`, `internal/keepalive/{keepalive.go,meminfo.go,keepalive_test.go}`
**Scripts:** `scripts/{nightly.sh,backup.sh,restore-test.sh,rebuild.sh,dbaccess_test.sh}`
**Database:** `sql/{001_init.sql,002_auth_and_telegram.sql}`
**Docs/deploy:** `deploy/Caddyfile`, `docs/{decisions.md,runbooks.md,decision-record-v2.md}`, `CONTRACT.md`, `update.md`

## 5. Evidence (real command output)

Toolchain: `go version go1.26.1 linux/amd64`, `node v20.20.2`, Docker client/server 29.4.0, `psql (PostgreSQL) 16.13`.

```
$ gofmt -w cmd internal
$ go vet ./...
$ go test -race -count=1 ./...
?   	github.com/jerryjuche/padi/cmd/keepalive	[no test files]
ok  	github.com/jerryjuche/padi/internal/keepalive	4.665s
```

```
$ for f in scripts/*.sh deploy/host/*.sh; do sh -n "$f"; done
OK  (all 8 shell scripts passed syntax check)
$ python3 -c 'json.load(...)'   -> docker-daemon.json OK
```

SQL applied twice to a **throwaway local PostgreSQL 16 database** (not the live Supabase project):
```
pass 1 ok
pass 2 ok            (idempotent; only "already exists, skipping" notices)
14                   (tables in schema agent)
rls: all 14 tables   relrowsecurity = t
telegram_state: 1|0  (single seeded row)
roles: agent_gateway, agent_router
```
Throwaway database and the two test roles were dropped afterwards.

Unit tests cover: `Desired` table cases (idle/busy/low-MemAvailable/dead-band/own-memory), no oscillation near target, keeper grows 128 MiB then yields to 0 on a busy host, hysteresis avoids one-block churn, `Run` stops on context cancel, `/proc/meminfo` parsing.

**Live Supabase read-only checks (2026-11-23, using `.env.local`):**
```
$ sh scripts/dbaccess_test.sh
PASS  sessions / repos / tasks / events / approvals / login_tokens / web_sessions /
      telegram_state / telegram_messages / system_flags / llm_usage / audit_log /
      provider_keys / risk_cache   -> HTTP 404 for all 14
dbaccess_test: anon key cannot read any agent table (14 checked)
$ psql "$DATABASE_URL" -Atc 'select current_user, current_database()'
postgres|postgres
$ psql "$DATABASE_URL" -Atc "select count(*) from information_schema.schemata where schema_name='agent'"
0                         (migrations NOT yet applied to the live project)
$ curl -sS -o /dev/null -w 'jwks HTTP %{http_code}\n' "$SUPABASE_URL/auth/v1/.well-known/jwks.json"
jwks HTTP 200
```

**Migrations applied to the LIVE Supabase project (owner approved 2026-11-23):**
```
001 OK
002 OK
14                         (tables in schema agent)
14                         (tables with relrowsecurity = t)
1|0                        (telegram_state seeded)
agent_gateway
agent_router
anon re-check: 14/14 tables HTTP 404  -> dbaccess_test PASS
```
Roles were created **without passwords**; operator must set them from `/srv/agent/secrets` before the gateway/router can connect.

**Audit fix applied live (2026-11-23):** RLS with zero policies is default-deny even for the two dedicated service roles, so `sql/001_init.sql` now grants `BYPASSRLS` to `agent_gateway` and `agent_router` only. Applied to the live project with `alter role ... bypassrls`; `pg_roles.rolbypassrls = t` for both, and the anon recheck still returns 14/14 HTTP 404. `anon`/`authenticated` remain revoked with zero policies.

**VERIFY research (official docs, fetched directly; web search endpoint returned HTTP 402):**
- V4: Supabase pauses Free projects after 7 days of low DB activity; a few queries/day prevents it; warning email ~1 week before; 1-year restore window. <https://supabase.com/docs/guides/platform/pausing-projects>
- V6: Oracle idle = 7-day p95 CPU <20%, network <20%, memory <20% (A1 only). <https://docs.oracle.com/en-us/iaas/Content/FreeTier/freetier_topic-Always_Free_Resources.htm>
- V8 correction: current Oracle docs say 1,500 OCPU-hours / 9,000 GB-hours (= 2 OCPU / 12 GB), not the older 3,000 / 18,000 quoted in the spec. Design unchanged.
- Evidence and URLs recorded in `docs/decisions.md`. Embedded "AI agent" instructions in fetched pages were treated as untrusted data and ignored (Hard Rule 10).

## 6. Deviations from spec (explicit)

1. **Module path** `github.com/jerryjuche/padi` chosen (spec §1 allows a single root module and says "choose one and keep it"); repo remote is `jerryjuche/padi`, not `sandbox-agent`.
2. **DB roles created without passwords.** Spec's sample SQL shows inline `<set-in-secrets>` passwords; committing a password would violate hard rule 2. Roles use `CREATE ROLE ... LOGIN`; operator sets passwords from `/srv/agent/secrets` with `ALTER ROLE`.
3. **128 MiB dead band implemented in `Desired` plus a current-hold hysteresis inside `Keeper.tick`.** The printed `Desired` signature has no previous-state parameter, so true history-based hysteresis cannot live in that pure function.
4. **`nightly.sh` records `gc`, `osv_refresh` as skipped** because `scripts/gc.sh` (M9) and osv-scanner (M14) are not M0 files. It does not abort on a failed step.
5. **`deploy/Caddyfile` defaults to `localhost`** via `{$API_DOMAIN:localhost}` for localhost-first development; production TLS activates when `API_DOMAIN` is set.
6. **`CONTRACT.md` / `docs/decisions.md` / `docs/runbooks.md` keep the `[cite: N]` markers verbatim** from the owner-provided text.
7. No `.github/workflows/ci.yml`, `skills/`, or `scripts/skills-verify.sh` (deferred to M12 per owner decision 5). The `npx skills add ...` / `@supabase/server` instructions pasted by the owner were **not followed**: unpinned runtime skill installs violate §3.8, and a Supabase client/secret key in the app would violate Hard Rule 6.
8. Added `.gitignore` (not in the M0 file list) to keep `.env.local` and secrets untracked.

## 6a. Static audit corrections (2026-11-23)

Independent static audit found 28 findings/gaps across Go, shell, SQL, spec evidence, security and tests. Fixed before any M1 work:

- **P0:** service roles now bypass RLS; `backup.sh` writes the raw dump before gzip so a failed `pg_dump` cannot produce a valid empty archive; CPU duty burns continuously in 100 ms slices inside the window; `dbaccess_test.sh` fails on any HTTP 200 (even `[]`).
- **P1:** safety cap is re-applied after hysteresis; rootless `DOCKER_HOST` is derived in `nightly.sh` and `restore-test.sh`; Docker log rotation config is installed; oneshot nightly has `TimeoutStartSec=0`; restore drill picks the newest dump.
- **P2:** restore drill verifies 14 tables, 14 RLS-enabled tables and the seed row, then prints row counts without failing on legitimately empty business tables; throwaway DB has memory/swap/CPU/PID limits; `gunzip` writes to a file before `psql`; `rebuild.sh` uses the absolute Go binary and waits for the user bus; `setup-swap.sh` formats an existing unformatted swapfile; CLI duty flags are range-checked; `printf` newline fixed.
- **Tests added:** duty-window boundary, cancellation during burn, cap calculation, cap-vs-hysteresis regression, state-write error propagation, RFC3339/target assertions.
- **Tracked low-severity follow-up:** host-side credentials remain visible in process argv (`pg_dump`, curl headers) and systemd `EnvironmentFile` escaping needs care. No session/verify container exists in M0, so Hard Rule 2 is unaffected. Revisit if M1 requires it.

## 7. M0 acceptance status and owner-approved deferrals

**Owner decision (Option B, 2026-11-23):** the local + Supabase M0 gates are complete and approved; the remaining ARM64-VM-only drills are explicitly **deferred until the Oracle ARM64 VM is provisioned** and no longer block M1 Spine on localhost.

### Complete and accepted locally / on Supabase

| Item | Status |
|---|---|
| `go vet ./... && go test -race -count=1 ./...`, `sh -n` on all shell scripts | PASS (§5, §6a). |
| SQL migrations applied twice, idempotent; 14 tables, all RLS on, `telegram_state` seeded, dedicated roles with `BYPASSRLS` | PASS locally and on the live Supabase project (§5). |
| `scripts/dbaccess_test.sh` against real Supabase | **PASS** — 14/14 tables HTTP 404 for the anon/publishable key (§5). |
| `restore-test.sh` against a real dump | **PASS** — `postgres:17` dump restored into a resource-limited throwaway container; `agent tables=14 rls_tables=14 telegram_state_seed=1` (§5). |
| V4 (localhost connectivity + pause rule), V6 (reclaim thresholds), V8 (allowance), V14 (rclone design) | Recorded in `docs/decisions.md`; VM-side portions remain in the deferred list below. |

### Deferred to the ARM64 VM (owner Option B)

| Item | Requirement that remains |
|---|---|
| Keepalive live floor | Run the built `keepalive` binary on the ARM64 VM; prove it holds the 4 GB memory floor and yields to a task (V9/V6 live portion). |
| Rootless 256 MB OOM drill | Provision rootless Docker with cgroup v2 delegation and kill a 256 MB-limited container that allocates 1 GB (V9). |
| rclone Object Storage upload | Real upload/prune to the private bucket using `oracleobjectstorage` + instance principal; needs bucket, namespace, region, compartment OCID, dynamic group and policy (§11, V14). |
| Caddy HTTPS | Set `API_DOMAIN`, point DNS at the VM, run Caddy, and verify `https://<API_DOMAIN>/healthz` plus VM reboot health. |
| External monitor drill | Dead-man's-switch ping to `HEALTHCHECK_PING_URL` and external HTTPS uptime monitor alert verified end to end (V14). |

SSH access is still unavailable; add the gitignored `AGENT_VM_*` values locally when the VM is ready (§11.1). Do not invent or commit them.

**`BUILD_SPEC.md` §9 M0 checkbox stays intentionally unchecked** until those five live VM drills are executed and recorded here and in `docs/decisions.md`. This does *not* block M1 Spine locally.

## 8. Open questions for the owner

1. ~~Can you supply `SUPABASE_URL` / anon key?~~ **DONE** — `.env.local` supplied; anon test and migrations pass.
2. What is `API_DOMAIN` (`api.<domain>`), the Object Storage bucket name, `OOS_NAMESPACE`, `OCI_REGION`, `COMPARTMENT_OCID`, and `HEALTHCHECK_PING_URL`? DNS must point at the VM.
3. ~~Should Object Storage upload use rclone or a Go helper?~~ **RESOLVED — use rclone** with instance-principal auth; no Python, no static keys. See §11.

## 9. Progress log

- **2026-11-23 (early)** — Repo inspected: empty, no commits, local `main`. Asked four clarifying questions; answers recorded. Created `update.md`. No implementation.
- **2026-11-23 (later)** — Owner supplied the three ZIP spec files plus canonical answers and the remaining M0 docs. Moved `decision-record-v2.md` into `docs/`; wrote `CONTRACT.md`, `docs/decisions.md`, `docs/runbooks.md`.
- **2026-11-23 (now)** — Implemented the full M0 file list. `go vet ./...` and `go test -race -count=1 ./...` pass. All 8 shell scripts pass `sh -n`. SQL migrations apply twice cleanly to a throwaway local PostgreSQL: 14 tables, all RLS on, telegram_state seeded, both roles created.
- **2026-11-23 (connection check)** — Owner added `.env.local`. Added `.gitignore`, set mode 600. Live Supabase: REST anon test PASS (14/14 HTTP 404), direct psql PASS (role `postgres`), JWKS HTTP 200, `agent` schema absent. Owner pasted a masked secret key in chat; rotation advised.
- **2026-11-23 (approved work)** — Owner approved applying migrations and web research. Applied `001`/`002` to the live project: 14 tables, all RLS on, telegram_state seeded, both roles created, anon re-check still 14/14 HTTP 404. Resolved V4/V6 with official docs, recorded V8 correction and Sources in `docs/decisions.md`. Web search tool returned HTTP 402, so official doc URLs were fetched directly. SSH details still not supplied.
- **2026-11-23 (VM status)** — Owner reported the ARM64 VM is **not ready yet**. Paused all VM-side checks. Local + Supabase portions of M0 are complete; the milestone is not accepted.
- **2026-11-23 (V14 research + runbook)** — Fetched primary docs for rclone's native Oracle backend, OCI S3 compatibility, credentials, boot-volume backups and free monitor tiers. Resolved V14 in favour of rclone + instance principal (no keys on VM); recorded sources in `docs/decisions.md`. Added the ordered VM command checklist in §11. Found and fixed a defect: `scripts/rebuild.sh` required root but called `rootless-docker.sh`, which refuses root — it now invokes it via `runuser` as the source-tree owner.
- **2026-11-23 (persistent output style)** — Installed the `i-have-adhd` skill globally at `~/.agents/skills/i-have-adhd` and registered the preference in `~/.config/opencode/AGENTS.md`. Applies to every project; off switch is "stop adhd mode" / "normal mode". User-level tooling only; no M0 file-list change.
- **2026-11-23 (static audit + fixes)** — Independent audit found 4 P0 defects. Fixed all P0/P1/P2 items listed in §6a, reran `gofmt`, `go vet ./...` and `go test -race ./...` (green), reran `sh -n` on all 8 shell scripts, and reapplied SQL migrations twice to a throwaway Postgres (`14|14|2|0`). Applied `BYPASSRLS` to the two service roles live; anon still 14/14 HTTP 404. Produced a real Supabase dump with `postgres:17` `pg_dump` and passed `scripts/restore-test.sh` against it. M0 remains unaccepted only because VM-side checks are pending.
- **2026-11-23 (approved push)** — Owner asked to push to a branch and selected `agent/m0-audit-fixes-b53482d`. Created that branch from `main` and pushed it to `origin` at `b53482d`. Default branch untouched. §11.2 now clones this branch on the VM instead of using rsync.
- **2026-11-23 (Option B + M1 preflight)** — Owner chose Option B: defer the five VM-only M0 drills, keep the §9 M0 checkbox unchecked, and start M1 Spine locally. Recorded the deferral in §7 and `docs/decisions.md`. Resolved V1/V12 against real `opencode v2.4.0` (local OpenAPI schema, config schema and a live `serve` probe); recorded the exact M1 file list and temporary deviations in §12. No M1 code written yet.

## 10. Handoff instructions for the next session

1. Read this file first, then `BUILD_SPEC.md` §2.4 M1, §3.3, §3.7, §7 and §8.
2. M1 Spine **is authorized** locally under owner Option B. Before code: post the exact M1 file list, resolve/record V1 and V12 in `docs/decisions.md`, and record the temporary M1 deviations and `gitexec.Run` conventions (§12).
3. Build M1 with table-driven tests; run `go vet ./... && go test -race -count=1 ./...`; verify the local Telegram → container → OpenCode → independent clone → Telegram summary loop; stop with the `BUILD_SPEC.md` §2.5 report.
4. Never push to `main` or any default branch. Only push approved `agent/*` branches after verification. Never put secrets in files or chat.
5. When the VM is ready, execute the five deferred drills in §7 and work through §11 in order. Do not skip the read-only recon in §11.1. Only then tick the M0 box in `BUILD_SPEC.md` §9.

---

## 11. ARM64 VM command checklist (run in order)

**Why this exists:** the approved review branch is pushed, so the VM can clone it from `origin` (§11.2) without copying local secrets. Every step below is idempotent unless marked otherwise. Placeholders in `<ANGLE_BRACKETS>` are values the owner supplies; **no secret values belong in this file or in chat**.

### 11.0 One-time console/account setup (owner, before SSH)

1. Create a private Object Storage bucket, e.g. `agent-backups`, and note its **bucket name**, **namespace**, **region**, and **compartment OCID**.
2. Create a **dynamic group** whose matching rule includes this instance:
   ```
   ANY {instance.id = '<INSTANCE_OCID>'}
   ```
   (or scope by compartment if preferred).
3. Add a least-privilege policy, e.g.:
   ```
   Allow dynamic-group <DYNAMIC_GROUP> to manage objects in compartment id '<COMPARTMENT_OCID>' where target.bucket.name = 'agent-backups'
   ```
   `manage objects` is required because `backup.sh` deletes objects older than 14 days.
4. Enable **policy-based automatic boot-volume backups** (Always Free includes 5 free volume backups).
5. Create a free [Healthchecks.io](https://healthchecks.io/) check for the dead-man's switch; note the ping UUID (`https://hc-ping.com/<UUID>`). For the drill in §11.10, temporarily set its period/grace to a few minutes, then restore ~24 h + grace afterwards.
6. Set up an external HTTPS uptime monitor (e.g. UptimeRobot free tier) for `https://<API_DOMAIN>/healthz`.
7. Point `API_DOMAIN` DNS at the VM's public IP.

### 11.1 First SSH — read-only recon

Add the SSH values to the gitignored `.env.local` **on this machine** (names only shown):
```
AGENT_VM_HOST=<ip or hostname>
AGENT_VM_USER=<ssh user>
AGENT_VM_KEY=/absolute/path/to/passphrase-free/private/key
AGENT_VM_PORT=22
```
Then confirm we are on the right host before changing anything:
```sh
set -a; . ./.env.local; set +a
ssh -i "$AGENT_VM_KEY" -p "${AGENT_VM_PORT:-22}" -o BatchMode=yes \
  "$AGENT_VM_USER@$AGENT_VM_HOST" \
  'echo "ARCH=$(uname -m)"; . /etc/os-release; echo "OS=$PRETTY_NAME"; free -m; stat -fc %T /sys/fs/cgroup; systemd --version | head -1; command -v docker || true; command -v rclone || true; id'
```
Expected: `ARCH=aarch64`, cgroup `cgroup2fs`, `docker`/`rclone` absent initially, owner user is **not** root.

### 11.2 Clone the pushed review branch on the VM and install prerequisites

The approved branch `agent/m0-audit-fixes-b53482d` now exists on `origin` at `b53482d`. Do **not** clone the default branch. On the VM, replace `<VM_USER>` with the non-root SSH user, then clone as that user so `rebuild.sh` can hand rootless Docker work back to it:

```sh
sudo install -d -o <VM_USER> -g <VM_USER> -m 755 /srv/agent
sudo -u <VM_USER> git clone --branch agent/m0-audit-fixes-b53482d --single-branch \
  https://github.com/jerryjuche/padi.git /srv/agent/src
```
On the VM, as root:
```sh
sudo apt update
sudo apt install -y ca-certificates curl git jq ufw unzip rsync \
  postgresql-client uidmap dbus-user-session slirp4netns fuse-overlayfs
```
Install pinned Go 1.26 (arm64) and verify the checksum from `https://go.dev/dl/?mode=json`:
```sh
curl -fLO "https://go.dev/dl/go1.26.1.linux-arm64.tar.gz"
# verify sha256 against the JSON manifest before extracting
sudo rm -rf /usr/local/go && sudo tar -C /usr/local -xzf go1.26.1.linux-arm64.tar.gz
/usr/local/go/bin/go version   # expect go1.26.1 linux/arm64
```

### 11.3 rclone + instance principal (no credentials on disk)

Install a **pinned** static arm64 build (check the current version at <https://rclone.org/downloads/>):
```sh
RCLONE_VER=<PINNED_VERSION>
curl -fLO "https://downloads.rclone.org/v${RCLONE_VER}/rclone-v${RCLONE_VER}-linux-arm64.zip"
curl -fLO "https://downloads.rclone.org/v${RCLONE_VER}/SHA256SUMS"
grep "rclone-v${RCLONE_VER}-linux-arm64.zip" SHA256SUMS | sha256sum -c -
unzip -o "rclone-v${RCLONE_VER}-linux-arm64.zip"
sudo install -m 755 "rclone-v${RCLONE_VER}-linux-arm64/rclone" /usr/local/bin/rclone
```
Write the remote config (this contains **no secrets** — instance identity comes from metadata):
```sh
sudo install -d -m 700 /root/.config/rclone
sudo tee /root/.config/rclone/rclone.conf >/dev/null <<'EOF'
[oos]
type = oracleobjectstorage
provider = instance_principal_auth
namespace = <OOS_NAMESPACE>
region = <OCI_REGION>
compartment = <COMPARTMENT_OCID>
EOF
sudo chmod 600 /root/.config/rclone/rclone.conf
# nightly.sh runs as root; also give the owner user a copy for interactive checks
sudo install -d -m 700 -o "$AGENT_VM_USER" -g "$AGENT_VM_USER" "/home/$AGENT_VM_USER/.config/rclone"
sudo install -m 600 -o "$AGENT_VM_USER" -g "$AGENT_VM_USER" \
  /root/.config/rclone/rclone.conf "/home/$AGENT_VM_USER/.config/rclone/rclone.conf"
```
Verify the dynamic-group policy works:
```sh
rclone lsd oos:                 # must list the bucket
rclone ls oos:<BUCKET>          # must succeed (empty is fine)
```
If either fails, stop: the instance-principal policy is wrong. Do **not** fall back to Customer Secret Keys without an explicit decision.

### 11.4 Secrets and DB roles

```sh
sudo install -d -m 700 /srv/agent/secrets
sudo tee /srv/agent/secrets/gateway.env >/dev/null <<'EOF'
DATABASE_URL=postgresql://agent_gateway:<PW>@<DB_HOST>/postgres
SUPABASE_URL=https://<ref>.supabase.co
SUPABASE_ANON_KEY=<anon key>
RCLONE_REMOTE=oos:<BUCKET>
HEALTHCHECK_PING_URL=https://hc-ping.com/<UUID>
EOF
sudo chmod 600 /srv/agent/secrets/gateway.env
```
Set the role passwords interactively so they never appear in shell history or process arguments:
```sh
psql "$MIGRATION_DATABASE_URL"     # the postgres-level connection string
-- at the psql prompt:
\password agent_gateway
\password agent_router
\q
```
`MIGRATION_DATABASE_URL` is only needed here and for future migrations; day-to-day `DATABASE_URL` uses `agent_gateway` (it has DML on `agent` and can run the nightly heartbeat).

### 11.5 Bootstrap (idempotent)

```sh
sudo sh /srv/agent/src/deploy/host/firewall.sh
sudo env AGENT_VM_USER="$AGENT_VM_USER" PATH=/usr/local/go/bin:$PATH \
  sh /srv/agent/src/scripts/rebuild.sh
```
`rebuild.sh` now calls `rootless-docker.sh` through `runuser` as the owner (the earlier root-only version would have aborted). It installs sysctl/swap, builds `/usr/local/bin/keepalive`, installs the units, restores config from the newest archive if present, and enables `keepalive.service` + `agent-nightly.timer`.

Caddy (HTTPS) is not in the M0 file list, so install it manually and keep the Caddyfile verbatim:
```sh
sudo apt install -y debian-keyring debian-archive-keyring apt-transport-https
curl -1sLf 'https://dl.cloudsmith.io/public/caddy/stable/gpg.key' \
  | sudo gpg --dearmor -o /usr/share/keyrings/caddy-stable-archive-keyring.gpg
curl -1sLf 'https://dl.cloudsmith.io/public/caddy/stable/debian.deb.txt' \
  | sudo tee /etc/apt/sources.list.d/caddy-stable.list >/dev/null
sudo apt update && sudo apt install -y caddy
sudo install -m 644 /srv/agent/src/deploy/Caddyfile /etc/caddy/Caddyfile
sudo install -d -m 755 /etc/systemd/system/caddy.service.d
printf '[Service]\nEnvironment=API_DOMAIN=%s\n' "$API_DOMAIN" \
  | sudo tee /etc/systemd/system/caddy.service.d/override.conf >/dev/null
sudo systemctl daemon-reload && sudo systemctl enable --now caddy
```
Reboot once, then confirm everything came back:
```sh
sudo reboot
# after reconnect:
systemctl is-active keepalive.service caddy agent-nightly.timer
docker --version
```

### 11.6 Accept: keepalive holds and yields

```sh
cat /srv/agent/state/keepalive.json
journalctl -u keepalive -n 50 --no-pager
free -m
```
Expected: `resident_mb` ≈ 4096 on an idle host (64 MiB steps), log lines "memory floor adjusted". Then create memory pressure for ~30 s and watch the floor drop:
```sh
timeout 30s awk 'BEGIN{while(1) s=s "aaaaaaaaaa"}' &
watch -n 2 'cat /srv/agent/state/keepalive.json; free -m'
```
Expected: `resident_mb` falls as `MemAvailable` shrinks; it grows back after the process exits. **Caution:** keep an SSH session open and stop the test process immediately if `MemAvailable` approaches 2.5 GB.

### 11.7 Accept: rootless Docker 256 MB OOM kill

```sh
docker run -d --name agent-oom-test --memory=256m --memory-swap=256m \
  alpine:3.20 awk 'BEGIN{while(1) s=s "aaaaaaaaaa"}'
sleep 20
docker inspect agent-oom-test --format 'OOMKilled={{.State.OOMKilled}} ExitCode={{.State.ExitCode}}'
docker rm -f agent-oom-test
```
Expected: `OOMKilled=true ExitCode=137`. If `OOMKilled=false`, cgroup delegation failed — re-check `Delegate=cpu memory pids` and cgroup v2 before proceeding.

### 11.8 Accept: nightly, backup upload, restore

```sh
sudo systemctl start agent-nightly.service
cat /srv/agent/state/nightly.json          # every step ok or explicitly skipped
rclone ls oos:<BUCKET>/agent               # must show agent-*.sql.gz and config-state-*.tar.gz
sudo sh /srv/agent/src/scripts/restore-test.sh
```
`restore-test.sh` restores into a throwaway `postgres:16-alpine` container and prints row counts; it never touches the live database.

### 11.9 Accept: HTTPS

```sh
curl -fsS "https://<API_DOMAIN>/healthz"    # expect: ok
curl -sS -o /dev/null -w '%{http_code}\n' "https://<API_DOMAIN>/nope"   # expect 404
```

### 11.10 Accept: external monitor drill

1. Confirm the uptime monitor shows `https://<API_DOMAIN>/healthz` as UP.
2. Trigger the dead-man's switch by stopping the VM (from the OCI console) or `sudo shutdown -h now`.
3. Wait past the check's grace period; confirm Healthchecks.io sends the alert.
4. Start the VM, confirm the next nightly ping clears the check, then **restore the check's normal ~24 h period + grace**.
5. Record the timestamps and screenshots.

### 11.11 Record and close M0

Paste the real outputs (with secrets redacted) into this file §5 and `docs/decisions.md` VERIFY rows V4/V6/V8/V9/V14. Only when every item above passes: tick the M0 box in `BUILD_SPEC.md` §9 and send the §2.5 report. **Owner has already given explicit approval (Option B) to proceed with M1 locally while these VM drills are deferred** (§7); this checklist remains the condition for closing M0, not for starting M1.

---

## 12. M1 Spine preflight

### 12.1 Exact files to create per `BUILD_SPEC.md` §2.4 M1

| Area | Files |
|---|---|
| Session image | `deploy/session/Dockerfile`, `deploy/session/entrypoint.sh`, `deploy/session/opencode.base.json`, `deploy/session/AGENTS.target.md` |
| Gateway entrypoint | `cmd/gateway/main.go` (minimal wiring) |
| Telegram | `internal/gateway/telegram/*` (poller, owner allowlist, reply streaming) |
| OpenCode client | `internal/gateway/opencode/*` including `internal/gateway/opencode/translate.go` |
| Runner | `internal/gateway/runner/*` (`Start`, `Wait`, `Stop`, `Exec`, `Reconcile`) |
| GitOps (M1 subset) | `internal/gateway/gitops/*` limited to `MintInstallationToken`, `EnsureCache`, `CloneForTask` |
| Store (minimal) | `internal/gateway/store/*` (events, tasks) |
| Config | `config/gateway.yaml` |

Additional test files will be colocated with each package (table-driven, `-race`). No `sql/003_harness.sql`, `gitexec.go`, `bundle.go`, `quarantine.go`, `deploy/docker-compose.yml` or `cmd/agent-verify` in M1 — those belong to M3a/M3b.

### 12.2 VERIFY results before code

V1 and V12 are resolved against the installed **`opencode v2.4.0`** binary, its live `/openapi.json`, the official `https://opencode.ai/config.json` schema and official docs. Full evidence is in `docs/decisions.md`. Headlines: `serve` exposes `/api/session`, `/api/session/{id}/prompt`, `/api/event` (SSE) and `/api/session/{id}/model`; HTTP auth is Basic with username `opencode` and password from `OPENCODE_PASSWORD` (unauthenticated probe returned 401); model changes are per-session, not per-message; `lsp` and `snapshots` are valid config keys, custom headers belong under `providers.<id>.headers`, and MCP servers under `mcp.servers`.

### 12.3 Temporary deviations and conventions

1. M1 only: a **throwaway free-tier model API key** is passed to the session container via environment variable at run time. It is never baked into the Dockerfile, image, compose file, config file, git or chat, and is revoked when M2 passes.
2. M1 only: containers use a plain bridge network named `agent-session-net` (not `internal`); M3a replaces it and proves isolation with `netcheck.sh`.
3. From day one: host `git` is never run inside `workspaces/*/repo`. All gateway git calls go through a single hardened `gitexec.Run` wrapper (`exec.CommandContext`, no shell, per-call timeout, neutral git env/config, protocol allowlist). M1 only needs the cache/installation-token/clone subset; the lint test that forbids any other `exec.Command("git", ...)` is specified for M3a, but the convention is followed now. Code leaves the container only as `out.bundle` fetched into a gateway-owned quarantine repo (M3a).

- **2026-11-23 (M1 complete)** — Implemented and verified the M1 Spine locally. `gofmt`, `go vet` and `go test -race` are green. Host probe drove real `opencode v2.4.0` with a fake OpenAI-compatible provider and executed an `edit` tool against a real README. Full local loop passed: fake Telegram → real Docker container → real OpenCode → independent clone edit → Telegram summary; task `DONE`, 9 events, no leftover container, no credential-like image env. Corrected V1/V12 evidence with data-only SSE frames and observed completion events. No push performed; M0 checkbox remains unchecked per Option B. Full §2.5 report in §13.

---

## 13. M1 Spine report (`BUILD_SPEC.md` §2.5)

### Done

- Session image: `deploy/session/Dockerfile`, `deploy/session/entrypoint.sh`, `deploy/session/opencode.base.json`, `deploy/session/AGENTS.target.md`.
- Gateway: `cmd/gateway/main.go`, `cmd/gateway/config.go`, `cmd/gateway/config_test.go`, `cmd/gateway/entrypoint_test.go`.
- Config: `config/gateway.yaml`.
- Telegram: `internal/gateway/telegram/api.go`, `poller.go`, `render.go`, `telegram_test.go`.
- OpenCode: `internal/gateway/opencode/client.go`, `translate.go`, `client_test.go`, `translate_test.go`.
- Runner: `internal/gateway/runner/docker.go`, `runner.go`, `runner_test.go`.
- GitOps: `internal/gateway/gitops/gitexec.go`, `git.go`, `gitexec_lint_test.go`, `git_test.go`.
- Store: `internal/gateway/store/store.go`, `store_test.go`.
- Module/dependencies: `go.mod`, `go.sum`.
- Continuity/evidence: `docs/decisions.md` (V1/V12 corrections), `update.md` (this report).

### Evidence

Formatting and static checks:

```text
$ gofmt -l cmd/gateway internal/gateway
(no output)

$ git diff --check
(no output)
```

```text
$ GOFLAGS=-mod=mod GOPROXY=off go vet ./... && GOFLAGS=-mod=mod GOPROXY=off go test -race -count=1 ./...
ok  	github.com/jerryjuche/padi/cmd/gateway	1.315s
?   	github.com/jerryjuche/padi/cmd/keepalive	[no test files]
ok  	github.com/jerryjuche/padi/internal/gateway/gitops	2.317s
ok  	github.com/jerryjuche/padi/internal/gateway/opencode	1.160s
ok  	github.com/jerryjuche/padi/internal/gateway/runner	1.467s
ok  	github.com/jerryjuche/padi/internal/gateway/store	1.128s
ok  	github.com/jerryjuche/padi/internal/gateway/telegram	1.420s
ok  	github.com/jerryjuche/padi/internal/keepalive	5.378s
```

Host-side probe with real `opencode v2.4.0`, fake OpenAI-compatible provider and SSE capture:

```text
SID=ses_df73f4363ffefENgxSIBv33Sfe
INFO:
{"version":"2.4.0","pid":178387,"hostname":"CL2-T1-S03","urls":["http://127.0.0.1:4598"],"paths":{"tmp":"/tmp/opencode"},"capabilities":{"persistentPty":true}}
SSE (delta/idle lines):
data: {"id":"evt_208c0f899001jgGhrEeaZ4zSY7","created":1795443194009,"type":"session.text.delta","location":{"directory":"/tmp/opencode/padi-m1-loop/host-probe"},"data":{"sessionID":"ses_df73f4363ffefENgxSIBv33Sfe","assistantMessageID":"msg_208c0f808001esta5QGmGaHX18","ordinal":0,"delta":"M1 loop verified with a fake model provider."}}
FAKE REQUEST PATHS:
/v1/chat/completions	m1-model	0
/v1/chat/completions	m1-model	14
/v1/chat/completions	m1-model	14
README AFTER:
# fixture

M1 loop verified by fake provider
```

Final local loop (evidence directory `/tmp/opencode/padi-m1-loop/evidence-1795445642470`):

```text
bridge_gateway=172.20.0.1
=== M1 LOCAL LOOP EVIDENCE ===
stamp=1795445642470
task_id=fc833f21-2eb7-4bba-8d44-11f40cddeff1
branch=agent/tmp-opencode-padi-m1-loop-source-fc833f21
gateway_exit=0
summary_received=1
task_state=DONE
event_count=9
source_inode=5961547 workspace_inode=6753100
leftover_containers=[]

--- source README (must stay unchanged) ---
# fixture
--- workspace README (must contain marker) ---
# fixture

M1 loop verified by fake provider
--- telegram outbox ---
{"method":"sendMessage","text":"Working..."}
{"method":"editMessageText","text":"Running a tool..."}
{"method":"editMessageText","text":"Running a tool..."}
{"method":"editMessageText","text":"Tool finished."}
{"method":"editMessageText","text":"Working..."}
{"method":"editMessageText","text":"M1 loop verified with a fake model provider."}
{"method":"editMessageText","text":"M1 loop verified with a fake model provider."}
--- gateway log ---
2026/11/23 15:54:25 gateway: M1 spine ready (model=fake/m1-model, network=agent-session-net)
--- fake provider requests (path/model/tool-count) ---
/v1/chat/completions	m1-model	0
/v1/chat/completions	m1-model	14
/v1/chat/completions	m1-model	14
--- image env credential scan ---
PASS: no credential-like env keys in image
--- evidence files ---
/tmp/opencode/padi-m1-loop/evidence-1795445642470
```

Persisted events for the completed task:

```text
1|status|{"state": "QUEUED"}
2|status|{"state": "PREPARING", "branch": "agent/tmp-opencode-padi-m1-loop-source-fc833f21"}
3|tool_call|{"text": "", "tool": "", "message": ""}
4|tool_call|{"text": "", "tool": "", "message": ""}
5|tool_result|{"text": "", "tool": "", "message": ""}
6|status|{"text": "", "tool": "", "message": "Working..."}
7|token|{"text": "M1 loop verified with a fake model provider.", "tool": "", "message": ""}
8|done|{"text": "", "tool": "", "message": ""}
9|done|{"summary": "M1 loop verified with a fake model provider."}
```

Provider headers observed by the fake provider:

```text
{"x-session-affinity":"ses_df7191ed7ffek7wV30trPMsS48","x-session-id":"ses_df7191ed7ffek7wV30trPMsS48","x-task-id":"fc833f21-2eb7-4bba-8d44-11f40cddeff1"}
```

Acceptance mapping to `BUILD_SPEC.md` M1:

- “Clone repo X and add a line to the README” edited the independent clone: PASS (source inode `5961547` unchanged; workspace inode `6753100` contains marker).
- Telegram shows the summary: PASS (`summary_received=1`, outbox final edit contains `M1 loop verified with a fake model provider.`).
- Container gone afterwards: PASS (`leftover_containers=[]`).
- No git credential in the container: PASS (`no credential-like env keys in image`; git access used the local fixture through `gitexec.Run` only).
- V1/V12: PASS with live evidence; details in `docs/decisions.md`.

### Deviations

1. M1 temporary deviations already recorded: throwaway model key via environment variable only, and plain bridge `agent-session-net`. M2/M3a replace them.
2. Local verification used a fake Telegram Bot API, a fake OpenAI-compatible provider and a local git fixture instead of real external credentials. The Docker container, OpenCode binary and git clone path were real.
3. Live V1 evidence corrected the pre-code expectation: v2.4.0 SSE frames are data-only (`type` inside JSON), and successful completion was observed as `session.text.ended` plus `session.execution.succeeded`, not `session.idle`. The client now accepts data-only frames; the translator treats `session.text.ended`/`session.idle` as done and execution/step failures as terminal errors.
4. Bundle export, quarantine, verification container, approvals and push are deliberately absent from M1 per §2.4; they belong to M3a/M3b.

### Open items

- ARM64 VM still not provisioned. Five VM-only M0 drills remain deferred by owner Option B; `BUILD_SPEC.md` §9 M0 checkbox stays unchecked.
- No real Telegram bot token, GitHub App credentials or real model key was supplied; local fakes were used. The throwaway key must be revoked when M2 passes.
- `agent_gateway` / `agent_router` DB roles still need passwords set from `/srv/agent/secrets` on the VM.
- The masked `sb_secret_...` key pasted earlier in chat must be rotated by the owner.
- No push was performed. Current work is uncommitted on `agent/m0-audit-fixes-b53482d`.

### Next

M2 Router v1, awaiting owner go-ahead. It retires the M1 throwaway key, introduces model aliases/rate limiting, and verifies custom-provider and tool-call passthrough.

---

## 14. Credential paste guide (NVIDIA + Groq)

### 14.1 Where to paste

All values go in `.env.local` (mode 600, gitignored). The gateway reads process environment, not the file directly, so load it before starting:

```sh
set -a; . ./.env.local; set +a
go run ./cmd/gateway
```

Never paste a token or private key into chat, `config/gateway.yaml`, an image or git.

Leave `TELEGRAM_API_BASE` and `PADI_MODEL_BASE_URL` **empty** unless you are pointing them at a local stub or proxy. The default Telegram API host is `https://api.telegram.org`; setting `TELEGRAM_API_BASE=https://telegram.org` (the website) breaks every Bot API call. `GITHUB_APP_PRIVATE_KEY_PATH` must point to a PEM **file** (mode 600), not a directory.

### 14.2 Telegram

1. In Telegram, message **@BotFather** → `/newbot` → follow prompts → copy the token into `TELEGRAM_BOT_TOKEN`.
2. Get your numeric user ID (for example from **@userinfobot**) → `TELEGRAM_ALLOWED_USER_ID`. Only this ID is admitted.
3. `TELEGRAM_OWNER_ID` and `GITHUB_INSTALLATION_ID` remain accepted aliases; the README names are now canonical and the code accepts both.

### 14.3 GitHub — create a GitHub App, not a PAT

Per `README.md` §8.8:

1. **Settings → Developer settings → GitHub Apps → New GitHub App** under Jerry's account.
2. Repository permissions: **Contents: read and write**, **Pull requests: read and write**, **Metadata: read**. Grant **no** Actions, Workflows or Administration permission.
3. Generate a private key in the App settings → download the PEM → store it on the host as `/srv/agent/secrets/github-app.pem` with mode 600. Only the path goes in `GITHUB_APP_PRIVATE_KEY_PATH`; never the key contents.
4. Copy the **App ID** into `GITHUB_APP_ID`.
5. Install the App on **selected repositories only** (that list is the repo allowlist). The installation ID is in the install URL / App settings installations page → `GITHUB_APP_INSTALLATION_ID`.
6. On each allowed repo, enable branch protection on the default branch: require PRs, block force pushes.
7. The gateway signs a short-lived RS256 JWT, exchanges it for an installation token and passes it per git command via `-c http.extraheader=...`. It never writes the token to git config, an image or a container.

A local path repo (`clone /tmp/...`) needs none of these; the fake GitHub credentials were used only for the local loop.

### 14.4 Model choice

`nemotron-3.5-bolt-e4b` is a valid NVIDIA API model (`nvidia/nemotron-3.5-bolt-e4b`) but is **absent from this OpenCode v2.4.0 bundled catalog**, so `deploy/session/opencode.base.json` declares it explicitly as an override. OpenCode then references it as `nvidia/nvidia/nemotron-3.5-bolt-e4b`.

Active configuration (already applied):

```yaml
# config/gateway.yaml
model: "nvidia/nvidia/nemotron-3.5-bolt-e4b"
model_key_env: "NVIDIA_API_KEY"
```

Paste your NVIDIA key into `PADI_MODEL_API_KEY` in `.env.local`; the gateway injects it into the session container under `NVIDIA_API_KEY`. Key link: <https://build.nvidia.com/settings/api-keys>.

Groq is declared as a custom OpenAI-compatible provider in the same base config (`env: ["GROQ_API_KEY"]`, base URL `https://api.groq.com/openai/v1`, models `openai/gpt-oss-120b` and `llama-3.3-70b-versatile`). To switch:

1. Paste the Groq key into `PADI_MODEL_API_KEY` (<https://console.groq.com/keys>).
2. In `config/gateway.yaml`, set `model: "groq/openai/gpt-oss-120b"` and `model_key_env: "GROQ_API_KEY"`.

No image rebuild is needed for the switch: the base JSON is copied into the image at build time, so only a rebuild is required if you add a *new* provider or model. Verified locally with `opencode models`: both `nvidia/nvidia/nemotron-3.5-bolt-e4b` and the `groq/...` models appear, and `opencode serve` accepts the dual-provider config with no keys set.

### 14.5 Sanity checks after filling the file

```sh
set -a; . ./.env.local; set +a
test -n "$TELEGRAM_BOT_TOKEN" && test -n "$TELEGRAM_ALLOWED_USER_ID" && echo telegram-ok
test -n "$PADI_MODEL_API_KEY" && echo model-key-ok
GOFLAGS=-mod=mod GOPROXY=off go vet ./... && GOFLAGS=-mod=mod GOPROXY=off go test -race -count=1 ./...
```

---

## 15. Live credential verification and gateway smoke test (owner-supplied values)

Date: 2026-11-23. All checks used the real values in the owner's gitignored `.env.local`; no credential value is recorded here.

### 15.1 Corrections made before testing

- `TELEGRAM_API_BASE` was set to `https://telegram.org` (website). Cleared so the default `https://api.telegram.org` is used.
- `PADI_MODEL_BASE_URL` was set to the NVIDIA endpoint already used by the provider. Cleared as a redundant override.
- `GITHUB_APP_PRIVATE_KEY_PATH` pointed at a directory. Set to the actual downloaded PEM; the PEM mode was `664` and is now `600`.
- `.env.local` remains mode `600` and gitignored. These are local operator fixes, not repository changes.

### 15.2 Results

| Check | Result |
|---|---|
| Telegram `getMe` | HTTP 200, `ok=true`, bot `@Padi_Ai_bot` |
| Database | Direct `DATABASE_URL` connection OK |
| NVIDIA key | HTTP 200 on `/v1/models`; 32 models returned, including `nvidia/nemotron-3.5-bolt-e4b` |
| GitHub App | JWT → installation token minted by `gitops.MintInstallationToken`; `GET /installation/repositories` HTTP 200 with `jerryjuche/padi` accessible |
| Real model through OpenCode | Prompt `Reply with exactly: M1 NVIDIA OK` returned exactly that text; SSE included `session.text.ended` and `session.execution.succeeded` |
| Gateway smoke test | 22-second bounded run; log line `gateway: M1 spine ready (model=nvidia/nvidia/nemotron-3.5-bolt-e4b, network=agent-session-net)`; no poller or startup errors; exit `124` is the expected SIGTERM from `timeout` |

The smoke test routed Telegram polling to a local stub that returned empty updates, so the two already-queued owner messages (`/start`, `Hi`) were **not** consumed. The stub logged 38 `getUpdates` calls, confirming the poller loop ran. No session containers were left behind (only the pre-existing `sabify-postgres`). No real task, push or PR was started.

### 15.3 Running it for real

The gateway is a long-running process; it is not started by these tests. To use it from Telegram:

```sh
cd /home/gamp/Desktop/padi
set -a; . ./.env.local; set +a
/tmp/opencode/padi-gateway
```

Then message the bot from the configured owner ID: `/status` or `/help` for help, or `clone <repo> and <prompt>` to start one M1 task. M1 still has no bundle, quarantine, push or PR stage; those land in M3a/M3b.

---

## 16. First real Telegram run — two fixes

### 16.1 Poisoned update cursor

The local M1 loop harness had left `agent.telegram_state.last_update_id` at `1795447659401` (its evidence timestamp). Real Telegram update IDs are around `374300000`, so `getUpdates(offset)` returned nothing and the bot looked silent. Reset once before switching from the local harness to real Telegram:

```sh
set -a; . ./.env.local; set +a
psql "$DATABASE_URL" -c "update agent.telegram_state set last_update_id=0 where id=1"
```

The harness should not write a fake timestamp into the shared cursor; local-loop state belongs in a separate DB or a distinct state row.

### 16.2 HTML parse error on help/hint messages

With the cursor reset, the poller reached the queued messages but every handler failed:

```
telegram handle update 374300388: telegram: sendMessage: HTTP 400:
Bad Request: can't parse entities: Unsupported start tag "repo" at byte offset 26
```

`telegram.Client.SendMessage` always sets `parse_mode=HTML`. The renderer escaped its own text, but `cmd/gateway/main.go` sent the `/help`, `/status` and no-repo hints with literal `<repo>` / `<prompt>`, which Telegram rejects as invalid HTML. All call sites now pass those strings through `html.EscapeString`; `cmd/gateway/main_test.go` guards it with an `httptest` Telegram stub that asserts no raw placeholder reaches the wire.

The poller also now waits 2 seconds after a failed handler before replaying the same update. Cursor semantics remain at-least-once (it still advances only after success), but a permanently bad message can no longer spin the loop at full speed.

### 16.3 Verification

- `gofmt`, `git diff --check`, `go vet`, `go test -race`: green.
- Gateway restarted with real credentials: `M1 spine ready (model=nvidia/nvidia/nemotron-3.5-bolt-e4b, network=agent-session-net)`, no handler errors.
- Cursor advanced `0` → `374300394`; `getUpdates` now reports zero pending updates.
- No new session or task was created by the queued `/start`, `Hi`, `Hello` messages — they only received the escaped help/no-repo hint. The gateway is still running in the background with pid recorded in `/tmp/opencode/gateway-live.pid`.

### 16.4 Silent task failures — `tasks_repo_fkey`

The next queued messages (`Clone GitHub.com/jerryjuche/koder`, then its prompt) created tasks but failed before a container started. The database error was:

```
store: create task: ERROR: insert or update on table "tasks" violates foreign key constraint "tasks_repo_fkey" (SQLSTATE 23503)
```

`agent.tasks.repo` references `agent.repos.full_name`; the bot only inserted into `agent.tasks`. Errors were logged but never returned to the owner, so Telegram looked dead. Fixes:

- `store.EnsureRepo` inserts `agent.repos(full_name, default_branch)` with `on conflict do nothing` before task creation.
- `normalizeRepo` converts `GitHub.com/owner/repo`, HTTPS URLs, `.git` and scp-style forms into the canonical `owner/repo` key.
- `application.reportError` returns `telegram.ReportedError`, so the user gets a short error reply and the poller advances instead of replaying the same update forever.
- User-visible failure messages are escaped and capped; full details stay in the server log.

### 16.5 GitHub token leak and clone URL fix

A synthetic end-to-end probe produced this failure detail:

```
git -c http.extraheader=Authorization: Bearer <installation-token> clone --bare ...
fatal: repository 'jerryjuche/koder' does not exist
```

Two defects were fixed:

1. **Credential exposure.** The installation token was passed as a git `-c` argument, so a git failure echoed it into the error, the server log and `agent.tasks.fail_reason`. The token is short-lived and was scrubbed from all local logs and stored task failures. `gitops.authEnv` now supplies `GIT_CONFIG_KEY_0=http.extraheader` / `GIT_CONFIG_VALUE_0=Authorization: Basic ...` through the process environment; command lines no longer contain the token. GitHub git-over-HTTPS requires `x-access-token:<token>` as Basic auth — a Bearer extraheader works for the REST API but makes git fall back to an anonymous username prompt. `gitexec.Run` and `cmd/gateway` additionally redact bearer, basic, installation-token and JWT-shaped strings before logging or replying.
2. **Wrong clone source.** The canonical DB key `owner/repo` was handed to git as a local path. `gitSource` now maps it to `https://github.com/owner/repo` while keeping `owner/repo` as the repo key, cache name and Telegram label.

M1 still performs no host `git` inside `workspaces/*/repo`; all git calls continue through `gitexec.Run`.

### 16.6 Professional conversational chat

The bot is no longer a one-shot `clone` wrapper. It now keeps one container and one OpenCode session per Telegram chat for up to 30 minutes idle:

- Plain follow-ups reuse the same repo, workspace, container and OpenCode conversation, so the agent can remember earlier findings and answers.
- `Build` (default) implements and verifies changes.
- `Plan` uses OpenCode's built-in primary `plan` agent and a read-only prompt; it investigates, asks clarifying questions and returns a plan instead of editing.
- `Research` uses the same read-only agent with a research directive: read/search, cite evidence, ask when scope is broad, never edit.
- Inline buttons switch mode, request a summary, show status or start a new topic. `/build`, `/plan`, `/research`, `/status`, `/new` and `/help` are also available.
- OpenCode v2.4.0 ignored ad-hoc custom agent definitions in `opencode.base.json`; the working configuration overrides the built-in `build` and `plan` agents, gates `plan` tools (`edit`, `write`, `patch`, `bash` off), and puts mode-specific instructions at the front of the prompt.

No new credentials are required. Telegram buttons use the existing `TELEGRAM_BOT_TOKEN`; conversation and research use the existing NVIDIA key (`PADI_MODEL_API_KEY` → `NVIDIA_API_KEY`). Groq remains switchable via `GROQ_API_KEY` and `config/gateway.yaml` as documented in §14.

### 16.7 Local config note

`config/gateway.yaml` keeps VM-valid `/srv/agent/...` paths. On this development host those directories are not writable, so `.env.local` points `GATEWAY_CONFIG` at `/tmp/opencode/gateway.local.yaml`, which overrides only `cache_dir` and `workspaces_dir` to `/tmp/opencode/padi-agent/...`. The committed config is unchanged.

### 16.8 Two-turn verification with real providers

A local Telegram stub injected two owner messages while the real Supabase DB, GitHub App, Docker image and NVIDIA key were used:

1. `clone jerryjuche/koder and List the top-level files in one short sentence`
2. `Now summarize what you found and ask me one clarifying question.`

Result: both tasks reached `DONE`, both used the **same** `agent.sessions` row (`4fc78801-8d6d-4748-820d-b0c3876431af`), and the session recorded 4 `done`, 5 `status`, 16 `token`, 2 `tool_call` and 1 `tool_result` events. That confirms the follow-up reused the workspace and OpenCode conversation instead of starting a new task. The gateway stopped its session container cleanly at shutdown; only the pre-existing Postgres container remained. The shared Telegram cursor was restored to `374300401`, so no real queued update was consumed by the test.

The earlier Bearer-extraheader attempt failed with git's anonymous username prompt; with `x-access-token:<token>` encoded as Basic auth, `git ls-remote https://github.com/jerryjuche/koder` succeeded and the two-turn clone passed. No credential value appears in command lines, logs, task failures or this record.

### 16.9 Telegram no-op edit fix

Live messages such as `hi`, `hello`, `why is this failing` and `3` were finishing their agent work but then failing with `telegram: editMessageText: HTTP 400 ... message is not modified`. Telegram returns this when the final text and reply markup are identical to the last streamed edit; the gateway was treating that benign no-op as a task failure.

`telegram.Client` now treats Telegram's `message is not modified` response to `editMessageText` as success. The renderer also tracks the last text/keyboard it sent and skips redundant edits. A stub that deliberately returned the exact HTTP 400 for unchanged edits then ran the same two-turn real-provider flow: both tasks reached `DONE` in one session, with no gateway errors. `go vet ./...` and `go test -race -count=1 ./...` are green.

## 17. M2 Router v1 report (`BUILD_SPEC.md` §2.5)

### Done

- Created `cmd/router/main.go`, `config/router.yaml`, and `internal/router/{handler,health,status,models}.go` plus `config`, `route`, `limiter`, `upstream` and `breaker` packages with table-driven `-race` tests.
- Added `scripts/router-hammer.sh` (parallel chat requests, token never printed or written to disk).
- Rewired `config/gateway.yaml`, `cmd/gateway/{config,main}.go`, `internal/gateway/runner/runner.go` and `deploy/session/opencode.base.json` so OpenCode only sees `padi/agent-chat` at `http://router:8090/v1` with `ROUTER_TOKEN`. No `NVIDIA_API_KEY`, `GROQ_API_KEY` or `PADI_MODEL_API_KEY` is injected into session containers.
- Chat style: owner-selected concise collaborator contract added to `AGENTS.target.md`, both agent prompts and `modePrompt`; contextual buttons replaced the mode-only grid; `cleanReply` drops tool-parameter jargon from Telegram replies.
- Defaulted `deploy/session/entrypoint.sh` to `padi/agent-chat` and rebuilt `sandbox-agent-session:latest`.
- Recorded V2/V5 evidence and M2 decisions in `docs/decisions.md`.

### Evidence

```
$ go vet ./... && go test -race -count=1 ./...
ok  	github.com/jerryjuche/padi/cmd/gateway	1.091s
ok  	github.com/jerryjuche/padi/internal/gateway/gitops	1.314s
ok  	github.com/jerryjuche/padi/internal/gateway/opencode	1.058s
ok  	github.com/jerryjuche/padi/internal/gateway/runner	1.289s
ok  	github.com/jerryjuche/padi/internal/gateway/store	1.089s
ok  	github.com/jerryjuche/padi/internal/gateway/telegram	1.753s
ok  	github.com/jerryjuche/padi/internal/keepalive	3.427s
ok  	github.com/jerryjuche/padi/internal/router	1.049s
ok  	github.com/jerryjuche/padi/internal/router/breaker	1.022s
ok  	github.com/jerryjuche/padi/internal/router/config	1.045s
ok  	github.com/jerryjuche/padi/internal/router/limiter	1.023s
ok  	github.com/jerryjuche/padi/internal/router/route	1.024s
ok  	github.com/jerryjuche/padi/internal/router/upstream	1.340s
```

Router startup (real NVIDIA key sourced from `.env.local`, never printed):

```
router: warning: provider deepseek: DEEPSEEK_API_KEY is empty; provider disabled
router: warning: provider groq: GROQ_API_KEY is empty; provider disabled
router: warning: provider openrouter: OPENROUTER_API_KEY is empty; provider disabled
router: warning: alias agent-vision target nvidia has <placeholder>; target disabled
router: warning: alias agent-vision has no enabled targets
router: warning: alias agent-eval has no enabled targets
router ready (listen=:8090, aliases=[agent-chat])
```

`GET /healthz` → `{"ok":true}`. `GET /v1/models` with the router token → only `agent-chat`. `GET /status` → per-provider counters only, no token, key or base URL.

Hammer, first run before the concurrency-queue fix:

```
      1 200
      9 503
router-hammer: at least one request failed
```

`limiter.Sem.Acquire(ctx)` replaced the non-blocking acquire. Same hammer after the fix, on a restarted router:

```
     10 200
router-hammer: ok (10 requests, all 2xx)
```

Real OpenCode task through the router (host-side probe using the rendered session config, `PADI_ROUTER_BASE_URL=http://127.0.0.1:8090/v1`):

```
state=idle after 3s
model {'id': 'agent-chat', 'providerID': 'padi'}
text ROUTER_LIVE_OK
```

A second probe with tool use produced a `glob` tool call (`pattern: *.md`) sent and returned through the provider; the local `glob` run timed out in the temporary fixture, but the session still ended `idle/outcome=succeeded`. Tool-call passthrough was already proven deterministically in the V2 fake-upstream verification.

Live gateway after the switch:

```
gateway: ready (model=padi/agent-chat, network=agent-session-net)
gateway env: ROUTER_TOKEN=<present>
direct key in gateway env? absent
image: sha256:3c3b852973b033b2e96e5c478fb6b0835080027a9c16cdf8c6c82b2f03b3bcbd
```

The first live Telegram task after the switch failed with `ECONNREFUSED`: the session container inherited `http://127.0.0.1:8090/v1`, which is the container itself, not the host router. The Docker bridge gateway is `172.20.0.1`; the local override now uses `http://172.20.0.1:8090/v1`, and a container probe returned `{"ok":true}` from `/healthz` there while `127.0.0.1:8090` refused the connection. Production config keeps the compose service name `http://router:8090/v1`.

A Telegram transport error also leaked the full bot URL, including the bot token, into the gateway log. `internal/gateway/telegram/api.go` now wraps those errors in a `redactedError` that replaces the token with `<redacted>` while preserving `errors.Is`; `TestCallRedactsBotToken` covers both properties.

After the bridge-URL fix, a real owner task sent over Telegram (`Find vulnerability, and report all to me`) completed through the router:

```
2026-11-24 13:01:14.666917+00 | DONE | Find vulnerability, and report all to me
router nvidia: limiter_used 0 -> 3 after the fix
```

### Deviations

- `internal/router/breaker/breaker.go` is a separate package (as in `BUILD_SPEC.md` §3.2 and §7) instead of an unexported type inside `handler.go`.
- `usage` rows are still not written; the spec assigns that to M4.
- The live gateway on this host uses `PADI_ROUTER_BASE_URL=http://172.20.0.1:8090/v1` (Docker bridge gateway) because there is no `router` compose service locally; production containers use `http://router:8090/v1` from config.

### Open items

- External revocation of the M1 NVIDIA throwaway key must be done in the NVIDIA console by the owner. The key is no longer referenced by gateway config or session containers; `/status` and test output contain no secret values.
- M0 VM-only drills remain deferred by owner Option B; M3a/M3b work is still out of scope.
- Groq/OpenRouter/DeepSeek targets stay `<placeholder>` and disabled until keys and live model IDs are verified.

### Next

Awaiting the owner's go-ahead for the next milestone.
