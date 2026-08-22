# Hexeris

A corporate messenger that runs on **your** infrastructure.
One Go binary and PostgreSQL — no Redis, no message broker, no search cluster.

Direct and group chats, files, voice messages, audio and video calls, push
notifications, LDAP/Active Directory, and an admin panel with a sign-in audit
and automatic backups. Conversations can be pinned, muted and archived, and
those settings live on the server, so a phone and a desktop always agree.

> **Status:** running in production and prepared for pilot deployments.
> The honest list of limitations is in
> [ROADMAP.md](ROADMAP.md#3-known-limitations) and is worth reading before
> any decision.

---

## Why this, when Slack and Teams exist

It installs where the cloud is not an option: the data stays on your server,
there are no external dependencies, and the source is fully available to you.

What separates it from Mattermost and Rocket.Chat is the cost of running it.
There is no stack of services here — a binary and Postgres, with everything
else optional. Companies of 50–300 people without a dedicated operations team
feel that immediately.

**Encryption.** Message bodies are encrypted in the database (AES-256-GCM) and
files on disk (AES-256-CTR). The key is held by the server, so this is **not**
end-to-end encryption — by design: a corporate customer needs archiving,
auditing and lawful access by their security team, and end-to-end encryption
stands in the way of all three. Details in [docs/SECURITY.md](docs/SECURITY.md).

---

## Quick start

### Locally, to look around

```bash
git clone <repository> && cd hexeris
docker compose up
```

Open `http://localhost`. The development secrets are baked into the compose
file, so no `.env` is needed.

### Production on your own domain

```bash
cp .env.example .env      # fill in: domain, secrets, TURN
docker compose -f docker-compose.prod.yml up -d
```

Every variable in [`.env.example`](.env.example) comes with the command that
generates it. Full instructions, TLS and coturn are in
[docs/DEPLOY.md](docs/DEPLOY.md).

> ⚠️ `SERVER_ENC_KEY` encrypts messages and files. **A backup without this key
> is useless.** Store it separately from the backups and never change it
> afterwards.

### The first user

Public registration is **disabled by default** — otherwise anyone who found
the address would be inside the company chat. Accounts are created like this:

| Method | When it fits | What to configure |
|---|---|---|
| **Admin panel → Add user** | always; needs no other infrastructure | `ADMIN_KEY` |
| **LDAP / Active Directory** | you have a corporate directory | `LDAP_URL`, build with `-tags ldap` |
| **Google Workspace** | the company runs on Google | `GOOGLE_CLIENT_ID`, `ALLOWED_EMAIL_DOMAINS` |
| Open registration | internal stands, training | `REGISTRATION_ENABLED=true` (plus `REGISTER_MAX_PER_IP`) |

Through the panel the server issues a temporary password and marks the account
as requiring a change: while the administrator knows the password, the account
does not belong to the employee. On first sign-in the user must set their own,
and reloading the page does not bypass that. Directory passwords are changed in
the directory, not here.

---

## Verify after installation

```bash
curl https://<domain>/healthz            # ok
curl https://<domain>/healthz?v=1        # per-component breakdown
```

- [ ] `/healthz` answers `ok`
- [ ] The admin panel opens and `ADMIN_ALLOWED_IPS` is filled in
- [ ] The first user exists; sign-in and the password change both work
- [ ] A message travels between two browser tabs
- [ ] A file uploads and opens
- [ ] A call connects (this exercises TURN, the most common point of failure)
- [ ] The PWA installs on a phone and receives a push notification
- [ ] `DB_BACKUP_ENABLED=true` and the panel shows a successful backup run
- [ ] A restore has been rehearsed: `scripts/restore-drill.sh`

The last item is not a formality. A backup nobody has ever restored is not a
backup.

---

## What is inside

```
server/     Go backend: HTTP and WebSocket, 43 endpoints
web/        Client: vanilla JS, 25 modules, PWA. No build step — files are served as-is
web/fonts/  Self-hosted fonts: the client never calls out and works without internet
docs/       Architecture, security, deployment, backups, recovery
docs/admin-panel/  The admin panel (static) plus a ready reverse-proxy config
scripts/    Load test, chaos drill, restore drill, benchmarks
deploy/     nginx, coturn
```

**Stack:** Go 1.25 · PostgreSQL · gorilla/websocket · WebRTC with coturn ·
Web Push (VAPID). Six direct dependencies.

**Delivery guarantees.** A persistent outbox, idempotency by message `id` and a
monotonic `seq` cursor. A missing ACK is not a loss: that is verified by tests
rather than asserted. The `delivered` flag means "handed to the queue", and the
protection against loss is the cursor. See
[docs/ARCHITECTURE.md §4.1](docs/ARCHITECTURE.md).

---

## Development

```bash
cd server
go build ./... && go vet ./...
go test -race ./...                     # some tests skip without a database

# Full run against a real PostgreSQL:
export TEST_DATABASE_URL='postgres://user@localhost:5432/hexeris_test?sslmode=disable'
go test -race ./...                     # 45 tests
```

CI runs both builds (with and without LDAP), `gofmt`, `vet`, `go test -race`
against a live Postgres, and `node --check` over every JS file.

There is nothing to build on the client: `web/` is served as static files.

---

## Performance

Measured, not estimated. The three sets below were taken under **different
conditions** and are not comparable with one another; the full detail is in
[ROADMAP.md §4](ROADMAP.md#4-evidence).

| Conditions | Result |
|---|---|
| Local VM, 8 vCPU, 1000 concurrent clients | 1000/1000 connections, 20000/20000 messages, **zero loss**, connect p50 **7 ms** |
| Constrained stand (one core each for app and database), after optimisation | **14 490 msg/s**, ACK p50 **212 ms** (from 2 430 msg/s and 3 742 ms) |
| Production host over the internet, 200 clients | 200/200, zero loss, a 200-client reconnect storm in 2.94 s |

Restore time from a backup is **~6 seconds**, measured by drill.

---

## Documentation

| File | About |
|---|---|
| [docs/DEPLOY.md](docs/DEPLOY.md) | Installation, production, the admin panel host |
| [docs/ARCHITECTURE.md](docs/ARCHITECTURE.md) | How it works, delivery guarantees, indexes, database failure |
| [docs/SECURITY.md](docs/SECURITY.md) | Threat model, encryption, limits, what was tested |
| [docs/BACKUP.md](docs/BACKUP.md) · [docs/DISASTER-RECOVERY.md](docs/DISASTER-RECOVERY.md) | Backups and recovery |
| [docs/RETENTION.md](docs/RETENTION.md) | Retention periods, including personal data |
| [ROADMAP.md](ROADMAP.md) | Maturity, measurements and the **honest limitations** |

---

## Limitations worth knowing up front

- A single application instance is a point of failure; there is no horizontal
  scaling
- No native mobile applications, only the PWA
- No SSO (SAML/OIDC); role-based access is limited to roles inside groups
- No external penetration test has been performed
- File links work for any authenticated user who knows the URL (the name is
  128 bits from `crypto/rand`)

The full list, with measurements and explanations, is in
[ROADMAP.md §3](ROADMAP.md#3-known-limitations).
