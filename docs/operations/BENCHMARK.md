# Resource consumption — measured figures

Every number below was **taken from a run**, not estimated. The method and the
raw conditions are stated honestly: if the test rig differs from yours, you
should be able to see that and recalculate.

To reproduce:
`python3 scripts/loadtest.py --server <URL> --steps 50,100,200 --mode ws`

---

## Test conditions

| Parameter | Value |
|---|---|
| Rig | 4 vCPU, 16 GB RAM, Linux container |
| Build | one Go binary, no Docker |
| Database | PostgreSQL 16, local, `fsync=off` |
| Database contents at measurement time | 557 000 messages, 304 MB |
| Transport | WebSocket, TLS terminated upstream (HTTP during the run) |

The caveat that matters more than the numbers: **`fsync=off` and the absence of
network latency make this rig more optimistic than a real VPS**. Treat
throughput as an upper bound; memory consumption and data size carry over
one to one.

---

## Memory

| State | Process RSS |
|---|---|
| Idle (0 connections) | **14 MB** |
| 200 concurrent WebSocket connections | **31 MB** |
| Peak over the whole run (including the 2000 msg/s spike) | **33 MB** |

The order of magnitude is tens of megabytes, not hundreds. That follows from
the architecture: one binary, no JVM, no message broker, no separate cache
layer. 200 connected users cost about 17 MB over idle, that is roughly **85 KB
per connection**.

PostgreSQL is counted separately and, at pilot volumes, fits inside the usual
256–512 MB of shared_buffers.

---

## Latency and loss

A realistic corporate rate: 200 connected users, 124 messages per second.

| Metric | Value |
|---|---|
| Connected | 200 / 200 |
| Delivered / sent | 1000 / 1000 |
| Lost | **0** |
| ACK p50 | 9 ms |
| ACK p95 | 16 ms |
| ACK p99 | **18 ms** |

For comparison, an artificial spike four times higher than any real office:

| Users | Rate | Loss | ACK p95 |
|---|---|---|---|
| 50 | 2057 msg/s | 0 | 78 ms |
| 100 | 2722 msg/s | 0 | 517 ms |
| 200 | 2075 msg/s | 0 | 1579 ms |

The key point: **there is no loss at any step**. Under load it is the
acknowledgement latency that grows, not the reliability of delivery — messages
queue and arrive. That is a direct consequence of the persistent outbox and
acknowledgement by `id`.

To put it in context: a 200-person company at 40 messages per employee per day
produces about **0.3 messages per second** on average and single digits at
peak. The measured ceiling is three orders of magnitude above that.

---

## CPU

Peak CPU during the run was 391% of 400% (4 cores), but that is the **spike of
200 users signing in at once**, not message traffic. Password hashing at
sign-in is deliberately expensive (bcrypt) — that is the brute-force
protection, and the cost belongs there. Steady traffic at 124 msg/s barely
touches the processor.

The practical planning consequence: budget CPU headroom not for traffic but for
**a mass simultaneous sign-in** — Monday morning, when the whole office opens
the application within the same minute.

---

## Disk space

Measured on the `messages` table with its indexes:

**≈ 555 bytes per message** (body + metadata + every index).

| Messages | Database size |
|---|---|
| 100 000 | ~55 MB |
| 1 000 000 | ~0.55 GB |
| 5 000 000 | ~2.8 GB |

A 200-person company at 40 messages a day accumulates about 2 million messages
a year — **on the order of 1.1 GB per year** in the database.

Files and voice messages are stored separately on disk and are counted by the
actual volume of attachments; they are not included in the figure above.

Retention is configurable (`docs/operations/RETENTION.md`), so growth of the
database is bounded from above by policy rather than by disk alone.

---

## What this means for a budget

A 50–200 person pilot fits in **2 vCPU / 4 GB RAM / 40 GB SSD**. That is the
bottom shelf of almost any hosting provider. At this profile the bottleneck is
neither the application nor the database but bandwidth and backups.

To check the claim on your own hardware: `scripts/bench.sh` and
`scripts/loadtest.py` are in the repository and need nothing beyond Python.

---

## What this measurement does NOT show

An honest list, so that you form no false expectations:

- **Group calls were not measured** — the product does not have them (1:1 over
  WebRTC goes peer-to-peer and does not load the server, apart from the TURN
  relay).
- **The TURN relay is not part of the measurement.** If calls go through a
  relay, bandwidth is counted separately: roughly 100 kbit/s for audio and up to
  1.5 Mbit/s for video in each direction.
- **Full-text search at large volumes** was measured separately and is not in
  the tables above.
- **The rig had no real network.** Clients connected locally; the round-trip
  time to actual workplaces will be added to every latency figure.
