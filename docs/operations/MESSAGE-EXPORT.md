# Exporting one employee's messages

A single file with one person's decrypted conversations for a period — CSV or
JSON. For internal investigations and lawful requests.

The **Message Export** tab in the admin panel; the endpoint is
`/admin/message-export`.

---

## Why this exists

The question comes up almost immediately in a pilot: "an employee left under a
cloud / there is an internal investigation / a request has arrived — export
their messages for the period". Before this endpoint the answer was "go into
the database with psql", and that is not an answer:

- message bodies in the database are encrypted with `SERVER_ENC_KEY`;
- attachments sit in separate files under random names;
- group messages are stored differently from direct ones.

So the export would have to be assembled by hand, with a real risk of
assembling it wrongly — and personnel and legal decisions are taken on the
strength of it.

---

## Three restrictions no other endpoint has

This is the **only** place in the project that hands decrypted message bodies
to the outside. Everything else under `/admin` deals in metadata: who, when,
from where. Hence restrictions that exist nowhere else.

**1. `user` is mandatory.** There is no "export everything" button. Exporting
correspondence is a targeted act about a specific person, and a forgotten
parameter has no right to turn into a dump of the whole company's
conversations.

**2. `reason` is mandatory** (at least 8 characters) **and goes into the
administrator log.** A security team does not ask "may this be exported", it
asks "who exported it, and on what grounds". An entry reading "admin exported
Smith's messages" with no reason answers nothing.

**3. The log entry is written BEFORE the file is served, and if it fails the
export is cancelled.** This is the only place in the project where an
unavailable audit table stops the operation itself: an unrecorded export of
somebody's correspondence is worse than an export that did not happen. (For
every other administrative action the trade-off runs the other way: blocking an
employee must not be undone by a logging failure.)

---

## Parameters

| Parameter | Required | Default | Meaning |
|---|---|---|---|
| `user` | **yes** | — | Whose messages |
| `reason` | **yes** | — | The grounds; written to the log (8–500 characters) |
| `from` / `to` | no | the last 30 days | `YYYY-MM-DD` or RFC3339, inclusive |
| `with` | no | every chat | Narrow to one peer or group (`g:…`) |
| `format` | no | `csv` | `csv` or `json` |
| `deleted` | no | off | Include deleted messages |

The period boundaries cover whole days: `from=2026-08-01&to=2026-08-31` means
00:00 on the 1st through 23:59:59 on the 31st.

```bash
curl -H "X-Admin-Key: $ADMIN_KEY" -OJ \
  "https://<domain>/admin/message-export?user=jsmith&from=2026-01-01&to=2026-03-31&reason=HR%20case%202026-114"
```

---

## Columns

`at_utc`, `seq`, `chat`, `chat_kind`, `direction`, `sender`, `recipient`,
`media_type`, `body`, `attachments`, `reply_to`, `forwarded`, `edited`,
`deleted`, `message_id`.

`chat` answers "who was this conversation with": for a sent message it is the
recipient, for a received one the sender, and for a group its name and id.
`direction` is `sent` or `received` relative to the person being exported.

The CSV starts with a UTF-8 BOM: without it Excel opens the file in the system
encoding and non-Latin text turns to rubbish — and Excel is exactly what these
exports get opened in.

---

## What does NOT go into the file

This is also stated inside the file itself (the `notes` field in JSON): the
report leaves and lives a life of its own, and whoever opens it six months
later will not be reading the documentation.

- **Attachments are not bundled.** The `attachments` column lists file names as
  they sit in `UPLOAD_DIR`; the files themselves are encrypted at rest. If the
  files are needed, they are taken from `UPLOAD_DIR` by those names and
  decrypted with the same key (`docs/operations/MEDIA-INTEGRITY.md`).
- **Groups the person has left.** Their own messages to those groups are in the
  export (they were the sender); other people's are not. Membership history is
  not kept in the schema — only `joined_at` — and pretending the export is
  complete would be wrong.
- **Deleted messages**, unless asked for explicitly. Every such row is marked
  `deleted=true` so that it cannot be presented as an ordinary one.
- **More than 100 000 messages at a time.** The ceiling is not silent: the
  response carries `X-Hexeris-Truncated`, the panel shows a warning, and the
  JSON has a `truncated` field. The cure is a narrower period.

---

## Who can see this

The endpoint sits behind `adminGuard`: the `ADMIN_KEY` plus `ADMIN_ALLOWED_IPS`
if a list is configured. There is no separate "may export messages" role —
administrators have no individual accounts at all
(`docs/security/SECURITY.md`), and who holds the key is an organisational
question. That is precisely why the reason is mandatory: it is the only thing
that separates a lawful export from curiosity when the log is read six months
later.

The response is marked `no-store`: an export of someone's correspondence must
not settle in any intermediate cache.

---

## Tests

`server/msgexport_test.go` (requires `TEST_DATABASE_URL`). Two kinds of failure
are checked, each expensive in its own way:

**Took too much** — other people's correspondence ended up in the export; a
group the person is not a member of.

**Lost what was needed** — a message inside the period was not exported; the
evening of the final day was lost; a body was not decrypted; a group name was
not substituted; attachment names were not collected.

Separately: without `user` and without `reason` no export happens, the reason
reaches the administrator log, the CSV starts with a BOM and is served with
`no-store`.

The panel is covered by `tests/ui/uitest_adminpanel.js`: without an employee,
without a reason and without a confirmation the request does not leave; the tab
order matches the `TABS` list; the button's route matches the server's.
