#!/usr/bin/env bash
# ==============================================================================
#  Hexeris restore drill — proves a backup is genuinely restorable and
#  measures RTO and RPO. Production is never touched.
# ==============================================================================
#
#  What it does, entirely in scratch resources:
#    1. Takes the latest backup set (or the one given by --stamp) from
#       DB_BACKUP_DIR:
#         hexeris-<stamp>.sql.gz        — the database dump
#         hexeris-files-<stamp>.tar.gz  — the attachments
#    2. Restores the dump into a scratch database and the files into a
#       scratch directory.
#    3. Measures RTO — how long the restore took.
#    4. Computes RPO — the age of the latest backup, the maximum data-loss
#       window.
#    5. Checks usability with `hexeris verify-restore`: non-zero row counts,
#       decryption of a sample of message bodies with the current
#       SERVER_ENC_KEY (which proves the key matches the backup) and the
#       structure of the files.
#    6. Removes the scratch database and directory on exit.
#
#  Requires psql, gunzip, tar and a built binary, with the same
#  SERVER_ENC_KEY in the environment as when the backup was taken.
#
#  Example (a local postgres with peer authentication, as in BACKUP.md):
#    export SERVER_ENC_KEY="<the same key>"
#    DB_BACKUP_DIR=/var/backups/hexeris PG_CONN="postgres://hexeris@localhost" \
#      HEXERIS_BIN=/root/hexeris/server/hexeris \
#      bash scripts/restore-drill.sh
# ==============================================================================
set -euo pipefail

BACKUP_DIR="${DB_BACKUP_DIR:-/var/backups/hexeris}"
PG_CONN="${PG_CONN:-postgres://localhost}"          # without a database name; maintenance = $PG_CONN/postgres
HEXERIS_BIN="${HEXERIS_BIN:-./hexeris}"
STAMP="${STAMP:-}"

while [ $# -gt 0 ]; do
  case "$1" in
    --backup-dir) BACKUP_DIR="$2"; shift 2;;
    --conn)       PG_CONN="$2"; shift 2;;
    --bin)        HEXERIS_BIN="$2"; shift 2;;
    --stamp)      STAMP="$2"; shift 2;;
    -h|--help)    sed -n '2,30p' "$0"; exit 0;;
    *) echo "unknown argument: $1" >&2; exit 2;;
  esac
done

command -v psql   >/dev/null || { echo "psql is required (package postgresql-client)"; exit 1; }
command -v gunzip >/dev/null || { echo "gunzip is required"; exit 1; }
command -v tar    >/dev/null || { echo "tar is required"; exit 1; }
[ -x "$HEXERIS_BIN" ] || { echo "binary not found: $HEXERIS_BIN (build it: cd server && go build -o hexeris .)"; exit 1; }
[ -n "${SERVER_ENC_KEY:-}" ] || { echo "SERVER_ENC_KEY is not set — verify-restore cannot check decryption"; exit 1; }

# ── Choosing the backup set ───────────────────────────────────────────────────
if [ -z "$STAMP" ]; then
  latest="$(ls -1t "$BACKUP_DIR"/hexeris-*.sql.gz 2>/dev/null | head -1 || true)"
  [ -n "$latest" ] || { echo "no hexeris-*.sql.gz in $BACKUP_DIR"; exit 1; }
  STAMP="$(basename "$latest" | sed -E 's/^hexeris-(.*)\.sql\.gz$/\1/')"
fi
DUMP="$BACKUP_DIR/hexeris-$STAMP.sql.gz"
FILES="$BACKUP_DIR/hexeris-files-$STAMP.tar.gz"
[ -f "$DUMP" ] || { echo "dump not found: $DUMP"; exit 1; }
echo "Set:        $STAMP"
echo "  dump:     $DUMP"
[ -f "$FILES" ] && echo "  files:    $FILES" || echo "  files:    (no hexeris-files-$STAMP.tar.gz — checking the database only)"

# ── RPO: the age of the latest backup ─────────────────────────────────────────
# The stamp is YYYYmmdd-HHMMSS in UTC; convert it to epoch seconds.
S="$STAMP"
iso="${S:0:4}-${S:4:2}-${S:6:2} ${S:9:2}:${S:11:2}:${S:13:2}"
if stamp_epoch="$(date -u -d "$iso" +%s 2>/dev/null)"; then
  now_epoch="$(date -u +%s)"
  age_s=$(( now_epoch - stamp_epoch ))
  printf "RPO:        latest backup %02d:%02d:%02d ago (maximum data-loss window)\n" \
    $(( age_s/3600 )) $(( (age_s%3600)/60 )) $(( age_s%60 ))
else
  echo "RPO:        could not parse stamp '$STAMP' (skipping the age calculation)"
fi

# ── Scratch resources, always cleaned up ──────────────────────────────────────
# A local postgres usually has no TLS while the Go driver defaults to
# sslmode=require and would fail, so disable is the default here. For a remote
# database with TLS, set PG_SSLMODE=require.
PG_SSLMODE="${PG_SSLMODE:-disable}"
SCRATCH_DB="hexeris_restore_drill_$$_$(date +%s)"
SCRATCH_UP="$(mktemp -d /tmp/hexeris-restore-XXXXXX)"
MAINT="$PG_CONN/postgres?sslmode=$PG_SSLMODE"
SCRATCH_URL="$PG_CONN/$SCRATCH_DB?sslmode=$PG_SSLMODE"

cleanup() {
  echo "── cleanup ──"
  psql "$MAINT" -v ON_ERROR_STOP=0 -q -c "DROP DATABASE IF EXISTS \"$SCRATCH_DB\";" >/dev/null 2>&1 || true
  rm -rf "$SCRATCH_UP" 2>/dev/null || true
}
trap cleanup EXIT

# ── Restore (measuring RTO) ───────────────────────────────────────────────────
echo "── restoring into scratch ($SCRATCH_DB) ──"
t0="$(date +%s.%N)"
psql "$MAINT" -v ON_ERROR_STOP=1 -q -c "CREATE DATABASE \"$SCRATCH_DB\";"
gunzip -c "$DUMP" | psql "$SCRATCH_URL" -v ON_ERROR_STOP=1 -q >/dev/null
if [ -f "$FILES" ]; then
  tar -xzf "$FILES" -C "$SCRATCH_UP"
fi
t1="$(date +%s.%N)"
rto="$(awk "BEGIN{printf \"%.1f\", $t1 - $t0}")"
echo "RTO:        restore took ${rto}s (dump + files; add a few seconds for a service restart in a real recovery)"

# ── Usability check ───────────────────────────────────────────────────────────
echo "── verify-restore ──"
# The binary's startup requires these variables even though verify-restore
# does not use them, so harmless defaults are supplied; real values from the
# environment win. Only SERVER_ENC_KEY actually matters here.
set +e
UPLOAD_DIR="${UPLOAD_DIR:-$SCRATCH_UP}" \
STATIC_DIR="${STATIC_DIR:-/tmp}" \
JWT_SECRET="${JWT_SECRET:-restore-drill-dummy}" \
  "$HEXERIS_BIN" verify-restore "$SCRATCH_URL" "$SCRATCH_UP"
verdict=$?
set -e

echo "════════════════════════════════════════════════════════════"
if [ "$verdict" -eq 0 ]; then
  echo "✅ RESTORE DRILL PASSED — backup $STAMP is restorable (RTO ${rto}s)"
else
  echo "❌ RESTORE DRILL FAILED — see VERIFY-RESTORE above (code $verdict)"
fi
echo "════════════════════════════════════════════════════════════"
exit "$verdict"
