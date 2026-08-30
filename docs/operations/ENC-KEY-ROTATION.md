# Changing `SERVER_ENC_KEY` without losing data

```bash
hexeris rotate-enc-key [--dry-run] [--yes]
```

Re-encrypts message bodies, two-factor secrets and the files in `UPLOAD_DIR`
from the old key to a new one. The run is **restartable**: an interruption
halfway through does not corrupt anything, and running it again finishes what
is left.

The neighbouring document, `ENC-KEY-GUARD.md`, is about not substituting the
key **by accident**. This one is about changing it **on purpose**.

---

## The procedure

```bash
# 1. A new key
openssl rand -base64 32

# 2. Stop the server
systemctl stop hexeris

# 3. Take a backup
hexeris backup

# 4. See how much work it is (changes nothing)
DATABASE_URL=… UPLOAD_DIR=… \
SERVER_ENC_KEY="<current>" SERVER_ENC_KEY_NEW="<new>" \
  hexeris rotate-enc-key --dry-run

# 5. Re-encrypt
… the same command with --yes

# 6. Replace SERVER_ENC_KEY in the service configuration and start
systemctl start hexeris
```

Without `--yes` the command does nothing and reminds you about steps 2 and 3.
Step 2 is not a formality: on a running server some records would appear
**during** the run, still with the old key, and would stay unreadable.

The command needs only `DATABASE_URL`, `UPLOAD_DIR` and the two keys. Not
`JWT_SECRET`, not `STATIC_DIR`, not `ADMIN_KEY` — on a production host those
live in a systemd drop-in, and assembling the whole environment just to
re-encrypt is pointless.

---

## Do not discard the old key

After a rotation it is still needed for **backups taken before it**. The copy
and the key live apart (`BACKUP.md`), and restoring yesterday's dump requires
yesterday's key. Keep it for as long as you keep the copies.

---

## What happens to what

| What | How |
|---|---|
| Message bodies | Decrypt with the old key, encrypt with the new one (AES-256-GCM) |
| 2FA secrets (`users.totp_secret`) | The same |
| Files in `UPLOAD_DIR` | Re-encrypted as a stream, new IV, atomic replacement |
| Files without our header | Left alone — they are served as-is anyway |
| Records unreadable with either key | Left alone, counted separately |

That last row matters: if the "foreign / plaintext" counter in the report is
large and that is a surprise, **do not change `SERVER_ENC_KEY`** until you know
why. Most likely the old key given was wrong.

A two-factor secret that neither key can read is named in the log **by
account**: that employee's 2FA will stop working, and it is better to learn
that now than from them.

---

## How restarting after an interruption works

Re-encrypting hundreds of thousands of records will be interrupted one day:
disk fills up, an operator presses Ctrl-C, a machine falls over. A second pass
over an already re-encrypted record with the **old** key would turn it into
rubbish — and that is precisely the kind of error that stays invisible until
the first read.

**Message bodies and secrets.** AES-GCM is authenticated, so a record is tried
with the new key first and the old one second. If neither fits, the record is
not ours and must not be touched. Integrity here is held by GCM itself; the key
order exists so that a repeat run does not put thousands of finished records
into the unreadable counter — that is, so the report does not look like a
reason to roll back a successful rotation.

**Files.** Those use AES-CTR with no integrity tag (for the sake of Range
requests when seeking in a video, see `filecrypt.go`), so "did this decrypt
correctly?" cannot be computed. Files therefore go through an `enc_rotation`
journal and a temporary file:

1. `<name>.rotating` is written in full — the original is still untouched;
2. a "done" mark is written to the journal;
3. `rename` over the original (atomic).

An interruption at any point is unambiguous:

| State | Meaning | Action on restart |
|---|---|---|
| No mark | The original is old, `.rotating` is unfinished rubbish | Delete the rubbish, redo |
| Mark present, `.rotating` still there | Step 3 did not happen | Finish the `rename` |
| Mark present, no `.rotating` | Finished | Skip |

The order "journal before `rename`" was chosen for exactly this reason: the
reverse would leave, after an interruption, a file encrypted with the new key
and no mark — so a repeat pass would destroy it.

---

## Deliberate refusals

**The old key is not the one the database was encrypted with.** The command
compares the fingerprint and refuses to run. Re-encrypting with a wrong old key
would spoil nothing — but it would also do nothing: not a single record would
decrypt, and the report would look like success.

**Some files were not re-encrypted.** The fingerprint in the database is **not**
updated and `SERVER_ENC_KEY` must not be changed: the server would come up with
the new key and those files would quietly stop opening. The command says so
plainly and suggests running itself again.

**The new key equals the old one.** There is nothing to change.

---

## Tests

`server/rotatekey_test.go` (requires `TEST_DATABASE_URL`).

The important part is restarting after an interruption:

- a database where some records already carry the new key: a repeat pass does
  not spoil them and **recognises** them as done rather than unreadable;
- a file with a finished `.rotating` and a mark in the journal: the move is
  completed;
- a file with a `.rotating` and no mark: the rubbish is discarded, the file is
  re-encrypted from scratch, and the next run leaves it alone.

The rest: a round trip of a message body (and the fact that a foreign key does
**not** fit — the whole era-distinguishing logic rests on that); plaintext is
not mistaken for ciphertext; a dry run changes no data but counts the work; a
file is re-encrypted byte for byte, the original is untouched until the
`rename`, and the IV is not reused; the header tells our file from a legacy one.

The command has also been run end to end against a database of ~14 000
messages: after the rotation `hexeris check-media` with the **new** key finds
the same attachments and with the **old** one finds nothing; a repeat run
reports "13979 already on the new key" and leaves the files with unchanged
checksums.
