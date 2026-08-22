# Disaster recovery

A recovery procedure written so that someone who did not write this code can
follow it: no "see the sources", and no assumption that the operator remembers
how the system is built.

The mechanics of backups — what is written where, and retention — are in
`BACKUP.md`. This document is **what to do once something is broken**.

## 0. The short answer for due diligence

| Question | Answer | Evidence |
|---|---|---|
| RTO (data restore) | **~6 s** for a production-scale dump (1385 messages plus files) | `scripts/restore-drill.sh`, recorded in `ROADMAP.md` |
| RTO (total loss of the host) | **⬜ not measured** — bounded by provisioning a new machine rather than by the data | — |
| RPO | ≤ `DB_BACKUP_INTERVAL_HOURS` (**24 h** by default) | configuration |
| Is the backup genuinely restorable? | Yes, verified end to end: the dump loads into a clean database and bodies **decrypt** with the current key, 100/100 | `hexeris verify-restore` |
| A copy off the machine | 🟡 **only when `DB_BACKUP_OFFSITE_CMD` is configured** (see §1) | — |

> **The main risk, stated plainly.** Without an off-site copy the backups sit
> on the same disk as production. A disk failure, a deleted instance or
> ransomware takes the data and its copies **together**, and no RTO helps. This
> is the first thing to configure before a pilot.

## 1. What to set up before an incident

### 1.1 A copy off the machine (mandatory)

Set `DB_BACKUP_OFFSITE_CMD` and every fresh backup is copied away
automatically. The command receives its paths through the environment rather
than string substitution, so spaces and quotes in paths are safe:

| Variable | Contents |
|---|---|
| `BACKUP_FILE` | the fresh database dump (`hexeris-<timestamp>.sql.gz`) |
| `BACKUP_FILES_ARCHIVE` | the attachment archive with the same timestamp |
| `BACKUP_DIR` | the backup directory as a whole |

Any transport works: `rclone`, `rsync -az … user@host:/backups/`, `aws s3 cp`.
The upload is bounded at 30 minutes, and an off-site failure does **not**
invalidate the local backup already taken — but it is logged and surfaced in
the metrics.

### 1.2 The encryption key, stored separately from the backup

⚠️ **`SERVER_ENC_KEY` is not in the backup and cannot be derived from it.**
Message bodies (AES-256-GCM) and files (AES-256-CTR) are unreadable without
it. A copy of the key must live **somewhere other than** the dumps: a secrets
manager, a safe, a sealed envelope. A backup without the key is entirely
useless.

The same applies to `JWT_SECRET`, though losing it merely invalidates every
session and signs all users out.

### 1.3 Confirming that backups are actually running

`/admin/metrics` → the `backup` block. What to watch for:

- `age_hours` noticeably larger than `DB_BACKUP_INTERVAL_HOURS` → the
  scheduler is not running;
- `last_ok: false` with `last_error` → look at the reason (usually `pg_dump`
  is missing or the database is unreachable);
- `offsite_ok: false` → a backup exists, but **only on this machine**;
- `"offsite": "not_configured"` → §1.1 has not been done.

> The scheduler takes a **catch-up** backup at startup when the newest file is
> older than the interval. Without it a service restarted more often than the
> interval — frequent deployments — would never back up at all, and that would
> be discovered exactly when it was needed.

### 1.4 Alerting on silent failures

`/healthz` works in two modes, and the status codes are unchanged, so existing
uptime monitoring keeps working:

| Request | Response |
|---|---|
| `GET /healthz` | `ok` (200) or `db unreachable` (503) |
| `GET /healthz?v=1` | JSON: `status` = `ok` / `degraded` / `down` plus a per-component breakdown |
| either | the header `X-Health-Status: ok\|degraded\|down` |

**200 is returned for `degraded` too**, deliberately. "The backup is stale"
does not mean "the service is down", and an external monitor should not page
anyone at night over it: otherwise the first degraded state turns the uptime
chart into a false outage, and alerts like that stop being read.

`degraded` is raised when the service answers while something is quietly
broken: a backup failed, went stale (older than two intervals) or never
reached off-site storage; a writer panicked; saves are timing out.

**How to configure an alert** in a monitor that can match a substring in the
response (any keyword-based uptime service):

- URL: `https://<domain>/healthz?v=1`
- condition: the response **does not contain** `"status":"ok"` → alert.

One check then covers both a total outage and a silent one. The component
checks are pinned by tests (`server/health_test.go`).

## 2. Scenario A: data is damaged, the machine is alive

Typical causes: an accidental bulk delete, a corrupted table, a bad migration.

```bash
# 1. Stop the application so it cannot write over the restore
docker compose -f docker-compose.prod.yml stop app

# 2. Check the backup BEFORE touching production (scratch database, production unchanged)
createdb hexeris_restore_check
gunzip -c /var/backups/hexeris/hexeris-<timestamp>.sql.gz | psql -d hexeris_restore_check

# 3. Confirm bodies decrypt with the CURRENT key
SERVER_ENC_KEY="$SERVER_ENC_KEY" ./hexeris verify-restore \
    'postgres://…/hexeris_restore_check' /var/backups/hexeris/files-<timestamp>

# 4. Only now restore into the live database
dropdb hexeris && createdb hexeris -O hexeris
gunzip -c /var/backups/hexeris/hexeris-<timestamp>.sql.gz | psql -d hexeris

# 5. Attachments
tar -xzf /var/backups/hexeris/hexeris-files-<timestamp>.tar.gz -C "$UPLOAD_DIR"

docker compose -f docker-compose.prod.yml up -d app
curl -sf https://<domain>/healthz && echo OK
```

Step 3 is not a formality: it answers "is this even the right key?" before
production is dropped. In the recorded drill decryption succeeded 100/100.

## 3. Scenario B: the machine is lost entirely

The order matters: data first, traffic last.

1. **Provision a host** with Docker, or with PostgreSQL, a reverse proxy, Go
   and coturn for a native install.
2. **Restore the secrets** from storage: `SERVER_ENC_KEY`, `JWT_SECRET`,
   `DATABASE_URL`, `ADMIN_KEY`. Without the first one there is no point
   continuing.
3. **Fetch the backup** from off-site storage.
4. **Restore the database and files** — steps 2–5 of §2.
5. **Build and start** the application and issue a TLS certificate.
6. **Switch DNS** to the new address. Until this step no user traffic arrives,
   so everything can be verified calmly.
7. Verify: `/healthz` returns 200, sign-in works, a message sends, a file opens.

**What is lost:** messages received after the last backup
(≤ `DB_BACKUP_INTERVAL_HOURS`). There is nowhere to recover them from — client
caches in local storage are not a source of truth and hold only the last 500
messages per conversation.

## 4. Scenario C: `SERVER_ENC_KEY` is compromised

There is one key per installation and it is **not rotated automatically**:

1. Generate a new key while **keeping the old one** — it is required to decrypt
   what is already stored.
2. Re-encrypt the data offline: read with the old key, write with the new one.
   There is **no ready tool** in the repository — ⬜ on the roadmap.
3. Until re-encryption is done the old key remains necessary: discarding it
   loses all history and files.

> Stated honestly: scenario C currently requires manual work and downtime. If a
> customer needs key rotation, that is a project rather than a setting.

## 5. Regular rehearsal (otherwise all of the above is theory)

A backup that has never been restored is not a backup.

```bash
# Restores into a scratch environment; production is untouched
bash scripts/restore-drill.sh
```

The script restores the latest dump into a temporary database and directory,
then `hexeris verify-restore` confirms that bodies **decrypt**. Run it monthly
and after every change to the backup format. The latest result is recorded in
`ROADMAP.md`.

## 6. What remains unmeasured

- **RTO for a total host loss is not measured.** Only the data import was
  timed (~6 s); provisioning a machine, installing packages, building and
  issuing a certificate depend on the provider and were never put on a
  stopwatch.
- **Off-site restore has not been rehearsed** end to end, only local restore.
- **`SERVER_ENC_KEY` rotation** has neither a procedure nor a tool.
- **A single machine.** Everything above is about recovery rather than high
  availability: there is no warm standby, and downtime during the procedure is
  unavoidable.
