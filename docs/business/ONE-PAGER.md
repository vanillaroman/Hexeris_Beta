# Hexeris — one page

## What it is

A corporate messenger that runs on the customer's own infrastructure.
One Go binary plus PostgreSQL. No Redis, no message broker, no search cluster.

## Who it is for

Companies of **50–300 people** that need their own perimeter and have **no
dedicated DevOps**. Typically manufacturing, engineering firms, healthcare,
public sector, finance and legal — organisations where cloud is ruled out by
policy and Mattermost is too expensive to operate.

## How it differs

| | Hexeris | Mattermost / Rocket.Chat |
|---|---|---|
| Components in production | 2 (binary + Postgres) | 4–7 (app, DB, Redis, search, object storage…) |
| Memory at 200 connections | **31 MB** (measured) | hundreds of MB to gigabytes |
| Hardware for a pilot | 2 vCPU / 4 GB | typically 2–4× more |
| Who operates it | a general sysadmin | in practice, a DevOps engineer |
| Upgrades | replace a single file | orchestrate several services |

We do **not** compete on feature count — we are behind there by design.
We compete on total cost of ownership and on the fact that an ordinary
administrator can run it.

## What already works

Direct and group chats · files and voice messages · 1:1 audio and video calls ·
reactions, replies, forwarding, edit and delete · full-history search ·
read receipts · pin, mute, archive · per-chat attachments panel · full-history
search · push notifications · two-factor authentication (TOTP) ·
LDAP/Active Directory, OIDC single sign-on and Google Workspace · admin panel ·
login and admin-action audit logs with export for any period · automated
backups with a rehearsed restore.

## Verifiable numbers

Measured by a load run, reproducible with a script from the repository
(`docs/operations/BENCHMARK.md`):

- **0** lost messages at every load step, including 2700 msg/s;
- **18 ms** p99 acknowledgement latency at 200 connections under a realistic rate;
- **31 MB** resident memory at 200 connections;
- **≈0.55 GB** of database per million messages;
- **≈1.1 GB per year** for a 200-person company.

## Limitations — stated plainly

No group calls. No native mobile apps, PWA only. Encryption is server-side,
**not** end-to-end: the server administrator can technically access the data —
a deliberate trade-off in favour of audit and archiving. There are no separate
administrator accounts. No threads, no federation.

Full list in `ROADMAP.md` and `docs/business/PILOT.md`, section 6.

## How to start

A 30–90 day pilot on the customer's infrastructure. Deployment takes hours.
Terms, obligations and success criteria are in `docs/business/PILOT.md`.
