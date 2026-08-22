# Retention: automatic removal of old data

**Disabled by default.** This is a corporate messenger: conversations that
silently evaporate are worse than a database that grew by a couple of
gigabytes. Retention only runs when it is switched on explicitly.

## What is removed

| Data | Variable | Default once enabled |
|---|---|---|
| Messages older than N days | `RETENTION_MESSAGE_DAYS` | 365 |
| Reactions on those messages | — (always with the message) | — |
| Attachments of those messages | — (always with the message) | — |
| `admin_audit` rows older than N days | `RETENTION_AUDIT_DAYS` | 180 |

`0` in any variable means "leave this category alone".

**Never removed:** users, groups, group membership, push subscriptions.

The sign-in audit is pruned on its own schedule, independent of this setting
(`LOGIN_AUDIT_KEEP_DAYS`, 90 days by default), because a username with an IP
address is personal data and should not accumulate indefinitely even where
content retention is off.

## Enabling it

With Docker Compose, set the variables in `.env`:

```ini
RETENTION_ENABLED=true
RETENTION_MESSAGE_DAYS=365
RETENTION_AUDIT_DAYS=180
RETENTION_INTERVAL_HOURS=24
```

Then restart the application container and check the log:

```bash
docker compose -f docker-compose.prod.yml up -d app
docker compose -f docker-compose.prod.yml logs app | grep RETENTION
# RETENTION enabled: messages older than 365d, audit older than 180d, every 24h
```

A pass runs every `RETENTION_INTERVAL_HOURS`, the first one an interval after
startup rather than immediately. Each pass logs its result:
`retention: removed 1240 messages, 88 audit rows, 37 files`.

## Before the first run

The operation is **irreversible**. Take a backup and confirm that it restores —
see [BACKUP.md](BACKUP.md):

```bash
docker compose -f docker-compose.prod.yml exec app hexeris backup
```

A safe order on a live deployment: set a deliberately long period first (for
example `RETENTION_MESSAGE_DAYS=3650`), confirm from the log that a pass ran
and removed nothing unexpected, and only then lower it to the target value.

## What the implementation takes care of

- Attachment paths are collected **before** the `DELETE`; otherwise the files
  would stay on disk forever with nothing referencing them.
- Files are removed **after** the rows are gone, so a database error cannot
  leave history full of broken attachments.
- Reactions are pruned explicitly: the `reactions` table has no foreign key to
  `messages` and would otherwise outlive them.
- File names go through `filepath.Base` with `..` rejected, so a path from the
  database can never take a deletion outside `UPLOAD_DIR`.

Covered by the integration test `TestIntegrationRetention`: an old message and
its reaction are removed, a recent one survives, and `0` is a no-op.
