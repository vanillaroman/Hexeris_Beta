#!/usr/bin/env python3
"""
Hexeris chaos drill — a reproducible reliability check against a live stack.

It answers "what happens when a component fails?" for all four parts of the
production topology: the app, the reverse proxy, PostgreSQL and coturn. The
core is always the same test — no message loss under failure. A sender sends N
messages with stable ids, the script kills or restarts a component mid-run, the
sender reconnects and resends whatever was not acknowledged (like a real
outbox), and the server is idempotent by id. At the end all N must be
acknowledged to the sender and received by the recipient (unique ids == N).

Scenarios:
  app     — restart the application under traffic → redelivery, no loss.
  nginx   — restart the proxy, dropping every socket → clients reconnect.
  db      — stop and start the database: /healthz must report 200→503→200 and
            messages sent during the outage must arrive afterwards.
  coturn  — stop and start the relay: messaging is unaffected (isolation; it
            only matters for WebRTC calls behind NAT).
  healthz — a quick standalone 200→503→200 probe with no traffic.

Run it on the host where the stack lives. The default target is the
development stack at http://localhost:
  pip install aiohttp
  python3 scripts/chaos-drill.py                      # every scenario, dev
  python3 scripts/chaos-drill.py --scenario app       # only the app restart
  python3 scripts/chaos-drill.py --evidence           # plus a markdown summary
  # against the production stack:
  python3 scripts/chaos-drill.py \
      --compose "docker compose -f docker-compose.prod.yml" \
      --server https://your-domain
"""

import argparse, asyncio, json, subprocess, time, urllib.request, uuid
try:
    import aiohttp
except ImportError:
    raise SystemExit("aiohttp is required:  pip install aiohttp")

PW = "chaos-drill-pass-123"
RESULTS = []   # [(scenario, ok, detail)]


def ws_url(server):  # http->ws, https->wss
    return server.replace("http", "ws", 1) + "/ws"


SVC_MISSING = set()   # services absent from the stack (no failure injected)


def compose(cmd, *args):
    return subprocess.run(cmd.split() + list(args), capture_output=True, text=True)


def _missing(r, svc):
    if r is not None and "no such service" in ((r.stderr or "") + (r.stdout or "")).lower():
        SVC_MISSING.add(svc)
        return True
    return False


def http_code(url):
    try:
        return urllib.request.urlopen(url, timeout=5).status
    except urllib.error.HTTPError as e:
        return e.code
    except Exception:
        return 0


async def _try_login(session, server, name):
    try:
        r = await session.post(f"{server}/login", json={"username": name, "password": PW})
        if r.status == 200:
            return (await r.json()).get("token"), None
        return None, f"login HTTP {r.status}"
    except Exception as e:
        return None, f"login err {e.__class__.__name__}"


async def reg_login(session, server, name):
    # Sign in first: a user left over from a previous run costs no
    # registration attempts, and the per-IP registration limit is exhausted by
    # the second run otherwise. Registration happens only if sign-in fails.
    tok, _ = await _try_login(session, server, name)
    if tok:
        return tok, None
    try:
        rr = await session.post(f"{server}/register", json={"username": name, "password": PW})
        reg_status = rr.status
    except Exception as e:
        return None, f"register err {e.__class__.__name__}"
    tok, err = await _try_login(session, server, name)
    if tok:
        return tok, None
    return None, f"register HTTP {reg_status}; {err}"


# ── Traffic: a sender with outbox semantics, and a recipient ──────────────────
async def sender(session, server, token, me, peer, ids, acked, deadline, diag=None):
    # Only ACKs for this scenario's ids are counted. The users are the same
    # across scenarios and the socket is reopened, so a tail of ACKs from the
    # previous scenario would otherwise land in the counter — reporting "63 of
    # 60 acknowledged" and, worse, ending the send loop early and masking a
    # real loss.
    mine = set(ids)
    while time.monotonic() < deadline and len(acked) < len(ids):
        try:
            async with session.ws_connect(ws_url(server) + f"?token={token}", heartbeat=20) as ws:
                if diag is not None:
                    diag["sender_connects"] = diag.get("sender_connects", 0) + 1

                async def reader():
                    async for m in ws:
                        if m.type == aiohttp.WSMsgType.TEXT:
                            d = json.loads(m.data)
                            if d.get("type") == "ack" and d.get("body") == "failed" \
                                    and diag is not None:
                                diag["ack_failed"] = diag.get("ack_failed", 0) + 1
                            if d.get("type") == "ack" and d.get("body") != "failed" \
                                    and d.get("id") in mine:
                                acked.add(d.get("id"))
                rt = asyncio.create_task(reader())
                for mid in ids:                       # resend anything unacknowledged
                    if mid in acked:
                        continue
                    await ws.send_json({"type": "message", "id": mid,
                                        "from": me, "to": peer, "body": "drill"})
                    await asyncio.sleep(0.08)
                t = time.monotonic()
                while time.monotonic() - t < 4 and len(acked) < len(ids) and not ws.closed:
                    await asyncio.sleep(0.2)
                rt.cancel()
        except Exception as e:
            # Record the error type. A silent `except Exception` here made an
            # invalid token or a 403 appear in the report as message loss —
            # a stand problem reported as lost data, which is the worst kind
            # of false alarm in a reliability test.
            if diag is not None:
                k = f"sender:{type(e).__name__}"
                diag[k] = diag.get(k, 0) + 1
            await asyncio.sleep(0.5)                  # dropped connection → reconnect


async def recipient(session, server, token, got, deadline, diag=None):
    while time.monotonic() < deadline:
        try:
            async with session.ws_connect(ws_url(server) + f"?token={token}", heartbeat=20) as ws:
                if diag is not None:
                    diag["recipient_connects"] = diag.get("recipient_connects", 0) + 1
                async for m in ws:
                    if time.monotonic() > deadline:
                        break
                    if m.type == aiohttp.WSMsgType.TEXT:
                        d = json.loads(m.data)
                        if d.get("type") in ("message", "") and d.get("id"):
                            got.add(d["id"])           # unique ids; resend duplicates do not count
        except Exception as e:
            if diag is not None:
                k = f"recipient:{type(e).__name__}"
                diag[k] = diag.get(k, 0) + 1
            await asyncio.sleep(0.5)


async def no_loss_drill(session, server, ta, tb, n, label, chaos, timeout, svc=None):
    """The shared "no loss under failure" harness. chaos is a coroutine that
    breaks a component midway through the run."""
    print(f"\n── {label} (N={n}) ──")
    ids = [str(uuid.uuid4()) for _ in range(n)]
    acked, got = set(), set()
    deadline = time.monotonic() + timeout

    diag = {}
    rtask = asyncio.create_task(recipient(session, server, tb, got, deadline, diag))
    ctask = asyncio.create_task(chaos())
    await sender(session, server, ta, "chaos_a", "chaos_b", ids, acked, deadline, diag)
    await asyncio.sleep(6)                             # grace: pending delivery and resends arrive
    for t in (rtask, ctask):
        t.cancel()
    recv = len(set(ids) & got)
    lost_ack, lost_recv = n - len(acked), n - recv
    print(f"  sent={n}  acknowledged={len(acked)}  received(unique)={recv}")
    errs = {k: v for k, v in diag.items() if ":" in k or k == "ack_failed"}
    if errs:
        print(f"  events: {errs}")

    # A broken stand is not data loss. If neither side ever connected, a
    # "loss" verdict would be a false alarm: what needs fixing is access, not
    # the delivery guarantees.
    if not diag.get("sender_connects") or not diag.get("recipient_connects"):
        print("  ⚠ STAND NOT WORKING: no WebSocket connection was established "
              f"(sender={diag.get('sender_connects', 0)}, "
              f"recipient={diag.get('recipient_connects', 0)}). "
              "This is not message loss — check the tokens and the server.")
        RESULTS.append((label, False, "stand did not come up (no WebSocket) — result is void"))
        return False

    ok = (lost_ack == 0 and lost_recv == 0)
    print(f"  {'✅ NO LOSS' if ok else f'❌ LOSS: unacknowledged={lost_ack}, not received={lost_recv}'}")
    detail = f"{len(acked)}/{n} ack, {recv}/{n} recv"
    if svc and svc in SVC_MISSING:
        detail += "  (service absent from the stack — isolation only, no failure injected)"
        print("  ⚠ no real failure injected — only isolation was verified")
    RESULTS.append((label, ok, detail))
    return ok


# ── Chaos actions ─────────────────────────────────────────────────────────────
def chaos_restart(cmp, svc, after):
    async def run():
        await asyncio.sleep(after)
        print(f"  >>> CHAOS: {cmp} restart {svc}")
        r = await asyncio.to_thread(compose, cmp, "restart", svc)
        if _missing(r, svc):
            print(f"  ⚠ service '{svc}' is not in the stack — no failure injected")
    return run


def chaos_stopstart(cmp, svc, after, down, healthz_url=None):
    async def run():
        await asyncio.sleep(after)
        print(f"  >>> CHAOS: {cmp} stop {svc}")
        r = await asyncio.to_thread(compose, cmp, "stop", svc)
        if _missing(r, svc):
            print(f"  ⚠ service '{svc}' is not in the stack — no failure injected")
            return
        if healthz_url:
            await asyncio.sleep(3)
            code = await asyncio.to_thread(http_code, healthz_url)
            exp = "503" if svc == "db" else "200"
            mark = "✅" if str(code) == exp else "⚠"
            print(f"  /healthz with {svc} stopped: {code} (want {exp}) {mark}")
            RESULTS.append((f"healthz@{svc}-down", str(code) == exp, f"got {code}, want {exp}"))
        await asyncio.sleep(down)
        print(f"  >>> recovery: {cmp} start {svc}")
        await asyncio.to_thread(compose, cmp, "start", svc)
        if healthz_url:
            await asyncio.sleep(10)
            code = await asyncio.to_thread(http_code, healthz_url)
            mark = "✅" if str(code) == "200" else "⚠"
            print(f"  /healthz after {svc} is back: {code} (want 200) {mark}")
            RESULTS.append((f"healthz@{svc}-up", str(code) == "200", f"got {code}, want 200"))
    return run


def scenario_healthz(server, cmp):
    print("\n── healthz: /healthz reflects the database state ──")
    u = server + "/healthz"
    a = http_code(u); print(f"  before:               {a}  (want 200)")
    compose(cmp, "stop", "db"); time.sleep(4)
    b = http_code(u); print(f"  database stopped:     {b}  (want 503)")
    compose(cmp, "start", "db"); time.sleep(10)
    c = http_code(u); print(f"  after recovery:       {c}  (want 200)")
    ok = (a == 200 and b == 503 and c == 200)
    RESULTS.append(("healthz 200→503→200", ok, f"{a}→{b}→{c}"))


# ── Orchestration ─────────────────────────────────────────────────────────────
async def run_scenarios(args):
    conn = aiohttp.TCPConnector(ssl=not args.insecure)
    async with aiohttp.ClientSession(connector=conn) as session:
        # Tokens are obtained once and reused, or repeated registrations hit
        # the per-IP limit. A JWT survives restarts.
        hz_code = await asyncio.to_thread(http_code, args.server + "/healthz")
        print(f"  preflight: {args.server}/healthz → {hz_code}"
              + ("" if hz_code == 200 else "  ⚠ the server does not answer 200 — check --server"))
        ta, ea = await reg_login(session, args.server, "chaos_a")
        tb, eb = await reg_login(session, args.server, "chaos_b")
        if not ta or not tb:
            print(f"  ! could not obtain tokens: chaos_a=[{ea or 'ok'}] chaos_b=[{eb or 'ok'}]")
            print("    Common causes:")
            print("    • --server points at production over http://localhost, where the")
            print("      proxy redirects 80→443 and POST /register|/login breaks. Use the")
            print("      development stack:")
            print("        docker compose up -d   →   --server http://localhost")
            print("      or pass the real https domain: --server https://<domain>")
            print("    • the per-IP registration limit is exhausted — wait 10 minutes")
            return
        hz = args.server + "/healthz"
        want = args.scenario
        run_all = (want == "all")

        if run_all or want == "app":
            await no_loss_drill(session, args.server, ta, tb, args.messages,
                                "A) app restart — the application under traffic",
                                chaos_restart(args.compose, "app", args.restart_after),
                                args.timeout, svc="app")
        if run_all or want == "nginx":
            await asyncio.sleep(2)
            await no_loss_drill(session, args.server, ta, tb, args.messages,
                                "B) proxy restart — every socket dropped",
                                chaos_restart(args.compose, "nginx", args.restart_after),
                                args.timeout, svc="nginx")
        if run_all or want == "db":
            await asyncio.sleep(2)
            await no_loss_drill(session, args.server, ta, tb, args.messages,
                                "C) db stop/start — PostgreSQL down, plus /healthz",
                                chaos_stopstart(args.compose, "db", args.restart_after, args.down, hz),
                                args.timeout, svc="db")
        if run_all or want == "coturn":
            await asyncio.sleep(2)
            await no_loss_drill(session, args.server, ta, tb, args.messages,
                                "D) coturn stop/start — isolation (messaging unaffected)",
                                chaos_stopstart(args.compose, "coturn", args.restart_after, args.down),
                                args.timeout, svc="coturn")


def print_summary(args):
    print("\n" + "═" * 60)
    print("CHAOS DRILL SUMMARY")
    print("═" * 60)
    if not RESULTS:
        print("  ⚠ RUN DID NOT EXECUTE — no scenario completed")
        print("    (see the error above; there is nothing to record)")
        return
    all_ok = True
    for name, ok, detail in RESULTS:
        all_ok = all_ok and ok
        print(f"  {'✅' if ok else '❌'}  {name:<44} {detail}")
    print("─" * 60)
    print(f"  {'✅ ALL SCENARIOS PASSED' if all_ok else '❌ FAILURES — SEE ABOVE'}")

    if args.evidence:
        from datetime import date
        print("\n── markdown summary ──\n")
        print(f"**Reliability drills** (`scripts/chaos-drill.py`, {date.today()}, "
              f"N={args.messages}/scenario):\n")
        print("| Component failure | Message loss | /healthz |")
        print("|---|---|---|")
        def row(label, key):
            r = next((x for x in RESULTS if x[0].startswith(key)), None)
            return "✅ none" if (r and r[1]) else ("❌" if r else "—")
        hz_db = next((x for x in RESULTS if x[0] == "healthz 200→503→200"
                      or x[0].startswith("healthz@db")), None)
        print(f"| restart app | {row('app','A)')} | — |")
        print(f"| restart nginx | {row('nginx','B)')} | — |")
        print(f"| stop/start PostgreSQL | {row('db','C)')} | "
              f"{'✅ 200→503→200' if hz_db and hz_db[1] else '⚠ check'} |")
        coturn_cell = ("⚠ coturn absent — isolation only" if "coturn" in SVC_MISSING
                       else row('coturn', 'D)'))
        print(f"| stop/start coturn | {coturn_cell} | — (unaffected) |")


def main():
    p = argparse.ArgumentParser(description="Hexeris chaos-drill (app/nginx/db/coturn)")
    p.add_argument("--server", default="http://localhost")
    p.add_argument("--compose", default="docker compose",
                   help='e.g. "docker compose -f docker-compose.prod.yml"')
    p.add_argument("--messages", type=int, default=60)
    p.add_argument("--restart-after", type=float, default=3.0, help="seconds before injecting the failure")
    p.add_argument("--down", type=float, default=6.0, help="seconds of downtime for stop/start (db, coturn)")
    p.add_argument("--timeout", type=float, default=120.0, help="hard per-scenario timeout, seconds")
    p.add_argument("--scenario", choices=["app", "nginx", "db", "coturn", "healthz", "all"],
                   default="all")
    p.add_argument("--evidence", action="store_true", help="print a markdown summary block")
    p.add_argument("--insecure", action="store_true", help="skip TLS verification (self-signed certificates)")
    args = p.parse_args()

    if args.scenario == "healthz":
        scenario_healthz(args.server, args.compose)
    else:
        asyncio.run(run_scenarios(args))
    print_summary(args)
    print("\nDone.")


if __name__ == "__main__":
    main()
