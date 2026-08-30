# The link-preview cache

A preview (`/unfurl`) visits someone else's site, reads its Open Graph tags and
renders a card. The cache exists so that this trip happens rarely.

---

## What was broken

There had always been a cache — an in-memory map with a six-hour life. But the
quota was spent **before** it was consulted:

```go
unfurlLimiter.recordFailure(username)   // ← charged here
… cache hit? …                          // ← checked here
```

Forty links in an open conversation ate the entire quota (40 per person per
10 minutes) even when every one of them was already cached and went nowhere.
That is where `429` responses came from with nothing provoking them: the person
was not hammering anything, they had scrolled through a conversation containing
links.

Three more problems from the same place:

1. **Failures were not remembered at all.** A dead link meant a fresh trip
   outside on every render — and failures are the most expensive kind, five
   seconds of timeout each.
2. **The cache lived only in process memory.** After every deployment every link
   was fetched again — precisely when the quota is tightest.
3. **On overflow the map was cleared entirely.** The five hundred and first link
   threw away five hundred warm ones.

---

## What it is now

**The limit counts only real trips outside.** The cache is consulted first. The
limit exists so that we do not hammer other people's sites; something served
from cache goes nowhere and has no business spending quota. The quota itself is
unchanged — 40 per 10 minutes.

**Two layers.** An in-memory map (the fast path) and an `unfurl_cache` table,
which survives a restart and is shared by every worker process. A miss in
memory lifts the record from the database.

**Failures are remembered for 15 minutes** (successes for 6 hours). The shorter
life is deliberate: a site may have been down for five minutes rather than dead
forever. The client receives the same answer it would get from a real attempt —
"we went and it did not work" and "we remember that it does not work" are
indistinguishable to it.

**Identical requests are coalesced.** Ten people who see one link in a group
produce **one** trip outside. Without that, a group chat turns us into a source
of load on somebody else's site — and into the party that site blocks.

**Eviction discards the oldest half**, not everything.

**The key is normalised** cautiously: the case of the scheme and host, a default
port, an empty fragment, a trailing `/`. Query-parameter order is **not**
touched — for many sites it is significant, and "clever" normalisation would
merge distinct pages into one.

---

## Lifetimes and ceilings

| What | Value | Why |
|---|---|---|
| Success lifetime | 6 hours | A page title changes rarely |
| Failure lifetime | 15 minutes | The site may have been down for five minutes |
| Records in memory | 500 | The fast path; on overflow the older half goes |
| Records in the database | 20 000 | So that growth has a boundary |
| Waiting on someone else's fetch | 10 s | A waiter must not hang longer than fetching it themselves would take |

Stale entries are removed by the retention sweeper (`retention.go`) — the cache
was given no schedule of its own: that would be a second moving part for the
sake of a table that already limits itself.

---

## Deliberately out of scope

- **The cache is shared across users**, not partitioned per person. That is by
  design: the card for a public page is the same for everyone, and a per-user
  cache would mean N trips to the same site. There is no private data in a card
  — only what the site serves to an anonymous visitor.
- **No pre-warming.** A card appears when it is first requested, not when the
  message is sent.
- **The preview image is not proxied** — the browser fetches it itself
  (`img-src https:` in the CSP). This is deliberate: proxying images would mean
  a second SSRF path and noticeable traffic.

---

## Tests

`server/unfurlcache_test.go` (requires `TEST_DATABASE_URL`):

- **sixty consecutive cached requests against a quota of forty — not one
  `429`**; that is exactly the failure the rewrite was for (negative control:
  restore the old ordering and the test fails on request 41);
- a remembered failure is served from cache in under a second and likewise
  spends no quota — a real attempt would hit the five-second timeout;
- a record survives the loss of memory and is lifted back into it;
- a failure has a shorter life than a success, and that is checked rather than
  assumed;
- eviction keeps the fresh records;
- coalescing: the first request goes, the rest wait and are woken;
- the sweeper deletes what is stale and leaves what is fresh;
- key normalisation merges what should merge and does **not** merge differing
  parameter orders.
