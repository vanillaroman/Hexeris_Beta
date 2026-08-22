#!/usr/bin/env python3
"""Summary of a scripts/bench.sh run: msg/s, ACK latency and each container's
CPU seconds during the message phase alone — the window between the "── WS:"
line and the "throughput" line in the log. That is what shows where the
bottleneck actually is."""
import re
import sys
import pathlib

BENCH = pathlib.Path(sys.argv[1] if len(sys.argv) > 1 else "/tmp/hexeris-bench")


def one(label):
    txt = (BENCH / f"{label}.txt").read_text().splitlines()
    cpu = (BENCH / f"{label}.cpu").read_text().splitlines()

    t_start = t_end = None
    thr = ack = conn = sent = None
    for line in txt:
        ts, _, body = line.partition(" ")
        try:
            ts = float(ts)
        except ValueError:
            continue
        if "── WS:" in body:
            t_start = ts
        if "throughput" in body:
            t_end = ts
            m = re.search(r"throughput\s+([\d.]+)", body)
            thr = float(m.group(1)) if m else None
        if "ack_latency" in body:
            ack = body.strip()
        if body.strip().startswith("connected"):
            conn = body.strip()
        if body.strip().startswith("sent="):
            sent = body.strip()
    if not (t_start and t_end):
        return f"{label}: message phase not found in the log"

    # CPU seconds over the phase window: the usage_usec delta between the
    # first and last samples.
    rows = []
    for line in cpu:
        parts = line.split()
        if len(parts) < 4:
            continue
        try:
            ts = float(parts[0])
        except ValueError:
            continue
        vals = {}
        for p in parts[1:]:
            k, _, v = p.partition("=")
            try:
                vals[k] = int(v)
            except ValueError:
                vals[k] = 0
        rows.append((ts, vals))
    inwin = [r for r in rows if t_start <= r[0] <= t_end]
    out = [f"\n=== {label} ===", f"  {conn}", f"  {sent}",
           f"  throughput   {thr} msg/s   (phase {t_end - t_start:.1f}s)", f"  {ack}"]
    if len(inwin) >= 2:
        first, last = inwin[0][1], inwin[-1][1]
        span = inwin[-1][0] - inwin[0][0]
        out.append(f"  CPU during the phase ({span:.1f}s window):")
        for c in first:
            secs = (last.get(c, 0) - first.get(c, 0)) / 1e6
            out.append(f"    {c:<18} {secs:7.1f} CPU-s   ({100*secs/span:5.0f}% of one core)")
    return "\n".join(out)


for label in sys.argv[2:] or ["baseline"]:
    print(one(label))
print()
