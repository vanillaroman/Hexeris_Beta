# Hexeris — project brief

One file that explains what this is, how it is built and where the boundaries
run. Written for a person and for an AI assistant seeing the project for the
first time: after this you can read the code without reassembling the context
from fragments.

---

## 1. What it is

**A corporate messenger deployed on the customer's own infrastructure.** One Go
binary and PostgreSQL — no Redis, no message brokers, no search cluster.

The niche: companies of 50–300 people for whom cloud is ruled out (policy, a
closed network, data-residency requirements) and who have no dedicated DevOps.
The difference from Mattermost and Rocket.Chat is not in features but in the
cost of operation: there, a stack of several services; here, a binary plus a
database.

**Status:** running in production, being prepared for pilot deployments. The
honest list of limitations is [ROADMAP.md §3](../../ROADMAP.md), and it is worth
reading before making decisions.

---

## 2. The key architectural decision: encryption without E2EE

Message bodies are encrypted in the database (AES-256-GCM) and files on disk
(AES-256-CTR). **The key is held by the server. This is NOT end-to-end
encryption, and that is deliberate.**

The reason is not implementation difficulty. A corporate customer needs
archiving, auditing and lawful access by their security team — end-to-end
encryption stands directly in the way. A product that promises "we cannot read
your correspondence" does not get approved where being able to is an
obligation.

The remaining decisions follow from this: the sign-in log, an admin panel that
can block users, and "delete chat" as a personal visibility boundary rather
than data deletion. Details in [SECURITY.md](../security/SECURITY.md).

---

## 3. Stack and layout

```
server/            Go backend: HTTP + WebSocket
web/               Client: vanilla JS, PWA. There is NO build — files are served as they are
web/fonts/         Fonts kept locally: the client never calls Google and works without internet
docs/              Architecture, security, deployment, backups, recovery, retention
docs/admin-panel/  Admin panel (static) plus a ready nginx config
scripts/           Load test, chaos drill, restore drill, benchmarks, fonts
deploy/            nginx, coturn
tests/ui/          Browser test suites against a live instance
```

**Go 1.25 · PostgreSQL · gorilla/websocket · WebRTC + coturn · Web Push
(VAPID).** Six direct dependencies.

The database schema is created idempotently from Go (`CREATE TABLE IF NOT
EXISTS` plus `ALTER … ADD COLUMN IF NOT EXISTS`); there are no separate
migration files.

---

## 4. Functionality

| Area | What exists |
|---|---|
| Messaging | direct and group chats, reactions, replies, editing, deletion, forwarding |
| State | read receipts, typing, presence (online plus a manual status) |
| Attachments | files of any type, voice messages, link previews, a per-chat attachments panel |
| Calls | WebRTC audio and video 1:1 plus TURN, network test. Group calls are not implemented |
| Chat list | pin, mute, archive, "delete for me" — a context menu on right click and long press |
| Notifications | Web Push (VAPID), browser notifications, PWA |
| Users | creation by an administrator with a forced password change, two-factor authentication (TOTP), LDAP/AD, OIDC single sign-on, Google Workspace, open registration (disabled by default) |
| Administration | metrics, sign-in log, action audit, group management, CSV exports, message export, retention |

---

## 5. Delivery guarantees

This is the most worked-through part and the project's main technical argument.

- **A persistent outbox.** The message goes to the database first, the socket
  second.
- **Idempotency by `id`.** A repeat from the client creates no duplicate.
- **A monotonic `seq` cursor.** The client catches up on what it missed with
  `/history?since=<seq>`; reactions have their own `rseq`.
- **A missing ACK is not a loss.** The `delivered` flag means "handed to the
  queue"; protection against loss is the cursor. This is verified by tests
  rather than asserted.

Details and measurements in [ARCHITECTURE.md §4.1](ARCHITECTURE.md).

---

## 6. Measured performance

The three sets were produced **under different conditions** and are not
comparable with each other.

| Conditions | Result |
|---|---|
| Local VM, 8 vCPU, 1000 concurrent clients | 1000/1000 connections, 20 000/20 000 messages, zero loss, connect p50 **7 ms** |
| Constrained rig (one core for app and database), after the optimisations | **14 490 msg/s**, ACK p50 **212 ms** (was 2 430 msg/s and 3 742 ms) |
| Production VPS, load over the internet, 200 clients | 200/200, zero loss, reconnect storm of 200 in 2.94 s |

RTO for a restore from backup is **~6 seconds** (measured by a drill, not
estimated).

---

## 7. Deployment topology

Two machines, and they are **easy to confuse** — most of the operational
mistakes in this project have been about exactly that.

```
AN EMPLOYEE USES THE MESSENGER
  phone/browser ──► nginx on the APPLICATION HOST ──► Go application ──► PostgreSQL
                    ↑ X-Forwarded-For belongs here, otherwise every entry in the
                      sign-in log reads 127.0.0.1

AN OPERATOR OPENS THE ADMIN PANEL
  browser ──► nginx on the admin host ──► nginx on the application host ──► Go
              ↑ basic auth plus injection of X-Admin-Key;
                XFF from here is deliberately NOT forwarded
```

**Why the panel goes through a proxy.** It addresses the relative path
`/admin-api/…` and knows neither the messenger's address nor `ADMIN_KEY`. The
key lives only in an nginx config owned by root and never reaches a browser.

**Why XFF from the admin host is suppressed.** The application takes the client
address from that header. Forward it from the operator's browser and anyone who
has passed basic auth can supply an arbitrary address and walk past
`ADMIN_ALLOWED_IPS`.

To diagnose addresses: `GET /healthz?v=1`, the `client` block — it shows which
address the server sees and why that one.

### Hosts

| Address | What | Where |
|---|---|---|
| `chat.example.com` | the working instance | application host |
| `admin.example.com` | status page | admin host |
| `admin.example.com/admin` | admin panel | admin host |

Both roles can live on one machine for a pilot. Separating them is what makes
the double boundary in section 9 meaningful.

---

## 8. Security: what is closed

- **JWT + bcrypt.** A password change revokes previous tokens
  (`forced_logout_at`).
- **Two-factor authentication (TOTP)** on top of a local password, enabled by
  each employee; the secret is stored encrypted
  ([TWO-FACTOR.md](../security/TWO-FACTOR.md)).
- **Encryption at rest** for bodies and files; the `SERVER_ENC_KEY` — **without
  it a backup is useless**, so keep it apart from the backups.
- **CSP without `script-src 'unsafe-inline'`.** There are no inline handlers in
  the markup: the binding from a name to a function lives in the registry in
  `web/js/events.js`, and the markup carries only a declaration
  (`data-act="…"`). A `<script>` injected through XSS is simply refused
  execution by the browser.
- **One external origin in the CSP** — `accounts.google.com` for sign-in. The
  fonts are ours.
- **The admin boundary:** `X-Admin-Key` (constant-time comparison) plus an IP
  allowlist plus basic auth at the nginx level. A refusal distinguishes the
  cause: a wrong key is not commented on in the response, a rejected address is
  named.
- **Headers are trusted only from `TRUSTED_PROXY_IPS`** — otherwise a client
  would give itself someone else's address and bypass both the rate limit and
  the allowlist.
- **Rate limiters** on every heavy endpoint; **SSRF hardening** in unfurl and
  push.

## 9. What is missing (honestly)

- One application instance is a point of failure; there is no horizontal
  scaling
- No native mobile applications, only the PWA
- SSO is OIDC only — no SAML; RBAC is limited to roles inside groups
- No group calls
- **No external penetration test has been performed**
- File links work for any authenticated user who knows the URL (the name is
  128 bits from `crypto/rand`)

---

## 10. Development conventions

**The product language is US English** — the interface, the documentation, log
lines and comments alike. A string a user will see must not be in any other
language.

**The client is not built.** `web/` is served statically as it is: no bundler,
no transpilation, no `node_modules` in production. This is a deliberate
constraint — the cost of entry for operating the system must stay low.

**Comments explain "why", not "what".** There are many places in the code where
the obvious solution is wrong (the shape of the `/history` query, the order of
calls when disbanding a group, suppressing XFF) — those carry a comment with
the reason. When changing such a spot, read the comment first: the simple
version has probably already been tried.

**Every test needs a control case.** A check that also passes on broken code
checks nothing — that has happened in this project already.

### Checks

```bash
go build ./... && go vet ./... && gofmt -l server

export TEST_DATABASE_URL='postgres://user@localhost:5432/hexeris_test?sslmode=disable'
go test -race ./...                    # integration tests need the database
```

CI runs both builds (with and without LDAP), `gofmt`, `vet`, `go test -race`
against a live Postgres, `node --check` over every JS file, and a Docker build
that has to answer on `/healthz`.

---

## 11. Key environment variables

| Variable | Why |
|---|---|
| `SERVER_ENC_KEY` | encryption of bodies and files. **Losing it means losing data**. Its fingerprint is compared against the database: with a foreign key the server does not start (`docs/operations/ENC-KEY-GUARD.md`) |
| `JWT_SECRET` | token signing; the server does not start without it |
| `ADMIN_KEY` + `ADMIN_ALLOWED_IPS` | the admin boundary |
| `TRUSTED_PROXY_IPS` | whose `X-Forwarded-For` to believe (loopback is always trusted) |
| `REGISTRATION_ENABLED` | open registration; **disabled** by default |
| `DB_BACKUP_ENABLED`, `DB_BACKUP_KEEP`, `DB_BACKUP_INTERVAL_HOURS` | backups |
| `RETENTION_ENABLED`, `LOGIN_AUDIT_KEEP_DAYS` | retention, including personal data |
| `LDAP_URL` (with a `-tags ldap` build) | corporate directory |
| `OIDC_ISSUER`, `OIDC_CLIENT_ID` | corporate single sign-on |
| `GOOGLE_CLIENT_ID`, `ALLOWED_EMAIL_DOMAINS` | Google Workspace sign-in |

Every variable in [`.env.example`](../../.env.example) comes with the command
that generates it.

---

## 12. Where to look next

| File | About |
|---|---|
| [README.md](../../README.md) | quick start, the first user, a checklist after installation |
| [ARCHITECTURE.md](ARCHITECTURE.md) | how it works, delivery guarantees, indexes, optimisations with measurements |
| [SECURITY.md](../security/SECURITY.md) | threat model, encryption, what was tested |
| [DEPLOY.md](../operations/DEPLOY.md) | installation, production, the admin host, common problems |
| [BACKUP.md](../operations/BACKUP.md) · [DISASTER-RECOVERY.md](../operations/DISASTER-RECOVERY.md) | backups and recovery |
| [RETENTION.md](../operations/RETENTION.md) | retention periods, personal data |
| [ROADMAP.md](../../ROADMAP.md) | maturity, measurements and the **honest limitations** |
