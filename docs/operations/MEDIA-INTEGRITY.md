# Attachment integrity

The link to an attachment lives in the message body; the file itself lives in
`UPLOAD_DIR`. These two halves drift apart quietly, and from the outside it
always looks the same: "some pictures in the conversation will not open, the
rest are fine".

The causes are always the same handful:

- a move to another server where the upload directory was forgotten;
- restoring a database from a copy newer than the copy of the directory;
- freeing disk space by hand.

## Checking

Exactly three variables are needed — the same ones the service runs with:
`DATABASE_URL`, `SERVER_ENC_KEY`, `UPLOAD_DIR`. Neither `JWT_SECRET` nor
`ADMIN_KEY` is required for the check.

The simplest way is to pick up the running process's environment:

```bash
sudo tr '\0' '\n' < /proc/$(pidof hexeris)/environ > /tmp/hexeris.env
set -a; . /tmp/hexeris.env; set +a
./hexeris check-media          # -v lists everything rather than the first 20
rm -f /tmp/hexeris.env         # it holds secrets — do not leave it lying about
```

Or set them by hand:

```bash
DATABASE_URL=… SERVER_ENC_KEY=… UPLOAD_DIR=… ./hexeris check-media
```

Run with no variables at all, the command lists every missing one at once and
shows these same recipes — there is no need to discover them one at a time.

The command is **read-only**: it changes nothing and deletes nothing. It walks
the messages, decrypts the bodies, extracts the `/files/…` links and compares
them against the disk — by exactly the same route the server uses to open a
file (`filepath.Base` plus `UPLOAD_DIR`).

The report says how many links there are in total, how many files are present,
which are missing and what period they belong to. **The period is the most
useful part.** If the gaps are continuous in time, what was lost is not an
individual file but the directory for that stretch, and the thing to look for
is a move or a mismatch between backup copies.

## What to do about missing files

Restore from the backup of the upload directory (see
`docs/operations/BACKUP.md`). Files are named after their content and are never
overwritten, so a copy can be unpacked over the current directory without any
risk of clobbering newer files.

If there is no copy, the attachments are gone for good: the server keeps them
on disk and nowhere else. The messages themselves survive; only the content is
missing.

## Why the browser shows 404 for longer than the problem exists

Error responses for `/files/` are served with `no-store`, but that was not
always so: an error response used to inherit the general media header
(`private, max-age=86400`) and settled into the browser for a day. The client
then showed a 404 out of its own cache without asking the server — and the log
stays empty, because there is no request.

**The signature of this particular case:** `check-media` reports every
attachment present, `curl` with the session cookie returns the file with a 200,
and the browser goes on showing 404 with not a line in the log.

The cure on one machine is DevTools → Application → Storage → **Clear site
data**. An ordinary reload is not enough: pictures are loaded dynamically, and
for those the browser looks in the cache again.

The cure for everyone at once is the `MEDIA_CACHE_BUST` constant in
`web/js/helpers.js`. It appends a `?v=<number>` marker to media addresses;
raise the number by one and browsers re-request the attachments once. Ordinary
application updates do not need this — the constant is changed by hand, and
only when a stuck cache has to be cleared.
