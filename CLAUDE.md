# Photos repo

Self-hosted photo library: Immich + syncthing + rclone-to-S3 backup, run via `docker-compose.yml`. Full pipeline and setup steps are documented in `README.md` — read that first for the "why". This file is a quick map so future sessions don't need to re-explore the repo from scratch.

## Layout
- `docker-compose.yml` — all services: `immich-server`, `immich-machine-learning`, `redis`, `database`, `syncthing` (container-side, syncs phone temp folders into per-person subfolders of `$STORAGE_DIR`, e.g. `Oleg/oleg-pixel`, `Kate/kate-samsung/Camera`), `rclone-s3-sync` (scheduled S3 backup, see below), `caddy` (HTTPS reverse proxy in front of `immich-server`, see below).
- `docker/rclone-s3-sync/` — custom-built image (Python + rclone) for the scheduled S3 sync. `sync.py` is a loop, not cron: it persists the last-sync timestamp to `/state/<SYNC_NAME>/last_sync.txt` (named volume `rclone_sync_state`) and syncs immediately if the configured `SYNC_INTERVAL` has elapsed — including catch-up after the container was stopped past a scheduled run. Defaults to dry-run. Storage layout is flat: all of `$STORAGE_DIR` is mounted at `/data` and copied to the **bucket root**, so a local path `$STORAGE_DIR/<rel>` maps to `$RCLONE_REMOTE:$AWS_BUCKET/<rel>`. `SYNC_NAME`/`RCLONE_SYNC_NAME` (default `Photos`) now only names the state file. Older backups live under the `Photos/` prefix in the bucket from the previous layout and are pending manual deletion.
- `docker/caddy/Caddyfile` — single site block reverse-proxying `$IMMICH_DOMAIN` (env var, e.g. `immich.funduckdev.ddns.net`) to `immich-server:2283`. Caddy auto-obtains/renews a Let's Encrypt cert for that domain, so it needs the domain's DNS to resolve to this host and ports 80/443 forwarded to it (80 for the ACME HTTP-01 challenge, 443 for TLS). `immich-server` no longer publishes its port to the host directly (`expose: 2283`, internal-only) — it's reached only via Caddy now. Cert/account state persists in the `caddy_data`/`caddy_config` named volumes.
- `scripts/` — one-off/manual shell scripts and a Python dedup tool:
  - `aws_setup.sh` — reference commands (not a script to run as a whole) to create the S3 bucket, IAM user/policy, budget alert.
  - `backup_photos_to_s3.sh` — the manual equivalent of what `rclone-s3-sync` now runs on a schedule.
  - `backup_immich.sh` — stops Postgres, backs up Immich assets + DB to `$IMMICH_BACKUP_DIR` (from `scripts/.env`), restarts. Manual, not scheduled.
  - `duplicate_resolver.py` — calls Immich API to find/delete duplicate assets. `--dry-run`/`--execute`.
  - `delete_copied_files.sh <src> <dst>` — deletes files from a host syncthing temp folder that already have an identical copy (size + `cmp`) at the same relative path under `$STORAGE_DIR`; only files older than `--min-age-days` (default 30).
  - `delete_files_local.sh` / `delete_files_s3.sh <list>` — delete a list of absolute `$STORAGE_DIR/...` paths locally / from the bucket root (S3 via `docker exec` into `rclone_s3_sync`; needs the temporary delete policy from `aws_setup.sh`).
  - All of these default to `--dry-run`; pass `--execute` to really delete.
- `rclone-archive-policy.json` — IAM policy JSON used by `aws_setup.sh`.

There's no standalone `rclone config`/verification script anymore — that was `scripts/rclone_setup.sh`, removed once `rclone-s3-sync` started generating its own `rclone.conf` and running an automatic dry-run. For ad hoc `rclone` CLI debugging against the same credentials, `docker exec` into the running container and reuse its generated config: `docker exec rclone_s3_sync rclone lsd $RCLONE_REMOTE:$AWS_BUCKET --config /tmp/rclone.conf`.

## Config: two separate `.env` files
- Root `.env` (from `.env.example`) — read by `docker-compose.yml` (via compose's auto-loaded root `.env`). Has Immich vars (`DB_PASSWORD`, `IMMICH_DEDUP_API_KEY`, `IMMICH_DIR`, `STORAGE_DIR`) plus, for `rclone-s3-sync`, `RCLONE_REMOTE`, `RCLONE_ACCESS_KEY`, `RCLONE_SECRET_ACCESS_KEY`, `AWS_BUCKET`, `AWS_REGION`, and optional `RCLONE_SYNC_INTERVAL` / `RCLONE_SYNC_DRY_RUN` overrides, plus `IMMICH_DOMAIN` for `caddy`.
- `scripts/.env` (from `scripts/.env.example`) — `export`-prefixed, meant to be `source`d by the shell scripts. Has the same rclone/AWS vars plus phone/storage paths (`PHONE_TMP_DIR`, `PHONE_STORAGE_DIR`, etc.).
- These two files **duplicate** some vars (`STORAGE_DIR`, the rclone/AWS vars) on purpose: docker-compose only auto-loads the root `.env`, and it can't parse `scripts/.env`'s `export ` prefixes, so there's no single source of truth to point compose at. Keep both in sync by hand when rotating credentials or changing the bucket/remote name.
- Both real `.env` files contain live secrets and are gitignored — never commit them, and double-check before staging anything under `scripts/` or repo root.

## Conventions
- Compose services: `container_name` set explicitly, `user: "503:20"`, `security_opt: [no-new-privileges:true]`, `cap_drop: [NET_RAW]`, `restart: unless-stopped`.
- rclone flags standardized across `backup_photos_to_s3.sh` and `rclone-s3-sync`: `--s3-storage-class DEEP_ARCHIVE --size-only --fast-list --transfers 8 --checkers 16 --progress`, plus `--exclude` patterns for macOS junk (`._*`, `.DS_Store`, `.Spotlight-V100/**`, `.Trashes/**`, `.fseventsd/**`, `.TemporaryItems/**`) since the synced folder gets touched from a Mac.
- New destructive/one-way scripts (S3 uploads, deletes) default to a dry-run mode, flipped on explicitly once verified.
