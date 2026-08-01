#!/usr/bin/env bash
# Rehearse the docs/BACKUP.md restore procedure against a throwaway database.
#
# A backup is only a backup if it restores. This runs the documented steps
# verbatim, then compares every table's row count against the source database,
# and drops the throwaway at the end. Issue #8.
#
# Usage:
#   scripts/restore-rehearsal.sh [path/to/dump.sql.gz]
#
# With no argument it rehearses the newest dump in backups/.
#
# Creating a database needs a role with CREATEDB, which the application role
# deliberately does not have. Either grant it once:
#     sudo -u postgres psql -c 'ALTER ROLE skein CREATEDB'
# or pre-create the target and pass SKEIN_RESTORE_DB_EXISTS=1:
#     sudo -u postgres createdb -O skein skein_restore_test
set -euo pipefail

REPO="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$REPO"

[ -f .env ] && set -a && . ./.env && set +a
: "${SKEIN_DATABASE_URL:?SKEIN_DATABASE_URL is not set (checked the environment and .env)}"

DUMP="${1:-$(ls -t backups/*.sql.gz 2>/dev/null | head -1)}"
[ -n "$DUMP" ] && [ -f "$DUMP" ] || { echo "no dump found; run 'make backup' first" >&2; exit 1; }

TARGET_DB="${SKEIN_RESTORE_DB:-skein_restore_rehearsal}"
BASE="${SKEIN_DATABASE_URL%/*}"
SUFFIX=""
case "$SKEIN_DATABASE_URL" in *\?*) SUFFIX="?${SKEIN_DATABASE_URL#*\?}";; esac
TARGET_URL="${BASE}/${TARGET_DB}${SUFFIX}"

# Tables the dump is expected to carry. Compared row-for-row below; a restore
# that silently drops one is the failure this rehearsal exists to catch.
#
# goose_db_version is checked by count only (see below): step 3 legitimately
# appends rows to it when the restore lands on an older schema than the repo
# defines, so its contents are allowed to differ.
TABLES="users sessions connected_accounts folders files file_shards"
COUNT_ONLY_TABLES="goose_db_version"

# Row counts alone would pass a restore that kept every row and corrupted
# every value, so this hashes whole rows: md5 over all columns of all rows,
# ordered so the result does not depend on physical row order. That is what
# actually proves the shard mapping and wrapped key material survived.
counts() { # $1 = database url
  for t in $TABLES; do
    n="$(psql -qAt "$1" -c "SELECT count(*) FROM $t" 2>/dev/null || echo ERR)"
    h="$(psql -qAt "$1" -c "SELECT coalesce(md5(string_agg(x::text,'|' ORDER BY x::text)),'empty') FROM $t x" 2>/dev/null || echo ERR)"
    printf '%s|rows=%s|md5=%s\n' "$t" "$n" "$h"
  done
  for t in $COUNT_ONLY_TABLES; do
    printf '%s|rows=%s\n' "$t" "$(psql -qAt "$1" -c "SELECT count(*) FROM $t" 2>/dev/null || echo ERR)"
  done
}

cleanup() {
  if [ "${KEEP:-0}" = "1" ]; then
    echo "KEEP=1 set; leaving $TARGET_DB in place"
    return
  fi
  psql -qAt "${BASE}/postgres${SUFFIX}" -c "DROP DATABASE IF EXISTS $TARGET_DB" >/dev/null 2>&1 \
    || sudo -u postgres dropdb --if-exists "$TARGET_DB" >/dev/null 2>&1 \
    || echo "note: could not drop $TARGET_DB; drop it by hand" >&2
}
trap cleanup EXIT

echo "==> dump:   $DUMP"
echo "==> target: $TARGET_DB"
echo

echo "--- source row counts ---"
BEFORE="$(counts "$SKEIN_DATABASE_URL")"
echo "$BEFORE"
echo

# Step 1 (docs/BACKUP.md): create the target database.
if [ "${SKEIN_RESTORE_DB_EXISTS:-0}" != "1" ]; then
  echo "--- step 1: createdb ---"
  if ! psql -qAt "${BASE}/postgres${SUFFIX}" -c "DROP DATABASE IF EXISTS $TARGET_DB" >/dev/null 2>&1; then
    echo "cannot drop/create as the app role; trying sudo -u postgres" >&2
  fi
  if psql -qAt "${BASE}/postgres${SUFFIX}" -c "CREATE DATABASE $TARGET_DB" >/dev/null 2>&1; then
    echo "created $TARGET_DB as the application role"
  elif sudo -n -u postgres createdb -O "${SKEIN_DB_OWNER:-skein}" "$TARGET_DB" 2>/dev/null; then
    echo "created $TARGET_DB via sudo -u postgres"
  else
    cat >&2 <<EOF
FAILED at step 1: cannot create $TARGET_DB.

The application role has no CREATEDB. Do one of these, then re-run:

  sudo -u postgres psql -c 'ALTER ROLE skein CREATEDB'      # once, permanent
  sudo -u postgres createdb -O skein $TARGET_DB             # then SKEIN_RESTORE_DB_EXISTS=1
EOF
    exit 1
  fi
fi

# Step 2: restore. ON_ERROR_STOP=1 is what makes a partial restore an error
# rather than a database that looks fine and is silently missing rows.
echo "--- step 2: restore (ON_ERROR_STOP=1) ---"
gunzip -c "$DUMP" | psql -q -v ON_ERROR_STOP=1 -d "$TARGET_URL" >/dev/null
echo "restore completed without error"

# Step 3: migrate forward to whatever the repo currently defines.
echo "--- step 3: migrate forward ---"
if [ -x "${GOBIN:-$(go env GOPATH)/bin}/goose" ]; then
  SKEIN_DATABASE_URL="$TARGET_URL" \
    "${GOBIN:-$(go env GOPATH)/bin}/goose" -dir internal/db/migrations postgres "$TARGET_URL" up
else
  echo "goose not installed; skipping (run 'make tools')"
fi

echo
echo "--- restored row counts ---"
AFTER="$(counts "$TARGET_URL")"
echo "$AFTER"
echo

if [ "$BEFORE" = "$AFTER" ]; then
  echo "PASS: every table matches the source exactly."
else
  echo "FAIL: restored database differs from the source." >&2
  diff <(echo "$BEFORE") <(echo "$AFTER") >&2 || true
  exit 1
fi
