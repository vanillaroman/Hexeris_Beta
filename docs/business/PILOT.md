# A Hexeris pilot

A document for the customer and the integrator. It answers the four questions
that come up before signing: what exactly we do, how long it takes, what counts
as success, and what happens afterwards.

---

## 1. What the pilot covers

**The perimeter is yours.** Hexeris is installed on the customer's own
infrastructure — your server, or your virtual machine at a hosting provider.
Nothing leaves it. The vendor has no access to the server or to the
conversations unless the customer grants it.

Included:

- deploying one instance (application server + PostgreSQL + nginx);
- domain and TLS setup;
- creating the administrator and the first users;
- onboarding up to 3 departments or working groups;
- the admin panel: users, groups, logs, metrics;
- an administrator briefing (one session, up to 2 hours);
- support for the whole duration of the pilot.

Not included, and worth discussing separately:

- LDAP/AD, OIDC and Google Workspace integration beyond the basic setup;
- migrating conversation history from another messenger;
- customisation for the customer's specific processes;
- group audio and video calls (the product does not have them — see section 6);
- native mobile applications (there is a PWA — see
  `docs/business/PWA-LIMITS.md`).

---

## 2. Timeline

A pilot is planned for **30–90 days**. The recommended length is 45.

| Stage | Duration | Outcome |
|---|---|---|
| Deployment | 1–2 days | A working instance on the customer's domain |
| Creating users | 1 day | Accounts, groups, permissions |
| Trial group | 1–2 weeks | 10–30 people using it daily |
| Roll-out | 2–6 weeks | The remaining departments |
| Review | 2 days | A decision on production use |

Deployment takes hours rather than days: one binary and one database. Most of
the pilot goes not into the technology but into people's habits.

---

## 3. What the vendor commits to

- **A response to a blocking problem within one business day.** Blocking means:
  the service is down, messages are not arriving, or sign-in is impossible.
- **A fix or a workaround for blocking problems within 3 business days.**
- Communication channel: email and a messenger, agreed at the start.
- Updates during the pilot: by agreement, outside working hours.
- Backups are configured at deployment; a restore is rehearsed once with the
  customer present (see `docs/operations/DISASTER-RECOVERY.md`).

What the vendor does **not** promise during a pilot: round-the-clock cover, a
guaranteed uptime figure (SLA), or custom development within the pilot period.
Those belong in a production contract, not in a pilot.

---

## 4. Success criteria

Agree these **before** the start, or the review turns into a matter of taste.
A suggested set:

**Technical** (verified by measurement, not by impression):

- zero lost messages over the period — checked against logs and metrics;
- delivery latency within the customer's network — up to 200 ms at p95;
- availability over the period — no less than 99% per the monitoring data;
- a restore from backup has been performed and met the stated RTO.

**Product**:

- at least 70% of the trial group use the system daily by the end of week 3;
- work conversations have moved off external messengers;
- the administrator performs routine operations — create, block, export a log —
  without contacting the vendor.

**Signs of failure** — worth writing down in advance as well:

- systematic message loss or delay, unresolved after two weeks;
- people return to external messengers despite management's decision;
- a missing feature turns out to be blocking and cannot be added in a
  reasonable time.

---

## 5. What happens after the pilot

Three outcomes, all of them normal:

1. **Moving to production.** The instance stays, the data stays, a support
   contract is signed. No reinstallation is needed.
2. **An extension** of 30 days — if there was not enough time to reach
   everyone.
3. **Stopping.** The instance is shut down and the data is either handed to the
   customer as a database and file export or destroyed on record. Nothing leaks
   anywhere, because all of it was on the customer's side the whole time.

Separately: **data export is possible at any moment**, not only at the end. The
database is ordinary PostgreSQL and the files are ordinary files on disk. No
proprietary formats, no vendor lock-in.

---

## 6. Limitations worth knowing before the start

This list is deliberately honest. Every entry is something that would otherwise
be discovered in week three and taken as a misrepresentation.

| Limitation | Details |
|---|---|
| Group calls | Not available. 1:1 audio and video are. |
| Mobile applications | PWA only (installed from the browser). See `docs/business/PWA-LIMITS.md`. |
| End-to-end encryption | Encryption is server-side, not E2E. The server administrator can technically access the data. See `docs/security/SECURITY.md`. |
| Administrator accounts | A single admin-panel key; there are no individual admin accounts. In the log an action is attributed to `admin`. |
| Threads | Replies to messages exist; separate discussion threads do not. |
| Federation | Not available. One instance is one perimeter. |

---

## 7. What is needed from the customer

A short list, so the start does not stall:

- a virtual machine: 2 vCPU, 4 GB RAM, 40 GB SSD (the reasoning is in
  `docs/operations/BENCHMARK.md`);
- a domain name and the ability to issue a TLS certificate;
- ports 443 open, plus the TURN range if calls are in scope;
- a responsible administrator on the customer's side;
- a management decision about which discussions move into the system — without
  it, the pilot becomes an installation for its own sake.
