# Two-factor sign-in (TOTP)

One-time codes from an authenticator application on top of the local password.
Each employee enables it themselves from the settings menu. Nothing has to be
configured on the server.

---

## What it is for, and what it protects against

A **stolen password**: leaked from another service, read over a shoulder,
phished by email. Employees who sign in through a corporate identity provider
get a second factor from that provider (`../engineering/SSO-OIDC.md`) — local
passwords had none. Local accounts always exist: administrators, service
accounts, contractors, and the entire pilot period before a provider is
connected.

This is **not** protection against someone who already has access to the
server. Nothing on the server protects against that (`SECURITY.md`, "trust
boundaries").

**Why TOTP rather than SMS or push.** TOTP demands nothing of the customer's
infrastructure: no SMS gateway (which costs money and is defeated by a SIM
swap), no push service, no outbound access. An app on a phone and a shared
secret, and that is all. It works with Google Authenticator, Aegis, 1Password
and any other application that understands `otpauth://`.

---

## How it looks to an employee

**Enabling.** Settings → Two-step verification → scan the QR code (or type the
key by hand if there is no camera) → enter the code the app shows → receive
**ten recovery codes**.

The code at enable time is mandatory: until the person has proved that their
app really holds the secret there is nothing to enable — otherwise the very
first outcome is an employee locked out by a failed scan.

**Signing in.** Password → a screen with a code field → the chat. Between those
steps the server issues a **ticket**, not a token: the ticket lives five
minutes, remembers the number of attempts (five) and grants nothing beyond the
right to present a code.

**A lost phone.** Instead of a one-time code, a recovery code is entered — it
works once, and the client says how many are left. When the codes run out, an
administrator resets it.

**Disabling** requires the password **and** a code. Turning the protection off
with one click from an already-open session would reduce it to "while the
laptop is unlocked".

---

## What an administrator sees and can do

The user list shows, for everyone, whether the second factor is on. A **Reset
2FA** button appears only for those who have it.

A reset removes the second factor entirely and **drops all of that employee's
sessions**: otherwise whoever obtained the reset gets in with the password
alone and the owner never finds out. The action is written to the log
(`reset_2fa`).

Do this only when an employee has lost both the phone and all recovery codes,
**and their identity has been confirmed by other means**. "Please reset my
second factor" is a standard social-engineering opening.

---

## Decisions that are easy to get wrong

**The secret is stored encrypted** — with the same `SERVER_ENC_KEY` as message
bodies. A TOTP secret *is* the entire second factor: whoever reads it out of
the database generates codes themselves. In plaintext next to a bcrypt password
hash it would mean that a database dump cancels the second factor, while the
same dump does not cancel the passwords.

**A code cannot be used twice.** The number of the last accepted window is kept
in `users.totp_last_step`, and a code from that window or an earlier one is
refused. Without this, a code read over a shoulder keeps working for another
minute and a half — exactly the length of the tolerance window. The check and
the write are one `UPDATE` with a condition: two parallel attempts would
otherwise both succeed.

**The tolerance is exactly one step** (±30 seconds, a minute and a half in
total). Less, and sign-in breaks on the drift between a phone's clock and the
server's; more, and the window *is* the time during which someone else's code
still works.

**Between the password and the code stands a ticket, not a token.** A token
"while verification is in progress" would mean no second factor at all: it
already reads conversations. The ticket lives only in the tab's memory — in
`localStorage` it would survive the tab closing and become a pass for entering
a code without a password.

**Recovery codes are hashed with sha256, not bcrypt.** We generate the code and
it carries 80 bits of randomness, so brute force is pointless regardless of
hash speed. Bcrypt would mean ten of its computations per sign-in attempt, that
is a second of CPU time per request.

**A failed second factor is its own outcome in the sign-in log** (`bad_totp`,
not `bad_credentials`). That is precisely the line that reveals a stolen
password, and mixing it with ordinary typos is not acceptable.

---

## Configuration (optional)

| Variable | Default | Meaning |
|---|---|---|
| `TOTP_ISSUER` | `APP_DOMAIN`, otherwise `Hexeris` | How the account is labelled in the authenticator app |

The instance name rather than "Hexeris" in general: a person may have several,
and there would otherwise be nothing to tell them apart by in the list.

---

## Endpoints

| Method | Path | Purpose |
|---|---|---|
| `GET` | `/auth/2fa/status` | Whether it is on; how many recovery codes remain |
| `POST` | `/auth/2fa/setup` | A new secret, an `otpauth://` link and a QR code |
| `POST` | `/auth/2fa/enable` | Confirm with a code and enable; returns the recovery codes |
| `POST` | `/auth/2fa/disable` | Disable (password + code) |
| `POST` | `/auth/2fa/verify` | The second sign-in step: ticket + code → token |

All except `/verify` require a valid token. `/verify` is public — at that step
there is no token yet.

The QR code is returned **inside the response** as a `data:` URI rather than at
its own address: the URL of such a request would carry the secret and would
settle in the nginx log and in browser history.

---

## The dependency

Drawing the QR code uses `github.com/skip2/go-qrcode` (pure Go, no transitive
dependencies). Departing from the project's habit of writing its own code was
deliberate: hand-written Reed–Solomon coding and masking fail in exactly the
way that costs most here — the image looks right and does not scan. The binary
grew from 14.8 to 15.2 MB (+2.7%), part of which is the second-factor code
itself.

TOTP itself is ours — thirty lines over HMAC from the standard library — and it
is checked against the RFC 6238 reference values.

---

## Tests

**The algorithm** (`server/totp_test.go`, no database): agreement with the
reference values of **RFC 6238 Appendix B** — the one thing that separates
"works" from "produces six digits". Plus: a tolerance of exactly one window and
not a step more, rejection of malformed codes, acceptance of a code containing
a space (apps display it as "123 456"), and a correct `otpauth://` link.

**Behaviour** (`server/twofa_test.go`, requires `TEST_DATABASE_URL`): signing
in with the factor enabled returns a ticket and does **not** return a token or
a cookie; reusing a code is refused, as is a code from an earlier window; the
ticket dies after five attempts and on expiry; enabling does not succeed with a
random code; disabling succeeds neither without the password nor without the
code; recovery codes work once each, and re-issuing cancels the old list; the
secret in the database is encrypted; an administrator's reset removes
everything and returns sign-in to a single step.

**The interface** (`tests/ui/uitest_twofa.js`): before the code there is
neither a token nor a ticket in `localStorage`; the refusal reason arrives
verbatim; after a refusal the button is clickable again; a dead ticket returns
the user to the sign-in screen with an explanation; going back discards the
ticket.
