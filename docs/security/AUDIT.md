# What a security team can see

A document for the reviewing side: which events are recorded, how to obtain
them, how long they are kept, and what the logs do **not** contain.

---

## Two logs, and why there are two

| Log | Answers | Table |
|---|---|---|
| Sign-ins | "Who signed in, and from where" | `login_audit` |
| Administrator actions | "What was done to the system" | `admin_audit` |

The split is not cosmetic. During an incident review these are two different
questions, and almost always both get asked: first "who signed in", then "what
did they change".

---

## The sign-in log

**Every** sign-in attempt is recorded, including the failed ones.

| Field | Contents |
|---|---|
| `username` | The login used |
| `outcome` | `ok`, `bad_credentials`, `bad_totp`, `blocked`, `rate_limited` |
| `method` | `password`, `ldap`, `google`, `oidc` |
| `ip` | The source address |
| `user_agent` | Browser and device (up to 200 characters) |
| `created_at` | When the attempt happened |

Failed attempts are recorded deliberately: without them, the brute-force
protection firing looks like a block with no cause. It also explains the NAT
situation, where a whole office arrives from one address — see `SECURITY.md`.

---

## The administrator action log

Recorded: creating a user, blocking and unblocking, resetting a password,
forcing a sign-out, deleting a user together with their conversations, deleting
a group, removing a member — **and exporting the logs themselves**.

That last one matters: "who took the log outside" is the first question in a
leak investigation, and the answer to it has to be in the log.

A separate entry covers **exporting an employee's messages**
(`message_export`, with the employee as the `target`). It is the only action in
the panel that hands message content to the outside, and therefore the only one
that requires a reason: the reason is written to `details` and cannot be
edited. It is also the only one that is **not performed** if the log write
fails — an unrecorded export of somebody's correspondence is worse than an
export that did not happen. Details in `../operations/MESSAGE-EXPORT.md`.

| Field | Contents |
|---|---|
| `action` | The machine name of the action |
| `target` | Whom or what it was done to |
| `details` | Free-text clarification |
| `ip` | The administrator's address |
| `created_at` | When the action happened |

---

## How to export

```
GET /admin/audit-export?from=2026-08-01&to=2026-08-31&kind=all&format=csv
Header: X-Admin-Key: <admin panel key>
```

| Parameter | Values | Default |
|---|---|---|
| `from`, `to` | `YYYY-MM-DD` or RFC3339 | the last 30 days |
| `kind` | `all`, `admin`, `login` | `all` |
| `format` | `csv`, `json` | `csv` |

The boundaries include the **whole** day: `to=2026-08-31` means up to and
including 23:59:59. The CSV is served with a BOM, so it opens straight into
Excel without mangling non-Latin text.

The export ceiling is 200 000 rows. On reaching it the response carries an
`X-Hexeris-Truncated` header; there is no silent truncation.

Example:

```bash
curl -H "X-Admin-Key: $ADMIN_KEY" \
  "https://<domain>/admin-api/admin/audit-export?from=2026-08-01&to=2026-08-31&format=csv" \
  -o audit-august.csv
```

---

## Retention

Set through environment variables and swept automatically:

| What | Variable | Default |
|---|---|---|
| Administrator actions | `RETENTION_AUDIT_DAYS` | 180 days |
| Sign-ins | `LOGIN_AUDIT_KEEP_DAYS` | 90 days |

The logs contain personal data (logins and addresses), so the period is bounded
by default rather than unlimited. A value of `0` disables the sweep — enable
that deliberately and with your own policy in mind. More in
`../operations/RETENTION.md`.

---

## What the logs do not contain

This section exists so that a reviewer forms no false expectations.

- **Message content.** The audit records actions, not messages. Who wrote what
  to whom is not in the logs.
- **Individual administrator accounts.** Access to the panel is granted by a
  single key, so every administrative action is attributed to `admin`. Who
  holds the key is an organisational question rather than a technical one. If
  per-person attribution is required, that is a change to be raised before the
  pilot.
- **User actions inside chats.** A user deleting or editing their own message
  does not appear in `admin_audit`.
- **An administrator reading messages.** Direct database access is not recorded
  by the log — a limitation of any system where the administrator has access to
  the DBMS. It is compensated organisationally, by separating access to the
  server from access to the admin-panel key.

---

## A typical review

1. Request a `kind=login` export for the period — see every sign-in and every
   failed attempt.
2. Request a `kind=admin` export for the same period — see every change.
3. Cross-check: every administrative action should be preceded by an
   administrator sign-in from the same address.
4. Check that `audit_export` records are present in the export — that is, that
   the act of exporting was itself recorded.
5. Check the `message_export` records: each one must name a reason. Exporting
   someone's correspondence without grounds is the first thing to look for.
6. Confirm that retention is configured and the sweep runs
   (`../operations/RETENTION.md`).

---

## Trust boundaries

Read alongside `SECURITY.md`. In short: encryption is server-side, not
end-to-end. The server administrator can technically access the data. That is a
deliberate choice in favour of archiving and auditing — with end-to-end
encryption neither is possible. If the customer's threat model requires
protection **from the administrator**, Hexeris does not meet it, and that
should be acknowledged before a pilot rather than after one.
