# Guarding against a substituted `SERVER_ENC_KEY`

The server remembers which key its database was encrypted with, and **refuses
to start** if that key has been swapped.

---

## What this prevents

`SERVER_ENC_KEY` encrypts message bodies in the database and files on disk.
Before this guard existed, a server with an accidentally changed key started as
if nothing had happened:

- old messages could no longer be decrypted — on failure `decryptBody` returns
  the stored value as it is, so the user sees base64 rubbish instead of their
  conversation;
- new messages are written with the new key.

The result is a database with two encryption eras. It breaks quietly: the
complaint arrives not at restart but when somebody scrolls their history
upwards — and by then a layer of new data sits on top of the old one, and there
is almost nothing left to separate the mixture with.

The important part is that **at start-up the data is still intact**: putting
the previous key back is enough. Refusing to start is therefore plainly cheaper
than running.

Where substitution comes from in practice: moving to another machine without
the secrets, an installation script regenerating `.env`, restoring a database
into an environment with a different key, a typo while editing a unit file.

---

## How it works

At start-up, immediately after `initDB`, the row `enc_key_fingerprint` in the
`server_meta` table is compared.

| In the database | Action |
|---|---|
| no row | record the fingerprint and start (first run, or an empty database) |
| matches | start silently |
| does not match | **`log.Fatal`** with an explanation and both fingerprints |
| table unavailable | skip the check and note it in the log |

That last row is deliberate: one service table being unavailable is no reason
to stop a working messenger.

What is stored is a **SHA-256 of the key** (the first 16 hex characters), not
the key itself: a database dump must not hand over the means to decrypt its own
contents. Sixteen characters are enough to tell one key from another and
useless for guessing one.

The check runs on every start and does not depend on any configuration setting
— it has no off switch.

---

## If the server did not come up

The log (`journalctl -u hexeris -n 50`) names both fingerprints and what to do.
In short:

**The ordinary case — the key was simply lost or mixed up.** Restore the
previous `SERVER_ENC_KEY`; look for it in the backup of the service
configuration (see `docs/operations/BACKUP.md` — the key is stored separately
from the dumps for exactly this reason).

**The key is being changed deliberately, and losing the old data is accepted.**
Confirm it explicitly, naming the fingerprint of the new key — the one the
server printed in the log:

```bash
SERVER_ENC_KEY_ACK=<fingerprint of the new key>
```

The confirmation requires the fingerprint rather than the word `yes`: a typo in
the key itself produces a different fingerprint and will not pass. After a
successful start the fingerprint in the database is updated, and
`SERVER_ENC_KEY_ACK` must be **removed** — otherwise the next substitution
passes unnoticed, exactly as it did before this guard existed.

⚠️ Confirming does not re-encrypt anything. Whatever was encrypted with the
previous key cannot be read with the new one. If the history matters, do not
confirm — re-encrypt: `hexeris rotate-enc-key` (`ENC-KEY-ROTATION.md`). After a
rotation the fingerprint in the database updates itself and no confirmation is
needed.

---

## Tests

`server/enckeyguard_test.go` (requires `TEST_DATABASE_URL`):

- one key's fingerprint is stable, different keys differ, and the fingerprint
  contains no fragment of the key;
- the first run records and admits;
- an unchanged key admits;
- **a changed key does not admit**, and rewrites nothing in the process —
  putting the previous key back must remain sufficient;
- the confirmation is accepted only with the correct fingerprint: empty, `yes`,
  `true`, the old fingerprint and a truncated new one are all rejected, while
  surrounding whitespace (as systemd supplies it) is tolerated;
- after a confirmation the next start proceeds without one.
