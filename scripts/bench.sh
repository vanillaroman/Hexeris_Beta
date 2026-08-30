#!/usr/bin/env bash
# Runs loadtest.py with per-container CPU attribution and a pg_stat_statements
# breakdown.
#
#   scripts/bench.sh <label> [users] [msgs] [url]
#
# Artefacts land in BENCH_DIR: <label>.txt (the test log with timestamps),
# <label>.cpu (container cpu.stat) and <label>.pgss (top Postgres queries).
#
# On method: cpu.stat is not sampled through `docker exec`, which starts a
# process inside the container's own cgroup and consumes its CPU limit — with
# a 1.0 limit on the database that visibly skewed the numbers. A single
# sidecar reads /sys/fs/cgroup instead and sees every container's counters.
set -u
LABEL="${1:-run}"; USERS="${2:-200}"; MSGS="${3:-100}"; URL="${4:-http://localhost:8081}"
BENCH_DIR="${BENCH_DIR:-/tmp/hexeris-bench}"
PY="${PY:-python3}"
mkdir -p "$BENCH_DIR"

psql_q() { docker exec hexeris-db-1 psql -U hexeris -d hexeris -q "$@"; }

# ── Reset state, or runs are not comparable ────────────────────────────────
# Each run leaves N×M rows in messages, so the next one starts with a
# different table and index size, and a different INSERT cost.
if [ "${BENCH_RESET:-1}" = "1" ]; then
  psql_q -c 'TRUNCATE messages, reactions RESTART IDENTITY' \
         -c 'VACUUM ANALYZE messages' >/dev/null 2>&1
  # An explicit CHECKPOINT before the run: otherwise one lands mid-measurement
  # and adds full-page writes all at once, which was the main source of
  # variance between runs (700…2800 msg/s on the same configuration).
  psql_q -c 'CHECKPOINT' >/dev/null 2>&1
  docker restart hexeris-app-1 >/dev/null 2>&1
  for _ in $(seq 1 30); do curl -sf -o /dev/null "$URL/healthz" && break; sleep 1; done
  sleep 2
fi

# Experimental schema changes go after the app restart: initDB re-creates
# indexes with IF NOT EXISTS, so dropping them beforehand achieves nothing.
[ -n "${BENCH_SQL:-}" ] && psql_q -c "$BENCH_SQL" >/dev/null 2>&1

psql_q -tAc 'SELECT pg_stat_statements_reset()' >/dev/null 2>&1

# ── CPU sampling sidecar ───────────────────────────────────────────────────
CG=""
for c in hexeris-app-1 hexeris-db-1 hexeris-nginx-1; do
  CG+="$c:$(docker inspect -f '{{.Id}}' "$c") "
done
docker run --rm --name hexeris-cpusampler -v /sys/fs/cgroup:/hcg:ro \
  -e CG="$CG" alpine sh -c '
    while :; do
      line="$(date +%s.%N)"
      for pair in $CG; do
        n=${pair%%:*}; id=${pair#*:}
        u=$(awk "/usage_usec/{print \$2}" /hcg/docker/$id/cpu.stat 2>/dev/null)
        line="$line $n=${u:-0}"
      done
      echo "$line"
      sleep 0.5
    done' > "$BENCH_DIR/$LABEL.cpu" 2>/dev/null &

trap 'docker rm -f hexeris-cpusampler >/dev/null 2>&1' EXIT

ulimit -n 65535
# -u: otherwise Python buffers output into the pipe, every line arrives at
# the end at once and the phase timestamps become meaningless.
$PY -u scripts/loadtest.py --server "$URL" -n "$USERS" -m "$MSGS" \
    --mode ws --drain 90 --admin-key "${ADMIN_KEY:-devadmin}" 2>&1 \
  | while IFS= read -r line; do echo "$(date +%s.%N) $line"; done \
  | tee "$BENCH_DIR/$LABEL.txt"

docker rm -f hexeris-cpusampler >/dev/null 2>&1

# Where the database time actually went (total_exec_time, ms, descending).
docker exec hexeris-db-1 psql -U hexeris -d hexeris -qAF$'\t' -c \
  "SELECT round(total_exec_time)::bigint ms, calls,
          round(mean_exec_time::numeric, 3) mean_ms,
          round(total_exec_time*100/nullif(sum(total_exec_time) OVER (),0))::int pct,
          left(regexp_replace(query, '\s+', ' ', 'g'), 110) q
     FROM pg_stat_statements
    WHERE query NOT LIKE '%pg_stat_statements%'
    ORDER BY total_exec_time DESC LIMIT 12" > "$BENCH_DIR/$LABEL.pgss" 2>&1

echo "→ $BENCH_DIR/$LABEL.{txt,cpu,pgss}"
