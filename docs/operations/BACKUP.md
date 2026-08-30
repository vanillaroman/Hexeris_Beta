# Backups and restore

A backup covers **both** sources of data:

| What | File | How |
|---|---|---|
| Database (users, messages, groups, reactions…) | `hexeris-<stamp>.sql.gz` | `pg_dump \| gzip` |
| Attachments (photos, video, voice messages, documents) | `hexeris-files-<stamp>.tar.gz` | `tar + gzip` of `UPLOAD_DIR` |

Both files share **one timestamp** (`<stamp>` = `YYYYmmdd-HHMMSS` in UTC) and
form a single consistent set. Files are written `0600` in a `0700` directory.

> ⚠️ **The critical part is `SERVER_ENC_KEY`.** Message bodies in the database
> and files on disk are encrypted with it at rest. **A backup without the key
> is useless.** Keep `SERVER_ENC_KEY` (and `JWT_SECRET`) somewhere safe and
> separate from the backups. Restoring onto a new server requires the **same**
> key.
>
> This is not left to the administrator's memory: the server records a
> fingerprint of the key and **refuses to start** with a different one while
> the data is still intact — see `docs/operations/ENC-KEY-GUARD.md`.

## Requirements

`pg_dump` (package `postgresql-client`) and `tar`. Both ship in the
application image; on a host install check with:

```bash
pg_dump --version && tar --version
```

## A one-off backup, to confirm it works

```bash
docker compose -f docker-compose.prod.yml exec app hexeris backup
docker compose -f docker-compose.prod.yml exec app ls -lh /data/backups
```

The `backup` subcommand takes one set and exits; it never starts the server.

## Enabling automatic backups

Disabled by default, so they cannot fill a small disk on their own. Enable in
`.env`:

```ini
DB_BACKUP_ENABLED=true
DB_BACKUP_INTERVAL_HOURS=24
DB_BACKUP_KEEP=7
```

```bash
docker compose -f docker-compose.prod.yml up -d app
docker compose -f docker-compose.prod.yml logs app | grep -i backup
# DB backup scheduler ENABLED — every 24h into /data/backups
```

`DB_BACKUP_KEEP=7` keeps the seven most recent sets of each type and removes
older ones.

> The scheduler ticks from process start. For a specific time of day (03:00,
> say), leave the scheduler off and invoke `hexeris backup` from cron.

## 🔴 Restore

```bash
# 0. Stop the application so nothing writes during the restore.
docker compose -f docker-compose.prod.yml stop app

# 1. Database. The dump overwrites current data — make sure the target is right.
gunzip -c /path/to/hexeris-<stamp>.sql.gz | psql "$DATABASE_URL"

# 2. Files. Clear the target directory and unpack the archive into it
#    (paths inside the archive are relative: ./<file>).
rm -rf "$UPLOAD_DIR"/* && tar -xzf /path/to/hexeris-files-<stamp>.tar.gz -C "$UPLOAD_DIR"

# 3. Confirm SERVER_ENC_KEY and JWT_SECRET match the values used at backup time.

# 4. Start again.
docker compose -f docker-compose.prod.yml up -d app
curl -fsS https://<domain>/healthz && echo " OK"
```

## Verifying a backup without touching production

A backup with no rehearsed restore is not a backup. Verify into a separate
database:

```bash
createdb hexeris_restore_test
gunzip -c hexeris-<stamp>.sql.gz | psql "postgres://<user>@localhost/hexeris_restore_test"
psql "postgres://<user>@localhost/hexeris_restore_test" -c "SELECT count(*) FROM users; SELECT count(*) FROM messages;"
dropdb hexeris_restore_test
```

Non-zero counts mean the dump is valid.

### Automatically: `restore-drill.sh`, which also measures RTO and RPO

One command restores the **latest** set into a scratch database and directory
(production is untouched), checks usability through `hexeris verify-restore` —
non-zero counts plus **decryption of a sample of message bodies with the
current `SERVER_ENC_KEY`**, which proves the key matches the backup rather
than merely that psql did not fail — measures **RTO** (restore time) and
**RPO** (the age of the latest backup), then cleans everything up.

```bash
export SERVER_ENC_KEY="<the same key used at backup time>"
DB_BACKUP_DIR=/var/backups/hexeris PG_CONN="postgres://hexeris@localhost" \
  HEXERIS_BIN=/path/to/hexeris \
  bash scripts/restore-drill.sh
```

The output gives RTO and RPO figures ready to record. `verify-restore` is a
read-only subcommand of the binary: it starts no server and touches nothing in
production.

## Off-site copies

Backups on the same machine do not survive the loss of that machine. Copy each
set elsewhere:

```bash
# Automatically, through DB_BACKUP_OFFSITE_CMD
# (see docs/operations/DISASTER-RECOVERY.md §1.1):
#   DB_BACKUP_OFFSITE_CMD=rclone copy "$BACKUP_FILE" remote:hexeris/
# By hand — rsync, scp or rclone to S3 or any other off-site store:
rsync -az /var/backups/hexeris/ user@backup-host:/backups/hexeris/
```

## Checklist

- [ ] `pg_dump` and `tar` are available
- [ ] `hexeris backup` creates both files
- [ ] `DB_BACKUP_ENABLED=true` in production, with "scheduler ENABLED" in the log
- [ ] `SERVER_ENC_KEY` and `JWT_SECRET` are stored separately and securely
- [ ] A restore has been rehearsed in a scratch database (non-zero counts)
- [ ] Backups are copied off-site
