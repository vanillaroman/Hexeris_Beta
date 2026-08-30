#!/usr/bin/env bash
# Runs every UI suite. A failure in any of them gives a non-zero exit code.
#
#   HEXERIS_URL=http://localhost:8080 tests/ui/run-all.sh
#
# Variables:
#   HEXERIS_URL      — the instance address (default http://127.0.0.1:8766)
#   PLAYWRIGHT_PATH  — path to the playwright module if it is not in node_modules
#   CHROMIUM_PATH    — path to the Chromium binary if Playwright cannot find it
#
# Requirements for the instance: registration enabled (REGISTRATION_ENABLED=true)
# and a raised REGISTER_MAX_PER_IP limit — the suites create test users.
set -u
cd "$(dirname "$0")"
fail=0
for f in uitest*.js; do
  printf '%-30s' "$f"
  if out=$(node "$f" 2>&1); then
    echo "OK"
  else
    echo "FAILED"
    echo "$out" | grep -E '^  FAIL|JS ERROR' | sed 's/^/    /'
    fail=$((fail+1))
  fi
done
echo
[ "$fail" -eq 0 ] && echo "All suites passed." || echo "Suites with failures: $fail"
exit "$fail"
