# What updates by itself, and what used to need a reload

An analysis of the complaint "I enabled a feature on the server and it only
shows after F5", and of what turned up alongside it.

---

## The root cause

The client's entire runtime configuration — application name, calls, Google
sign-in, open registration, SSO — was a **one-off snapshot**: fetched when the
page loaded and applied imperatively, straight into the markup, inside a
`fetch` handler in `bootstrap.js`.

Three problems followed from that one fact, and to a person all three looked
identical — "you have to reload the page":

1. **Nothing ever re-read the configuration.** Not a socket reconnect, not the
   tab regaining focus. The only way to learn of a change was F5.
2. **Application was one-directional:** `if (enabled) show()` with no `else
   hide()`. So even a repeat call would have turned nothing off — a feature
   disabled on the server stayed on the screen, and pressing it led to a
   refusal.
3. **There was nowhere to re-read it from:** the applying code was not a
   function but a fragment of a handler.

`initSSO` in `sso.js` had the same disease in its purest form: `if
(!st.enabled) return` — a button, once shown, was never removed.

---

## What was done

### The configuration became live

`web/js/appconfig.js`:

- **`applyAppConfig(c, sso)`** — an ordinary idempotent function that works in
  **both directions**. Every branch has an `else`.
- **`refreshAppConfig(force)`** — re-reads `/api/config` and
  `/auth/oidc/status` (in parallel) and applies them if anything changed.

It is called wherever the state might have changed:

| When | Why |
|---|---|
| page start | as before |
| **every socket (re)connection** | a server restart carrying a new flag reaches the client exactly this way — there is no other signal |
| the tab regaining focus | no more than once every 10 s |

If nothing changed, the DOM is not touched at all — a signature of the previous
state is compared. Otherwise every reconnect would redraw the buttons in front
of the user.

If **both** requests fail (the network is down), the state is left as it is:
dimming the buttons because of a network glitch is worse than showing them for
an extra minute.

One side effect worth naming: turning registration off while someone is
standing on the "Sign up" tab moves them to "Sign in" — otherwise they would be
left on a form the server will reject.

### Reconnecting without dead seconds

The old backoff started at **3 seconds**: `3000 * 2^0`. The most common
disconnection in production is not "the network died" but a server restart
during deployment, and by the time the browser notices the break the server is
usually already back. Every client paid those 3 seconds at every deployment.

Now the **first attempt is immediate** (with 0–300 ms of jitter so a thousand
clients do not arrive in the same millisecond), and the backoff starts from the
second: `0 → 1s → 3s → 6s → 12s → 24s → 30s`.

The `/status` check (has the account been deleted?) is **skipped** on the first
attempt: it is an extra round trip in front of the connection that matters most
for speed, and a blocked account will be rejected by the socket handshake
anyway — so the check does its work on the next round, a second later.

Measured by a test: a message sent into a dead socket arrives **100 ms** after
recovery.

### Calls

Three things, each of which cost either delay or silence:

**TURN credentials were fetched sequentially after `getUserMedia`.** That is, a
round trip was added to the pause between pressing and the ringing tone, after
the microphone was already open. Now `warmIceServers()` starts **before**
`getUserMedia` (the request returns inside its window for free) and again
**while the phone is ringing** at the receiving end, so that nothing has to go
over the network between "answer" and voice. An in-flight request is
deduplicated.

**A call on a dead socket went silently into a queue.** The check was `!ws`,
which let through a closed or reconnecting socket: the invitation landed in
`_outbox`, "Calling…" hung on the screen, and nothing rang for the other party
until the reconnect. The check is now `ws.readyState === 1`, and the refusal is
honest.

**`iceCandidatePoolSize: 1`** — the browser starts gathering ICE without
waiting for the first `createOffer`.

---

## What was deliberately left alone

- **No push channel for configuration was introduced.** Flags live in
  environment variables and change only on a service restart, and a restart *is*
  a socket break. A separate WS message type would duplicate a signal that
  already exists.
- **Backoff against a genuinely dead server was not softened.** From the second
  attempt it is unchanged: hammering an unavailable server every second is not
  acceptable.
- **Trickle ICE, the candidate buffer and the 45-second ring** were already
  right and were not touched.

---

## Tests

`tests/ui/uitest_realtime.js` — in a live browser, with the server's responses
supplied by a route (a real restart with a different environment cannot be
reproduced in a test, and to the client it is indistinguishable):

- enabling registration and SSO arrives **without a reload**;
- **disabling arrives too** — the main thing the rewrite was for (negative
  control: restore the one-directional application and the test fails on
  exactly these two lines);
- the "or" separator appears with the first method and disappears with the
  last;
- the application name updates;
- a repeat re-read with no changes **does not touch the DOM**;
- the page never reloaded once during the whole run;
- `onopen` re-reads the configuration; the old 3000 ms is not in the code;
- `startCall` checks that the socket is **open**, not merely that it exists;
- TURN credentials are fetched **before** `getUserMedia`.

`tests/ui/uitest_hostile.js` gained the second half of the queue invariant: a
message is not only marked with a clock and placed in the durable queue but is
also **sent on after the reconnect**. The test used to rely on the three-second
pause before the first reconnection and, once that pause was removed, started
catching an already-delivered message — that is, it was measuring the length of
the pause rather than the thing it was written for. Reconnection is now
disabled explicitly for the duration of the check.
