#!/usr/bin/env python3
"""
Hexeris — a load test shaped around how the system actually works.

It measures what matters:

  1. Login       — how many users signed in, and why the rest did not.
  2. Connect     — how many WebSockets come up and how fast, at a controlled
                   connection rate rather than all at once.
  3. Throughput  — steady-state capacity, measured from the moment every
                   client is connected to the last ACK.
  4. HTTP        — latency of the heavy endpoints under concurrency.
  5. Storm       — separately: a deliberate simultaneous reconnect, the kind
                   that follows a proxy restart. A stress case, not a norm.

Why the connection rate is a parameter
──────────────────────────────────────
Opening all N connections in one unbounded gather means 500 concurrent TLS
handshakes plus several database queries per connect, which queues on the
connection pool: a "connect p95 = 36 s" then measures that queue rather than
the server's ceiling. Real users do not arrive that way — except in a
reconnect storm, which has its own phase. A controlled rate gives reproducible
numbers; use `--mode storm` for the storm.

Users: registration is rate-limited per IP, so accounts cannot be created in
bulk. The harness signs in to an existing pool instead. To seed users:
    python3 scripts/loadtest.py --print-seed-sql -n 1000 | psql "$DATABASE_URL"
(requires pip install bcrypt; the seed updates the password of existing
lt_ users, or an older pool with a different password silently fails to sign in)

Run:
    python3 scripts/loadtest.py --server https://<domain> -n 200 -m 20 --mode all
Steps (find the ceiling and capture the pool at peak):
    python3 scripts/loadtest.py --server https://<domain> --steps 200,500,1000 \
        -m 20 --admin-key "$ADMIN_KEY" --evidence

Requirements: python3 and aiohttp (pip install aiohttp).
"""

import argparse
import asyncio
import json
import statistics
import sys
import time
import uuid

try:
    import aiohttp
except ImportError:
    raise SystemExit("aiohttp is required:  pip install aiohttp")

USER_PREFIX = "lt_"
PASSWORD = "loadtest-pass-123"


# ── helpers ────────────────────────────────────────────────────────────────
def pct(xs, p):
    if not xs:
        return 0.0
    xs = sorted(xs)
    k = max(0, min(len(xs) - 1, int(round((p / 100.0) * (len(xs) - 1)))))
    return xs[k]


def summarize(name, lat):
    if not lat:
        print(f"  {name:<12} — no data")
        return
    print(f"  {name:<12} n={len(lat):<5} avg={statistics.mean(lat):6.0f}ms "
          f"p50={pct(lat,50):6.0f} p95={pct(lat,95):6.0f} "
          f"p99={pct(lat,99):6.0f} max={max(lat):6.0f}")


def ws_url(server):
    return server.replace("https://", "wss://").replace("http://", "ws://") + "/ws"


def describe_exc(e):
    """A human-readable reason for a failed connection.

    A bare class name is useless: ClientConnectorError wraps OSError, and
    `errno=24 Too many open files` (the client's own limit) versus
    `errno=111 Connection refused` (the server refusing) call for opposite
    actions, while both print identically without this.
    """
    name = type(e).__name__
    status = getattr(e, "status", None)
    if status is not None:
        return f"{name}(HTTP {status})"
    errno_ = getattr(e, "errno", None)
    strerr = getattr(e, "strerror", None)
    if errno_ is not None:
        return f"{name}(errno={errno_} {strerr or ''})"
    os_err = getattr(e, "os_error", None)
    if os_err is not None:
        return f"{name}(errno={os_err.errno} {os_err.strerror})"
    msg = str(e)[:50]
    return f"{name}({msg})" if msg else name


def open_fds():
    """How many descriptors this process has open (Linux); None if unknown."""
    try:
        import os
        return len(os.listdir("/proc/self/fd"))
    except Exception:
        return None


def user_names(n):
    return [f"{USER_PREFIX}{i:04d}" for i in range(n)]


def raise_fd_limit(needed):
    """Raise the soft descriptor limit to the hard one.

    The client needs roughly one descriptor per connection plus headroom.
    Merely advising `ulimit -n` made a run that hit the limit look like a
    server failure, so the limit is raised here and reported when short.
    """
    try:
        import resource
    except ImportError:
        return None  # not POSIX
    soft, hard = resource.getrlimit(resource.RLIMIT_NOFILE)
    want = needed + 256
    if soft >= want:
        return soft
    new = min(hard, max(want, soft))
    try:
        resource.setrlimit(resource.RLIMIT_NOFILE, (new, hard))
        soft = new
    except (ValueError, OSError):
        pass
    if soft < want:
        print(f"  [!] Descriptor limit {soft} is below the ~{want} needed. "
              f"Raise the hard limit: ulimit -n {want}")
    return soft


# ── auth ───────────────────────────────────────────────────────────────────
async def login(session, server, name):
    """Returns (token, reason); reason is set only on failure."""
    try:
        async with session.post(f"{server}/login",
                                json={"username": name, "password": PASSWORD}) as r:
            if r.status == 200:
                tok = (await r.json()).get("token")
                return (tok, None) if tok else (None, "200 without a token")
            body = (await r.text())[:60].replace("\n", " ")
            return None, f"HTTP {r.status}: {body}"
    except asyncio.TimeoutError:
        return None, "timeout"
    except Exception as e:
        return None, type(e).__name__


async def register(session, server, name):
    try:
        async with session.post(f"{server}/register",
                                json={"username": name, "password": PASSWORD}) as r:
            return r.status in (200, 201)
    except Exception:
        return False


async def get_tokens(session, server, names, do_register, concurrency, retries=2):
    """Signs the pool in, returning (tokens, reasons) with failures aggregated.

    A successful sign-in costs no rate-limit budget, since the limiter counts
    failures only, so a valid pool signs in as a batch. But bcrypt costs
    50–100 ms of CPU per sign-in, and too much concurrency saturates the
    server and produces timeouts that look like refusals — hence a moderate
    default and a retry with a pause.
    """
    tokens, reasons = {}, {}
    sem = asyncio.Semaphore(concurrency)
    aborted = asyncio.Event()   # set once the server starts answering 429

    async def one(name):
        if aborted.is_set():
            reasons["skipped (rate limiter already tripped)"] = \
                reasons.get("skipped (rate limiter already tripped)", 0) + 1
            return
        last = "?"
        for _ in range(retries + 1):
            async with sem:
                # Re-checking here, not only on entry, is essential: all N
                # coroutines start together and pass the outer check before
                # the first 429 arrives, then wait on the semaphore. Without
                # this the block burns the entire pool of requests.
                if aborted.is_set():
                    reasons["skipped (rate limiter already tripped)"] = \
                        reasons.get("skipped (rate limiter already tripped)", 0) + 1
                    return
                tok, why = await login(session, server, name)
            if tok:
                tokens[name] = tok
                return
            last = why or "?"
            if last.startswith("HTTP 429"):
                # The limiter counts failures and keys on the IP address, so
                # hammering on keeps the block alive and turns a local problem
                # (a couple of users with the wrong password) into a failure
                # of the whole pool. The sign-in phase stops entirely.
                aborted.set()
                break
            if last.startswith("HTTP 401") or last.startswith("HTTP 403"):
                # A wrong password or a blocked account will not change on a
                # retry, while each attempt brings the limiter closer.
                break
            await asyncio.sleep(0.4)   # network or timeout: worth retrying
        if do_register and not aborted.is_set() and await register(session, server, name):
            tok, why = await login(session, server, name)
            if tok:
                tokens[name] = tok
                return
            last = why or last
        reasons[last] = reasons.get(last, 0) + 1

    await asyncio.gather(*(one(n) for n in names))
    return tokens, reasons


# ── /admin/metrics snapshot (pool, goroutines, sockets) at peak ──────────────
async def fetch_metrics(session, url, key):
    if not url or not key:
        return None
    try:
        async with session.get(url, headers={"X-Admin-Key": key}) as r:
            if r.status == 200:
                return await r.json()
            return {"_error": f"HTTP {r.status}"}
    except Exception as e:
        return {"_error": type(e).__name__}


def snap(m):
    """A compact slice of /admin/metrics, or {'err':…} when unavailable."""
    if not m:
        return None
    if "_error" in m:
        return {"err": m["_error"]}
    dp = m.get("db_pool", {})
    mw = m.get("msg_writer", {}) or {}
    return {"wait_count": dp.get("wait_count"), "wait_ms": dp.get("wait_duration_ms"),
            "in_use": dp.get("in_use"), "open": dp.get("open"),
            "max_open": dp.get("max_open_allowed"),
            "goroutines": m.get("goroutines"), "conns": m.get("online_conns"),
            "save_timeouts": mw.get("save_timeouts"), "fast_fails": mw.get("fast_fails"),
            "panics": mw.get("panics"), "slow_drops": m.get("slow_client_drops")}


# ── WebSocket client: connect → barrier → send ───────────────────────────────
async def ws_client(session, server, name, token, peer, n_msgs, st, drain,
                    gate, connect_timeout, pace, inflight=None):
    """One client.

    After connecting, a client waits on a shared gate before sending.
    Otherwise the first arrivals send and get their ACKs while the last ones
    are still connecting, stretching the throughput window across the entire
    connection phase — which reported tens of msg/s alongside an honest ACK
    latency of 200 ms.
    """
    url = ws_url(server) + f"?token={token}"
    pending = {}
    ws = None
    try:
        t0 = time.monotonic()
        if inflight is not None:
            async with inflight:   # held only for the handshake
                ws = await asyncio.wait_for(
                    session.ws_connect(url, heartbeat=25), timeout=connect_timeout)
        else:
            ws = await asyncio.wait_for(
                session.ws_connect(url, heartbeat=25), timeout=connect_timeout)
        st["connect_ms"].append((time.monotonic() - t0) * 1000)
        st["connected"] += 1
    except asyncio.TimeoutError:
        st["connect_errors"] += 1
        st["errs"]["ConnectTimeout"] = st["errs"].get("ConnectTimeout", 0) + 1
        return
    except Exception as e:
        st["connect_errors"] += 1
        k = describe_exc(e)
        st["errs"][k] = st["errs"].get(k, 0) + 1
        return
    finally:
        st["settled"] += 1

    try:
        async def reader():
            async for msg in ws:
                if msg.type != aiohttp.WSMsgType.TEXT:
                    continue
                try:
                    m = json.loads(msg.data)
                except Exception:
                    continue
                if m.get("type") == "ack":
                    t = pending.pop(m.get("id"), None)
                    if t is None:
                        continue
                    now = time.monotonic()
                    st["last_ack"] = max(st["last_ack"], now)
                    if m.get("body") == "failed":
                        st["failed_acks"] += 1
                    else:
                        st["ack_ms"].append((now - t) * 1000)

        rtask = asyncio.create_task(reader())
        await gate.wait()          # everyone starts sending together
        for _ in range(n_msgs):
            mid = str(uuid.uuid4())
            pending[mid] = time.monotonic()
            await ws.send_json({"type": "message", "id": mid,
                                "from": name, "to": peer, "body": f"load {mid}"})
            st["sent"] += 1
            if pace:
                await asyncio.sleep(pace)
        deadline = time.monotonic() + drain
        while pending and time.monotonic() < deadline:
            await asyncio.sleep(0.2)
        st["lost"] += len(pending)
        rtask.cancel()
    except Exception as e:
        st["send_errors"] += 1
        k = "send:" + describe_exc(e)
        st["errs"][k] = st["errs"].get(k, 0) + 1
    finally:
        try:
            await ws.close()
        except Exception:
            pass


async def preflight(session, server, tokens, connect_timeout):
    """A few sequential WebSocket connections before the bulk run.

    This separates "the server or network refuses WebSockets at all" from
    "it does not hold up under load". If a single connection fails, running a
    thousand only fills the report with identical errors.
    """
    print("\n── Preflight: 5 single WebSocket connections ──")
    toks = (list(tokens.values()) * 5)[:5]
    ok = 0
    times = []
    for i, tok in enumerate(toks):
        t0 = time.monotonic()
        try:
            ws = await asyncio.wait_for(
                session.ws_connect(ws_url(server) + f"?token={tok}"),
                timeout=connect_timeout)
            dt = (time.monotonic() - t0) * 1000
            await ws.close()
            ok += 1
            times.append(dt)
            print(f"  #{i+1}: OK in {dt:.0f} ms")
        except Exception as e:
            print(f"  #{i+1}: FAILED — {describe_exc(e)}")
        await asyncio.sleep(0.3)

    # This is the signature of packet filtering rather than load. Spaced-out
    # single connections cannot strain the server, so a growing connect time
    # means dropped SYNs: the kernel retries with doubling pauses, and the
    # timings land on the 1 / 3 / 7 / 15 / 31 s retransmission grid — exactly
    # what a firewall DROP rule looks like.
    if len(times) >= 3 and times[-1] > 800 and times[-1] > times[0] * 4:
        print()
        print("  ⚠ SIGNS OF PACKET FILTERING rather than server overload:")
        print(f"     single connections slow from {times[0]:.0f} to {times[-1]:.0f} ms")
        print("     without any load, and the timings match the SYN retransmission")
        print("     grid (1 / 3 / 7 / 15 / 31 s) — packets are being dropped.")
        print("     The usual cause is an intrusion-prevention or firewall rule")
        print("     treating the test as an attack (many connections, one IP).")
        print("     Check on the server:")
        print("       fail2ban-client status              # active jails")
        print("       fail2ban-client status <jail>       # is your IP listed")
        print("       iptables -L -n -v | grep -iE 'drop|reject'")
        print("     Add the test machine to the ignore list and restart the")
        print("     service, or the filter is what gets measured.")
    if ok == 0:
        print("\n  STOP: not even a single WebSocket connection comes up.")
        print("  This is not a load ceiling — the path to /ws is broken. Check:")
        print("    • the proxy forwards Upgrade/Connection for /ws;")
        print("    • the certificate is valid (or run with --insecure);")
        print("    • the URL uses https:// and has no trailing slash.")
        return False
    if ok < len(toks):
        print(f"  [!] Only {ok}/{len(toks)} came up — /ws is unstable before any load.")
    return True


async def phase_probe(session, server, tokens, connect_timeout):
    """Finds the rate at which packet filtering starts.

    It opens ten connections at increasing rates and watches for connect times
    jumping onto the SYN retransmission grid (1 / 3 / 7 / 15 / 31 s). Use it to
    confirm the path is clear after allow-listing the test machine: load
    numbers only mean something when no filter interferes. The pause between
    steps lets limiter buckets drain, or the previous step is what gets
    measured.
    """
    print("\n── Probe: finding the rate at which filtering begins ──")
    print(f"  {'rate':>8} {'ok':>7} {'p50':>8} {'p95':>8} {'max':>8}  errors")
    toks = list(tokens.values())
    clean_upto = None
    base_p50 = None
    for rate in (1, 2, 3, 5, 10, 25, 50, 100):
        n = 10
        lat, errs = [], {}

        async def one(i):
            await asyncio.sleep(i / rate)
            t0 = time.monotonic()
            try:
                ws = await asyncio.wait_for(
                    session.ws_connect(ws_url(server) + f"?token={toks[i % len(toks)]}"),
                    timeout=connect_timeout)
                lat.append((time.monotonic() - t0) * 1000)
                await ws.close()
            except Exception as e:
                k = describe_exc(e)
                errs[k] = errs.get(k, 0) + 1

        await asyncio.gather(*(one(i) for i in range(n)))
        tail = f"  {errs}" if errs else ""
        print(f"  {rate:>6}/s {len(lat):>4}/{n} {pct(lat,50):>8.0f} "
              f"{pct(lat,95):>8.0f} {max(lat) if lat else 0:>8.0f}{tail}")
        if rate == 1:
            base_p50 = pct(lat, 50)
        # The threshold is relative to the baseline rather than absolute: on
        # a link with 250 ms RTT a healthy p95 is far from zero, and a fixed
        # 800 ms would raise a false alarm.
        limit_ms = max(800.0, (base_p50 or 0) * 3 + 300)
        if len(lat) == n and pct(lat, 95) < limit_ms:
            clean_upto = rate
        else:
            break
        await asyncio.sleep(5)

    print()
    if base_p50:
        print(f"  Baseline (1/s): p50 {base_p50:.0f} ms — the round trip to the server")
        print("  plus TLS. Filtering shows up as p95 growth at higher rates, not here.")
    if clean_upto is None:
        print("  VERDICT: filtering starts at one connection per second.")
        print("  Load numbers are meaningless until the path is fixed.")
    elif clean_upto >= 100:
        print("  VERDICT: the path is clear up to 100 connections/s.")
    else:
        print(f"  VERDICT: clear up to {clean_upto}/s; beyond that packets drop.")
        print("  Allow-list this machine's PUBLIC address, not its interface")
        print("  address: behind NAT they differ, and allow-listing a private")
        print("  address does nothing. Check the proxy's connection and request")
        print("  limits as well, plus any SYN rate limits in the firewall —")
        print("  a threshold of a few connections per second is usually one of those.")
        print("  Alternatively run with --connect-rate <threshold>,")
        print(f"  for example --connect-rate {clean_upto} — but then the connection")
        print("  phase stretches out and the numbers are not the server's ceiling.")
    return clean_upto


async def sample_metrics(session, url, key, out, stop, period=0.5):
    """Samples /admin/metrics while the connection phase runs.

    Before-and-after snapshots do not show what happens to the pool during the
    surge. A rising open count with zero waits means the pool is expanding —
    the cost of establishing Postgres connections, which the first surge after
    a restart always pays. A high wait count with open == max_open is the
    opposite: pool exhaustion. The two call for different fixes.
    """
    if not url or not key:
        return
    while not stop.is_set():
        m = snap(await fetch_metrics(session, url, key))
        if m and "err" not in m:
            out.append(m)
        try:
            await asyncio.wait_for(stop.wait(), timeout=period)
        except asyncio.TimeoutError:
            pass


async def phase_ws(session, server, tokens, n_msgs, drain, connect_rate,
                   connect_timeout, pace, storm=False, connect_inflight=0,
                   admin_url=None, admin_key=None):
    n = len(tokens)
    how = "STORM (all at once)" if storm else f"rate {connect_rate}/s"
    print(f"\n── WS: connect + throughput/ACK ({n} users × {n_msgs} messages, {how}) ──")
    st = {"connected": 0, "connect_errors": 0, "send_errors": 0, "sent": 0,
          "failed_acks": 0, "lost": 0, "settled": 0,
          "connect_ms": [], "ack_ms": [], "errs": {}, "last_ack": 0.0}
    ring = list(tokens.keys())
    gate = asyncio.Event()

    # The rate controls how often a connection starts, not how many are in
    # flight: when a handshake takes seconds, hundreds pile up. A separate cap
    # keeps the connection phase manageable.
    inflight = asyncio.Semaphore(connect_inflight) if connect_inflight else None

    async def spawn(i, name):
        # Spread connections over time at connect_rate per second.
        if not storm and connect_rate > 0:
            await asyncio.sleep(i / connect_rate)
        await ws_client(session, server, name, tokens[name],
                        ring[(i + 1) % len(ring)], n_msgs, st, drain, gate,
                        connect_timeout, pace, inflight)

    samples, stop_sampling = [], asyncio.Event()
    sampler = asyncio.create_task(
        sample_metrics(session, admin_url, admin_key, samples, stop_sampling))

    tasks = [asyncio.create_task(spawn(i, nm)) for i, nm in enumerate(ring)]

    # Barrier: wait until every connection attempt resolves, either way.
    ramp = 0 if storm else n / max(connect_rate, 1)
    barrier_deadline = time.monotonic() + ramp + connect_timeout + 5
    while st["settled"] < n and time.monotonic() < barrier_deadline:
        await asyncio.sleep(0.1)
    connect_done = time.monotonic()
    gate.set()

    await asyncio.gather(*tasks, return_exceptions=True)
    stop_sampling.set()
    await sampler

    acked = len(st["ack_ms"])
    print(f"  connected     {st['connected']}/{n}  (errors {st['connect_errors']})")
    if st["errs"]:
        print(f"  errors        {st['errs']}")
        joined = " ".join(st["errs"])
        fds = open_fds()
        if "errno=24" in joined or "errno=23" in joined:
            print(f"  DIAGNOSIS: hit the client's descriptor limit "
                  f"({fds} open). Raise the hard limit: ulimit -n 65535")
        elif "errno=111" in joined:
            print("  DIAGNOSIS: connection refused — the proxy or server is "
                  "refusing (worker_connections / listen backlog), not the app.")
        elif "errno=99" in joined or "errno=98" in joined:
            print("  DIAGNOSIS: local ports exhausted on the client. "
                  "Run from several machines or lower the step.")
        elif "errno=110" in joined or "ConnectTimeout" in joined:
            print("  DIAGNOSIS: packets leave and nothing answers — a queue on "
                  "the server, packet filtering, or loss along the path.")
        elif "ClientConnectorError" in joined:
            print(f"  DIAGNOSIS: at the TCP level, not the application. Descriptors "
                  f"open: {fds}. See the errno above.")
    summarize("connect", st["connect_ms"])
    print(f"  sent={st['sent']}  acked={acked}  "
          f"lost(true)={st['lost']}  failed_acks={st['failed_acks']}")
    # The window runs from the barrier release (everyone connected) to the
    # last ACK. The connection phase is excluded, or this measures connection
    # speed rather than throughput.
    window = st["last_ack"] - connect_done
    thr = (acked / window) if window > 0 else 0.0
    if window > 0:
        print(f"  throughput    {thr:.1f} msg/s (send window {window:.1f}s, "
              f"excluding the connection phase)")
    summarize("ack_latency", st["ack_ms"])
    if samples:
        peak_open = max(x.get("open") or 0 for x in samples)
        peak_use = max(x.get("in_use") or 0 for x in samples)
        max_open = samples[-1].get("max_open") or 0
        w0 = samples[0].get("wait_count") or 0
        w1 = samples[-1].get("wait_count") or 0
        peak_gor = max(x.get("goroutines") or 0 for x in samples)
        print(f"  database pool during the phase: open peak {peak_open}/{max_open}, "
              f"in_use peak {peak_use}, wait +{w1 - w0}, goroutines peak {peak_gor}")
        # Exhaustion means the pool hit its ceiling, not merely that some
        # waits occurred. Comparing a cumulative wait count with an
        # instantaneous peak declares "pool exhausted" at open 25/50 with
        # in_use 5 — a pool at half its limit.
        waits = w1 - w0
        if max_open and peak_open >= max_open * 0.95 and waits > 0:
            print(f"  → pool EXHAUSTED: {peak_open} open against a ceiling of "
                  f"{max_open} with {waits} waits. Raise DB_MAX_OPEN_CONNS or make")
            print("    the queries on the connect path cheaper.")
        elif waits > 0:
            print(f"  → the pool stayed below its ceiling ({peak_open}/{max_open}) "
                  f"but saw {waits} waits:")
            print("    short spikes between samples (sampled every 0.5 s).")
            print("    Worth attention only if it grows with ACK latency.")
        else:
            print("  → the pool is calm: no waits and the ceiling untouched.")
    return {
        "n": n, "connected": st["connected"], "connect_errs": st["connect_errors"],
        "sent": st["sent"], "acked": acked, "lost": st["lost"],
        "failed_acks": st["failed_acks"], "throughput": thr,
        "ack_p50": pct(st["ack_ms"], 50), "ack_p95": pct(st["ack_ms"], 95),
        "ack_p99": pct(st["ack_ms"], 99),
        "connect_p50": pct(st["connect_ms"], 50),
        "connect_p95": pct(st["connect_ms"], 95),
        "errs": st["errs"],
    }


# ── HTTP under concurrency ─────────────────────────────────────────────────
async def phase_http(session, server, tokens, rounds=10, concurrency=50):
    """Latency of the heavy endpoints under bounded concurrency.

    Gathering over every token at once — 200 requests in one millisecond, ten
    times over — is the same unbounded storm as the WebSocket phase, only over
    HTTP: it measures what happens when the whole pool arrives together rather
    than an endpoint's latency. On a modest host that produced 6–10 s averages
    even for a trivial /status whose real work takes milliseconds. Bounding it
    makes the numbers comparable between runs.
    """
    print(f"\n── HTTP endpoints under concurrency "
          f"({len(tokens)} workers × {rounds} rounds, {concurrency} at a time) ──")
    sem = asyncio.Semaphore(concurrency) if concurrency else None
    a_user = next(iter(tokens))
    endpoints = [("/history", "?since=0&limit=50"), ("/groups", ""),
                 ("/search", "?q=test"), ("/reactions", "?since=0"),
                 ("/status", f"?user={a_user}")]
    for ep, qs in endpoints:
        lat, errs = [], {}

        async def one(tok):
            if sem is not None:
                await sem.acquire()
            t0 = time.monotonic()
            try:
                async with session.get(f"{server}{ep}{qs}",
                                       headers={"Authorization": f"Bearer {tok}"}) as r:
                    await r.read()
                    if r.status >= 400:
                        errs[f"HTTP {r.status}"] = errs.get(f"HTTP {r.status}", 0) + 1
                    else:
                        lat.append((time.monotonic() - t0) * 1000)
            except Exception as e:
                k = describe_exc(e)
                errs[k] = errs.get(k, 0) + 1
            finally:
                if sem is not None:
                    sem.release()

        for _ in range(rounds):
            await asyncio.gather(*(one(t) for t in tokens.values()))
        tail = f" errors={errs}" if errs else ""
        if lat:
            print(f"  {ep:<12} n={len(lat):<5} avg={statistics.mean(lat):6.0f}ms "
                  f"p95={pct(lat,95):6.0f}ms{tail}")
        else:
            print(f"  {ep:<12} no successful responses{tail}")


# ── reconnect storm ────────────────────────────────────────────────────────
async def phase_reconnect(session, server, tokens, connect_timeout):
    print(f"\n── Reconnect storm ({len(tokens)} simultaneous) ──")
    errs = {}

    async def reconnect(tok):
        try:
            ws = await asyncio.wait_for(
                session.ws_connect(ws_url(server) + f"?token={tok}"),
                timeout=connect_timeout)
            await ws.close()
            return True
        except asyncio.TimeoutError:
            errs["ConnectTimeout"] = errs.get("ConnectTimeout", 0) + 1
            return False
        except Exception as e:
            # errno=24 (EMFILE) is the client's ceiling; 111 refused or an
            # HTTP 503 means the server or proxy is shedding the surge.
            k = describe_exc(e)
            errs[k] = errs.get(k, 0) + 1
            return False

    t0 = time.monotonic()
    results = await asyncio.gather(*(reconnect(t) for t in tokens.values()))
    dur = time.monotonic() - t0
    ok = sum(1 for x in results if x)
    print(f"  successful {ok}/{len(results)}  failed {len(results)-ok}  in {dur:.2f}s")
    if errs:
        print(f"  errors: {errs}")


# ── seed SQL ───────────────────────────────────────────────────────────────
def print_seed_sql(n):
    try:
        import bcrypt
    except ImportError:
        raise SystemExit("--print-seed-sql requires bcrypt:  pip install bcrypt")
    h = bcrypt.hashpw(PASSWORD.encode(), bcrypt.gensalt(rounds=10)).decode()
    print(f"-- Seeds {n} load-test users (password: {PASSWORD}).")
    print("-- Staging and test databases only.")
    print("-- DO UPDATE rather than DO NOTHING: an existing lt_ user with an old")
    print("-- password would silently fail to sign in and drop out of the test")
    print("-- with no explanation. It also clears the blocked flag.")
    print("INSERT INTO users (username, password_hash) VALUES")
    rows = ",\n".join(f"  ('{name}', '{h}')" for name in user_names(n))
    print(rows)
    print("ON CONFLICT (username) DO UPDATE SET password_hash = EXCLUDED.password_hash;")
    # An anchored regular expression rather than LIKE, where `_` is a single
    # wildcard and 'lt_%' would also match a real user such as 'ltAlex'. The
    # same convention as in
    # scripts/cleanup-loadtest.sql.
    print(f"UPDATE users SET blocked = FALSE WHERE username ~ '^{USER_PREFIX}[0-9]+$';")


# ── Summary ────────────────────────────────────────────────────────────────
def _wait_gor(r):
    """(db.wait delta, goroutines) from a step's metric snapshots."""
    b, a = r.get("metrics_before"), r.get("metrics_after")
    if not a:
        return "n/a", "n/a"
    if "err" in a:
        return a["err"], "n/a"
    gor = str(a.get("goroutines"))
    if b and "err" not in b and a.get("wait_count") is not None and b.get("wait_count") is not None:
        return str(a["wait_count"] - b["wait_count"]), gor
    if a.get("wait_count") is not None:
        return str(a["wait_count"]), gor
    return "n/a", gor


def print_scale_summary(rows, args):
    if not rows:
        return
    print("\n" + "═" * 92)
    print("SCALE — load steps (finding the ceiling and the degradation point)")
    print("═" * 92)
    print(f"{'N':>5} {'conn':>10} {'acked/sent':>13} {'loss':>5} {'thr/s':>7} "
          f"{'conn p50/p95':>14} {'ACK p50/p95/p99':>18} {'db.wait Δ':>10} {'gor':>5}")
    print("─" * 92)
    for r in rows:
        dwait, gor = _wait_gor(r)
        ack = f"{r['ack_p50']:.0f}/{r['ack_p95']:.0f}/{r['ack_p99']:.0f}"
        cn = f"{r['connect_p50']:.0f}/{r['connect_p95']:.0f}"
        conn = f"{r['connected']}/{r['n']}"
        assent = f"{r['acked']}/{r['sent']}"
        print(f"{r['n']:>5} {conn:>10} {assent:>13} {r['lost']:>5} {r['throughput']:>7.0f} "
              f"{cn:>14} {ack:>18} {dwait:>10} {gor:>5}")
    print("─" * 92)
    print("The ceiling is where connected < N, loss > 0, ACK latency jumps")
    print("or db.wait spikes. Growth in connect time alone, with healthy ACK")
    print("latency, means an expensive connect path rather than a messaging ceiling.")

    if args.evidence:
        from datetime import date
        print("\n── markdown summary ──\n")
        print(f"**Scale test** (`scripts/loadtest.py`, {date.today()}, off-host, "
              f"N×{args.messages} messages, connect rate {args.connect_rate}/s):\n")
        print("| N concurrent | Connects | acked/sent | Loss | Throughput | "
              "connect p50/p95 (ms) | ACK p50/p95/p99 (ms) | db_pool.wait Δ | goroutines |")
        print("|---|---|---|---|---|---|---|---|---|")
        for r in rows:
            dwait, gor = _wait_gor(r)
            dwait = "—" if dwait == "n/a" else dwait
            gor = "—" if gor == "n/a" else gor
            ack = f"{r['ack_p50']:.0f}/{r['ack_p95']:.0f}/{r['ack_p99']:.0f}"
            cn = f"{r['connect_p50']:.0f}/{r['connect_p95']:.0f}"
            print(f"| {r['n']} | {r['connected']}/{r['n']} | {r['acked']}/{r['sent']} | "
                  f"{r['lost']} | {r['throughput']:.0f}/s | {cn} | {ack} | {dwait} | {gor} |")


async def run(args):
    # A trailing slash in --server produces "//login" → 301 → aiohttp turns
    # the POST into a GET → no tokens. Normalise it once, here.
    args.server = args.server.rstrip("/")
    steps = sorted(args.steps or [args.users])
    max_n = max(steps)
    names = user_names(max_n)
    timeout = aiohttp.ClientTimeout(total=args.http_timeout)
    # A longer DNS cache: the test targets one host, and thousands of repeated
    # lookups (especially against dynamic-DNS names) start failing on their
    # own, arriving as a ClientConnectorError indistinguishable from a server
    # refusal.
    conn = aiohttp.TCPConnector(limit=0, ssl=(not args.insecure),
                                use_dns_cache=True, ttl_dns_cache=600)
    admin_url = args.admin_url or (args.server + "/admin/metrics")

    print("METHOD: run this from a separate machine, not the server itself,")
    print("  or the test competes with it for CPU and descriptors.")
    fd = raise_fd_limit(max_n * 4 + 1024)
    if fd:
        print(f"  Descriptor limit: {fd}")
    print(f"  Steps: {steps}   connect rate: "
          f"{'storm' if args.mode == 'storm' else str(args.connect_rate) + '/s'}\n")

    # The sign-in phase runs in its own session and closes before the
    # WebSocket phase. Otherwise its keepalive connections — hundreds of
    # sockets after a thousand sign-ins — stay in the pool and consume the
    # descriptor limit, so the next phase hits it on the first step and looks
    # like a server failure.
    print(f"Signing in a pool of {len(names)} users ({args.login_concurrency} at a time)…")
    t0 = time.monotonic()
    # A trial sign-in with one user. If the path is already closed (limiter,
    # wrong password, unreachable server) the remaining N-1 requests add
    # nothing to the diagnosis and only lengthen the output.
    probe_conn = aiohttp.TCPConnector(limit=4, ssl=(not args.insecure),
                                      use_dns_cache=True, ttl_dns_cache=600)
    async with aiohttp.ClientSession(timeout=timeout, connector=probe_conn) as ps:
        _tok, _why = await login(ps, args.server, names[0])
    if _why and _why.startswith("HTTP 429"):
        raise SystemExit(
            f"\n  STOP: /login already answers 429 for this address ({_why}).\n"
            "  The limiter counts failed sign-ins, keys on the IP and holds a\n"
            "  10-minute window from the last failure. A blocked address gets\n"
            "  429 immediately without recording a new failure, so the block\n"
            "  does not extend itself.\n"
            "  Either wait ten minutes or restart the service, since the\n"
            "  limiter lives in process memory.")
    if _why and (_why.startswith("HTTP 401") or _why.startswith("HTTP 403")):
        raise SystemExit(
            f"\n  STOP: the trial sign-in for {names[0]} was rejected ({_why}).\n"
            "  The pool was seeded with a different password, or the accounts\n"
            "  are blocked.\n"
            f"  Re-seed: python3 {sys.argv[0]} --print-seed-sql -n {max_n} | psql \"$DATABASE_URL\"\n"
            "  Make sure psql points at the same database as the service.")
    login_conn = aiohttp.TCPConnector(limit=args.login_concurrency * 2,
                                      ssl=(not args.insecure),
                                      use_dns_cache=True, ttl_dns_cache=600)
    async with aiohttp.ClientSession(timeout=timeout, connector=login_conn) as ls:
        tokens_all, reasons = await get_tokens(
            ls, args.server, names, args.register, args.login_concurrency)
    await asyncio.sleep(0.3)   # let the kernel close the sockets
    fd_after_login = open_fds()

    async with aiohttp.ClientSession(timeout=timeout, connector=conn) as session:
        print(f"  tokens obtained: {len(tokens_all)}/{len(names)} in {time.monotonic()-t0:.1f}s")
        if fd_after_login is not None:
            print(f"  descriptors open after sign-in: {fd_after_login}")
        if reasons:
            # Swallowing the reasons leaves a bare "944/1000" unexplained.
            print("  sign-in failures by reason:")
            for why, cnt in sorted(reasons.items(), key=lambda kv: -kv[1]):
                print(f"    {cnt:>5} × {why}")
            joined = " ".join(reasons)
            if "429" in joined or "rate limiter" in joined:
                print()
                print("  DIAGNOSIS: the sign-in limiter tripped. It counts failures and")
                print("  keys on the IP (five per ten minutes), so a few pool users with")
                print("  the wrong password block the entire pool from this machine and")
                print("  everyone else starts failing for no visible reason.")
                print("  Either re-seed the pool (the seed below rewrites passwords)")
                print("  or wait ten minutes, or restart the service — the limiter")
                print("  lives in process memory.")
            if "401" in joined:
                print()
                print("  DIAGNOSIS: part of the pool was created with a different")
                print("  password. Re-seed it; the seed performs DO UPDATE:")
            if "timeout" in joined:
                print()
                print("  DIAGNOSIS: sign-in timeouts. bcrypt costs 50–100 ms of CPU per")
                print("  sign-in and saturates the server at high concurrency.")
                print("  Lower --login-concurrency (currently "
                      f"{args.login_concurrency}) or raise --http-timeout.")
            print(f"  Seed: python3 {sys.argv[0]} --print-seed-sql -n {max_n} | psql \"$DATABASE_URL\"")
        if len(tokens_all) < 2:
            raise SystemExit(
                "Not enough users. Seed them (on a staging database):\n"
                f"  python3 {sys.argv[0]} --print-seed-sql -n {max_n} | psql \"$DATABASE_URL\"")

        ordered = list(tokens_all.items())
        if args.admin_key:
            probe = snap(await fetch_metrics(session, admin_url, args.admin_key))
            if probe and "err" in probe:
                print(f"  [!] /admin/metrics is unavailable ({probe['err']}) — pool metrics "
                      "will be missing (needs a valid --admin-key and an allow-listed IP).")

        if args.mode == "probe":
            await phase_probe(session, args.server, tokens_all, args.connect_timeout)
            print("\nDone.")
            return

        rows = []
        if args.mode in ("all", "ws", "storm"):
            if not await preflight(session, args.server, tokens_all, args.connect_timeout):
                raise SystemExit("Preflight failed — the bulk run is cancelled.")
            for k in steps:
                if k > len(ordered):
                    print(f"\n[!] Step {k}: only {len(ordered)} tokens available — "
                          f"running with {len(ordered)}.")
                    k = len(ordered)
                sub = dict(ordered[:k])
                before = snap(await fetch_metrics(session, admin_url, args.admin_key))
                res = await phase_ws(session, args.server, sub, args.messages, args.drain,
                                     args.connect_rate, args.connect_timeout, args.pace,
                                     storm=(args.mode == "storm"),
                                     connect_inflight=args.connect_inflight,
                                     admin_url=admin_url, admin_key=args.admin_key)
                after = snap(await fetch_metrics(session, admin_url, args.admin_key))
                res["metrics_before"], res["metrics_after"] = before, after
                if after and "err" not in after:
                    bad = {kk: after.get(kk) for kk in
                           ("save_timeouts", "fast_fails", "panics", "slow_drops")
                           if after.get(kk)}
                    if bad:
                        print(f"  [!] server-side failure counters: {bad}")
                rows.append(res)
                await asyncio.sleep(args.cooldown)

        top = dict(ordered[:min(max_n, len(ordered))])
        if args.mode in ("all", "http"):
            hb = snap(await fetch_metrics(session, admin_url, args.admin_key))
            await phase_http(session, args.server, top, concurrency=args.http_concurrency)
            ha = snap(await fetch_metrics(session, admin_url, args.admin_key))
            if hb and ha and "err" not in hb and "err" not in ha:
                dw = (ha.get("wait_count") or 0) - (hb.get("wait_count") or 0)
                dms = (ha.get("wait_ms") or 0) - (hb.get("wait_ms") or 0)
                print(f"  during the phase: db_pool.wait +{dw} ({dms} ms), goroutines {ha.get('goroutines')}")
        if args.mode in ("all", "reconnect"):
            await phase_reconnect(session, args.server, top, args.connect_timeout)

        print_scale_summary(rows, args)
    print("\nDone.")


def main():
    p = argparse.ArgumentParser(description="Hexeris WS/HTTP load test")
    p.add_argument("--server", default="http://localhost",
                   help="base URL of the deployment under test")
    p.add_argument("-n", "--users", type=int, default=50)
    p.add_argument("--steps", type=lambda s: [int(x) for x in s.split(",") if x.strip()],
                   default=None, help="concurrency steps, e.g. 200,500,1000 (otherwise a single -n)")
    p.add_argument("-m", "--messages", type=int, default=20, help="messages per user over WebSocket")
    p.add_argument("--drain", type=int, default=25,
                   help="seconds to wait for ACKs after sending (>= real ACK latency)")
    p.add_argument("--mode", choices=["all", "ws", "http", "reconnect", "storm", "probe"], default="all",
                   help="probe = find the filtering threshold; storm = all at once; ws = controlled rate")
    p.add_argument("--connect-rate", type=float, default=50.0,
                   help="connections per second during the WebSocket phase (0 = unlimited)")
    p.add_argument("--connect-timeout", type=float, default=30.0,
                   help="seconds allowed for one WebSocket connection")
    p.add_argument("--connect-inflight", type=int, default=100,
                   help="maximum concurrent handshakes (0 = unlimited)")
    p.add_argument("--http-timeout", type=float, default=60.0, help="seconds allowed for one HTTP request")
    p.add_argument("--login-concurrency", type=int, default=25,
                   help="concurrent sign-ins (bcrypt saturates the server's CPU)")
    p.add_argument("--http-concurrency", type=int, default=50,
                   help="concurrent HTTP requests during the endpoint phase (0 = unlimited)")
    p.add_argument("--pace", type=float, default=0.01,
                   help="pause between one client's messages, seconds (0 = burst)")
    p.add_argument("--cooldown", type=float, default=3.0, help="pause between steps, seconds")
    p.add_argument("--admin-key", default=None,
                   help="X-Admin-Key, to sample /admin/metrics at peak")
    p.add_argument("--admin-url", default=None,
                   help="metrics URL (defaults to <server>/admin/metrics)")
    p.add_argument("--evidence", action="store_true", help="print a markdown table of the steps")
    p.add_argument("--register", action="store_true",
                   help="register missing users (small N only: the per-IP limit applies)")
    p.add_argument("--insecure", action="store_true", help="skip TLS certificate verification")
    p.add_argument("--print-seed-sql", action="store_true",
                   help="print the SQL that seeds users, then exit")
    args = p.parse_args()
    if args.print_seed_sql:
        print_seed_sql(args.users)
        return
    asyncio.run(run(args))


if __name__ == "__main__":
    main()
