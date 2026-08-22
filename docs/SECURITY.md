# Security

The actual security model, written for technical due diligence. Everything
below is implemented in the code (`server/*.go` and the reverse-proxy
configuration) unless explicitly marked ⬜ planned or 🟡 partial. The honest
list of limitations is at the end and in `ROADMAP.md`.

## 1. Transport and HTTP headers

- **TLS** is terminated by the reverse proxy (`TLS_CERT`/`TLS_KEY` when the
  application terminates it itself).
- **Security headers** are set by Go middleware (`server/main.go`) as a single
  source, never duplicated by the proxy:
  - `Content-Security-Policy` — `script-src 'self'` **without
    `'unsafe-inline'`** (all client JS lives in files), `frame-ancestors
    'none'`, `object-src 'none'`, `base-uri 'self'`, `form-action 'self'`. The
    only external origin left is `accounts.google.com` (script/frame/connect)
    for Google sign-in. `font-src 'self'`: the fonts live in `web/fonts`, so
    the client no longer calls a third-party CDN — which used to disclose
    every user's address to it and failed entirely in a closed network.
    `style-src` still allows inline styles (🟡 — `style=""` attributes remain
    in the markup).
  - `X-Frame-Options: DENY`, `X-Content-Type-Options: nosniff`,
    `Referrer-Policy: strict-origin-when-cross-origin`,
    `Permissions-Policy: camera=(self), microphone=(self), geolocation=(), payment=(), usb=()`.
- **Stored-XSS defence:** a strict username pattern, escaping on every DOM
  insertion, and event delegation through the `data-act` registry, so there
  are no inline handlers at all.
- **Files:** `/files/` serves inline **only raster images and video**;
  everything else (svg, html, …) gets `Content-Disposition: attachment` plus
  `nosniff`, so it downloads rather than rendering in this origin.

## 2. Authentication and authorisation

- **Passwords** — bcrypt at the library's default cost.
- **JWT** session tokens, with forced sign-out through an in-memory
  `logoutCutoff` (revocation on deletion or blocking) and a `blocked` flag on
  the user.
- **Public registration is off by default** (`REGISTRATION_ENABLED`).
  `/register` answers 403 while it is disabled, so a stranger who finds the
  address of a corporate instance cannot create an account. The "Sign up" tab
  is hidden accordingly.
- **Google OAuth** — the identity token is verified server-side (including
  `aud`), and accounts are bound by `google_sub` (UNIQUE). An existing local
  account with a matching email is never re-bound, which closes account
  takeover by email prefix. The **domain gate** `ALLOWED_EMAIL_DOMAINS` admits
  listed domains only, and creating a **new** account through Google is
  refused while registration is disabled and no domain list is set — otherwise
  Google sign-in is a way around registration being off.
- **Blocking is enforced at every entry point.** `/login` and `/google-auth`
  refuse a blocked account before issuing a token, and `/ws` refuses at
  connect. Without the check on `/google-auth` a block could be bypassed: an
  administrator blocks an employee (`blocked=TRUE`, `forced_logout_at`,
  connections dropped), the employee signs in through Google and receives a
  fresh token whose `iat` is newer than the cutoff, so revocation does not
  catch it. The WebSocket handler would still reject them, but the HTTP
  endpoints (`/history`, `/search`, `/files/`, `/upload`, `/api/profile`)
  verify only the signature, and a blocked account could keep reading
  conversations and downloading files. The boundary is pinned by
  `TestIntegrationBlockedUserTokenBoundary`.
- **`/google-auth` is rate-limited** (30 per 10 minutes per IP) and calls
  Google with a timeout. The endpoint is unauthenticated: without a limit every
  request reaches an external service and may compute bcrypt, and an
  unbounded HTTP client would let a stalled response hold a goroutine forever.
- **A media cookie** — `/api/session-cookie` issues an HttpOnly cookie so
  `<img>` and `<video>` can fetch `/files/` authenticated, since a browser does
  not send bearer tokens from tags.
- **Contact details are visible only to contacts.** `GET /api/profile?user=`
  returns `email` and `phone` only to someone sharing context with that user (a
  conversation or a group, via `hasContactWith`); anyone else gets the card
  without them. Name, position and avatar stay visible, since that is how a
  colleague is found. Serving contacts of **any** user to **any** signed-in
  caller, unlimited, is a downloadable staff directory of personal data plus an
  account-existence oracle (200 versus 404). A dedicated limiter allows 200
  lookups of other people's cards per 10 minutes; one's own profile is exempt.
  The list endpoint `/api/profiles` never had the hole, being bounded by the
  caller's peer list. Pinned by
  `TestIntegrationProfileContactsHiddenFromStrangers`.
- **Sign-in audit** — the `login_audit` table records who, from where, when and
  with what outcome (`ok`, `bad_credentials`, `blocked`, `rate_limited`), the
  method (`password` or `google`), the IP address and the user agent. It
  answers a security team's first question and explains rate-limiter blocks:
  without failed attempts recorded, a blocked address looks like an event with
  no cause. Visible in the **Sign-in Log** tab of the admin panel or through
  `/admin/login-audit`.
  ⚠️ **This is personal data** (a name plus an address), so retention is bounded
  by `LOGIN_AUDIT_KEEP_DAYS` (90 days by default) and pruned **independently**
  of `RETENTION_ENABLED`: general retention is the operator's decision about
  conversation content, while operational personal data should not accumulate
  indefinitely even where that is switched off.
- **Account creation** — with public registration off, accounts come from an
  administrator (`/admin/user-action` with `create`), an LDAP/AD directory, or
  Google sign-in from an allowed domain. An admin-created account is flagged
  `must_change_password`: while someone other than the owner knows the
  password the account is not theirs, and the client shows the password screen
  instead of the chat. The flag is also returned in the user's own profile, so
  reloading the page does not bypass it. The same flag is set by an
  administrative `reset_password`. A temporary password is 14 characters from
  `crypto/rand` (~83 bits) over an alphabet without ambiguous glyphs, returned
  **once**; the database holds only the hash.
- **Changing one's own password** (`/change-password`) — without it a user
  whose password leaked could not close access themselves. It always requires
  the **current** password, including the first change, or a stolen token would
  mean permanent account takeover. A successful change revokes all previous
  tokens (`forced_logout_at`) and drops live connections — signing the other
  devices out, which is the point of changing a leaked password — while the
  current session receives a new token immediately. Directory accounts are
  refused (409): their password lives in the directory and no local one may be
  created here.
- **The admin boundary** — `/admin/*` sits behind `adminGuard`: an
  `X-Admin-Key` check (`ADMIN_KEY`) **plus** an IP filter, both on the Go side.
  The key lives server-side only; the operations proxy injects it into
  `/admin-api/*` and it never reaches a browser. On top of that the panel host
  adds basic auth and its own IP allowlist. That is a **double boundary**: a
  misconfigured proxy still cannot open the endpoints. A ready proxy template
  is `docs/admin-panel/nginx-admin-panel.conf`.
  ⚠️ The IP filter depends on the proxy **not** forwarding a browser's
  `X-Forwarded-For`: the messenger trusts its own proxy and reads the client
  address from that header, so a forged one would bypass `ADMIN_ALLOWED_IPS`.
  The supplied configuration clears `X-Forwarded-For` and `X-Real-IP`
  explicitly, verified end to end.
- **CSV exports** (users, audit, sign-in log) follow RFC 4180 escaping, and
  values a spreadsheet would execute as a formula (`=…`, `@SUM(…)`, `+SUM(…)`,
  `-WEBSERVICE(…)`) are prefixed with an apostrophe. The fields come from user
  input (username, position, user agent), so this is a boundary rather than
  cosmetics. The rule is narrow — "can call a function" rather than "starts
  with a sign" — because the broad version mangled every phone number.

## 3. SSRF hardening

The server fetches external URLs in two places, and both are protected:

- **`/api/push/subscribe`** requires an external **https** endpoint, and
  deliveries go through a dialler that refuses private and reserved ranges.
  Ten subscriptions per account at most.
- **`/unfurl`** (link previews) uses the same dialler; after the redirect limit
  a 3xx response is **not** accepted; timeouts throughout.

## 4. Rate limiting (the actual limits)

From `server/limiter.go`; the default window is 10 minutes.

| Endpoint / action | Limit | Key |
|---|---|---|
| `/login` | 5 / 10 min | IP |
| `/login` | 10 / 10 min | username (anti-botnet) |
| `/register` | 3 / 10 min | IP **and** 3 / 10 min per username |
| `/upload` | 30 / 10 min | user |
| `/search` | 60 / 10 min | user |
| `/status` | 200 / 10 min | user |
| `/history` | 300 / 10 min | user |
| `/reactions` (sync) | 300 / 10 min | user |
| edit/delete message | 80 / 10 min | user |
| reaction (WS, writes to the database) | 120 / 10 min | user |
| group creation | 20 / 10 min | user |
| `/unfurl` | 40 / 10 min | user |
| `/api/profile?user=` (other people's cards) | 200 / 10 min | user |
| `/google-auth` | 30 / 10 min | IP |

`X-Forwarded-For` counts as a trust boundary only behind the configured proxy,
which is covered by a unit test.

> ⚠️ **The per-IP `/login` limit is a blunt instrument behind NAT.** It counts
> failed sign-ins and keys on the address: five failures in ten minutes block
> the **whole address**. Behind a corporate NAT the entire office shares one,
> so five typos by different people close sign-in for everyone for ten
> minutes. Guessing at a specific account is covered by the separate per-username
> limiter (10 per 10 minutes), which NAT does not break. This was observed
> during load runs, where a few accounts with wrong passwords took down the
> whole pool. The threshold is deliberately unchanged — loosening it weakens
> the defence against distributed guessing — but an instance behind corporate
> NAT should account for it.

## 5. Protecting the database and resources from abuse

- **Group creation** — an unbounded member list in one request would mean tens
  of thousands of database operations. The size limit now applies at creation
  as well as when adding members, alongside a creation rate limit.
- The group size limit (200) applies to both paths.
- Message length is one constant (`maxMessageRunes`, 4096) across **both** the
  WebSocket path and `/edit-message`; otherwise editing bypasses the limit.
- Uploads pass through `MaxBytesReader` at 60 MB, before the multipart form is
  parsed, so overflow never lands on disk.
- History and search are paginated, and `DB_STATEMENT_TIMEOUT_MS` (10 s by
  default) bounds every query.

## 6. Encryption at rest

- **Message bodies** in the database — **AES-256-GCM**.
- **Files** on disk — **AES-256-CTR**, streamed with Range support.
- One key, `SERVER_ENC_KEY`. ⚠️ **A backup without it is useless**; store it
  separately (see `BACKUP.md`). Legacy plaintext is read transparently, and
  damaged ciphertext is rejected; both are covered by tests.

## 7. What tests and CI verify

- **CI** on every push and pull request: conflict markers, `gofmt`, both Go
  builds (default and `-tags ldap`), `go vet`, `go test -race` against a **real
  Postgres**, and `node --check` over every JS file.
- **Unit tests** (`core_test.go`): body encryption with legacy passthrough and
  rejection of damaged ciphertext, JWT with the logout cutoff, the private-IP
  filter including CGNAT boundaries, the `X-Forwarded-For` trust boundary, the
  rate limiter, DSN parsing.
- **Integration tests** (`integration_test.go`, against Postgres): delivery
  idempotency, encryption and decryption through `/history`, pagination, group
  access (403 for non-members), edit/delete authorisation with the length
  limit, read receipts, retention.
- **Delivery guarantees** (`delivery_test.go`): a missing ACK is not a loss; a
  message is not consumed by a failed socket write; history rebuilds through
  `seq` without gaps or duplicates under concurrent writes; the scope of the
  all-conversations sync (nothing foreign, nothing of one's own missing); the
  offline queue is bounded; contacts are hidden from strangers.
- **Concurrency** (`concurrency_test.go`, no Postgres needed, meaningful only
  with `-race`): the race on the connection list during a disconnect (which
  failed with `DATA RACE` before the fix) and writer survival after a panic.

## 8. Known limitations

- 🟡 `style-src 'unsafe-inline'` is still required (`style=""` attributes in the
  markup); removing them is on the roadmap.
- ⬜ **No** enterprise SSO (SAML/OIDC), no RBAC, no organisation management, no
  audit trail of user actions (only `admin_audit` exists).
- ⬜ **No** DLP or antivirus scanning of uploads — any file type is accepted
  (magic-byte checks apply to images only; everything else is served as a
  download).
- ⬜ Compliance certifications (GDPR/DPA) are **not claimed**; per-user data
  export and full deletion are on the roadmap.
- 🟡 **`/files/` links are capability URLs, not bound to conversation
  participants.** Any authenticated user who knows the link can download the
  file; there is no check that they belong to that conversation. The name is
  128 bits from `crypto/rand`, so guessing is impractical, but a leaked or
  forwarded link keeps working indefinitely, including for someone removed from
  the group. Binding files to a message and its participants is on the roadmap.
- 🟡 **`logoutCutoff` is an in-memory cache** warmed at startup from
  `users.forced_logout_at`. If that query fails, previously revoked tokens are
  accepted again; the case is now logged as a WARNING rather than passing
  silently.

## 9. What a security review looked at and did not find

A negative result is a result: this records what was examined specifically, so
a repeat audit does not start from zero.

| Area | Examined | Outcome |
|---|---|---|
| **authz / IDOR** | `/history`, `/reactions`, edit/delete, groups (roles, membership, leave) | No violations: ownership and membership are checked on every endpoint |
| **JWT** | signing method, `alg=none` and confusion, `exp`, revocation | The method is pinned, algorithm substitution is impossible, and the cutoff cache is warmed at startup |
| **Google sign-in** | `aud`, `email_verified`, the domain gate, bypassing the registration gate, takeover by email prefix | Closed; **a block bypass was found and fixed** (see §2) |
| **File serving** | path traversal, sniffing, Range, the legacy path | `filepath.Base` with `..` rejected; everything outside the inline list is `attachment` plus `nosniff`, so even `.html` and `.svg` download instead of executing; ranges parse correctly |
| **Uploads** | size-limit bypass, type spoofing | `MaxBytesReader` before the form is parsed; magic bytes verified for raster images |
| **Client-side XSS** | every `innerHTML` render path | Data passes through escaping; match highlighting escapes before inserting `<mark>`; link previews (third-party data) are escaped whole |
| **SSRF** | `/unfurl`, `/api/push/subscribe` | Resolved once, every resulting address checked, and the connection made to the checked address (DNS rebinding closed); non-http(s) redirects refused |
| **Admin boundary** | `adminGuard` | Constant-time key comparison plus the IP filter; an empty `ADMIN_KEY` fails startup rather than opening the endpoint |

- 📏 No external penetration test has been performed. An internal audit covered
  the authorisation boundaries, the SSRF dialler, JWT handling, uploads and
  file serving, and the rate limits. What it found, and the fixes, are recorded
  in `ROADMAP.md` and `docs/ARCHITECTURE.md`.

Remediation priorities are in `ROADMAP.md`.
