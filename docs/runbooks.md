# Operations & Recovery Runbooks

Reference: `README.md` §11.8, §11.9, §14 and `BUILD_SPEC.md` §2.4 (M0), §3.7[cite: 2, 4].

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



