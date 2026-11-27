# Operations & Recovery Runbooks

Reference: `README.md` §11.8, §11.9, §14 and `BUILD_SPEC.md` §2.4 (M0), §3.7[cite: 2, 4]; local implementation evidence in `update.md` §§23–29.

## 1. Growing the OCI Boot Volume (50 GB to ~100 GB)

1. In the Oracle Cloud Console: **Compute -> Instances -> [VM] -> Boot Volume -> Edit**, increase size from `50 GB` to `100 GB` and save.
2. SSH into the VM and rescan the block device:
   ```bash
   sudo dd iflag=direct if=/dev/oracleoci/oraclevda of=/dev/null count=1
   echo "1" | sudo tee /sys/class/block/sda/device/rescan

```
3. Grow the partition and filesystem:
```bash
sudo growpart /dev/sda 1
sudo resize2fs /dev/sda1
df -h /

```



## 2. VM Reclaimed or Reinstalled (Full Rebuild from Backup)

Supabase Postgres holds all authoritative state; the VM holds only disposable caches/workspaces and manually re-entered secrets.

1. Provision a fresh `VM.Standard.A1.Flex` Ubuntu 24.04 ARM64 instance (2 OCPU, 12 GB RAM) and attach the same VCN security list (TCP 22, 80, 443 inbound).

2. Grow the boot volume to ~100 GB (Runbook 1).

3. Clone this repository to `/srv/agent/src` and run the idempotent rebuild script:
```bash
sudo bash /srv/agent/src/scripts/rebuild.sh

```

4. Re-create `/srv/agent/secrets/` (`chmod 700` dir, `chmod 600` files) from your password manager (`gateway.env`, `router.env`, `egress.env`, `github-app.pem`, `key_seal.key`, `preview_signing.key`).

5. Start services and verify end-to-end health:
```bash
sudo systemctl enable --now keepalive.service agent-nightly.timer
bash /srv/agent/src/scripts/smoke.sh

```



## 3. Restoring Database from Nightly Backup

1. Download the desired `agent-YYYYMMDDTHHMMSSZ.sql.gz` dump from Object Storage (or `/srv/agent/backups/`).

2. Test against a throwaway container first using `scripts/restore-test.sh`.

3. To restore into Supabase Postgres (caution: overwrites schema `agent`):
```bash
gunzip -c agent-YYYYMMDDTHHMMSSZ.sql.gz | psql "$DATABASE_URL"

```

## 4. Silent or Stalled Task (No Model Output)

Use when a task stays `EXECUTING` with no visible progress. Evidence: `update.md` §28–§29.

1. Send `/options` (or tap **Options** on `/status`). The rolling trace shows phases `QUEUED → CONTRACT → CLONE → SANDBOX → EXECUTE → VERIFY → PUBLISH → DONE`; for active work expect `Thinking → Agent planning → Agent executing`. A finished/failed task rebuilds the last 18 events from `agent.events`, so the trace survives container closure.
2. Look for `♥ alive` from the 15 s activity probe, the retry count, and the last transport error. No model output plus retries is a transport failure, not a slow model.
3. Check the failure reason: after 6 retries the gateway fails the task instead of waiting. In `update.md` §29, stale `EXECUTING` tasks were marked `FAILED` with reason `model unreachable: router DNS alias missing`; remove any orphan container before retrying with a fresh contract.
4. Verify the host-bridge aliases: `PADI_ROUTER_HOST_IP=172.20.0.1` in the gateway environment and Docker `ExtraHosts` `router:172.20.0.1` on the session container. If this host has no `egress-proxy` listener, `PADI_PROXY_URL` must be empty; a leftover `HTTP_PROXY=http://egress-proxy:3128` makes the model client fail DNS before it reaches the router. Check the network is still internal:
   ```bash
   docker network inspect agent-session-net --format 'internal={{.Internal}} gateway={{range .IPAM.Config}}{{.Gateway}}{{end}}'
   docker inspect <container> --format '{{json .HostConfig.ExtraHosts}}'
   ```
5. Check router health from a container on `agent-session-net` (`http://router:8090/healthz`). Compose deployments have a real `router` container and use Docker DNS; the `ExtraHosts` mapping is only for the local host-bridge runtime. Do not open egress or set `internal=false` to work around a DNS miss.
