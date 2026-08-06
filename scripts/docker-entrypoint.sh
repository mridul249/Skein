#!/bin/sh
#
# Container entrypoint for the Skein server.
#
# It exists to do one thing the binary deliberately does not: generate the two
# secrets a first run needs, and then TELL THE USER, loudly and in a file they
# can keep.
#
# THE MASTER KEY IS THE WHOLE REASON THIS IS CAREFUL. SKEIN_MASTER_KEY encrypts
# every shard. Generating one silently would mean a user runs `docker compose
# up`, uploads their files, loses the container, and discovers the only copy of
# the key went with it — every file permanently unreadable, with nothing having
# warned them. So a generated key is written to a host-mounted file, announced
# in a banner that is hard to miss, and never regenerated over an existing one.
set -eu

INFO_DIR=${SKEIN_INFO_DIR:-/data}
INFO_FILE="$INFO_DIR/skein-setup-info.txt"

# Written only when this run generated something. A restart that supplies both
# secrets from the environment leaves the file untouched.
generated_key=0
generated_jwt=0

random_b64() {
  # head -c from /dev/urandom, base64 with no wrapping. openssl is not in the
  # final image and is not worth adding for two lines of shell.
  head -c "$1" /dev/urandom | base64 | tr -d '\n'
}

# ---- SKEIN_MASTER_KEY -------------------------------------------------------
#
# Read back from the info file before generating, so a restart with an empty
# environment does not mint a SECOND key and render the first run's files
# unreadable. That is the single most destructive thing this script could do,
# so the check comes before the generation rather than after.
if [ -z "${SKEIN_MASTER_KEY:-}" ] && [ -f "$INFO_FILE" ]; then
  recovered=$(sed -n 's/^SKEIN_MASTER_KEY=//p' "$INFO_FILE" | head -n 1)
  if [ -n "$recovered" ]; then
    SKEIN_MASTER_KEY="$recovered"
    export SKEIN_MASTER_KEY
    echo "skein: reusing the master key recorded in $INFO_FILE"
  fi
fi

if [ -z "${SKEIN_MASTER_KEY:-}" ]; then
  SKEIN_MASTER_KEY=$(random_b64 32)
  export SKEIN_MASTER_KEY
  generated_key=1
fi

# ---- SKEIN_JWT_SECRET -------------------------------------------------------
#
# Losing this signs everyone out. That is an inconvenience, not data loss, so
# it is regenerated freely rather than recovered from the info file.
if [ -z "${SKEIN_JWT_SECRET:-}" ]; then
  SKEIN_JWT_SECRET=$(random_b64 48)
  export SKEIN_JWT_SECRET
  generated_jwt=1
fi

# ---- The report -------------------------------------------------------------
if [ "$generated_key" -eq 1 ] || [ "$generated_jwt" -eq 1 ]; then
  if ! mkdir -p "$INFO_DIR" 2>/dev/null || [ ! -w "$INFO_DIR" ]; then
    echo "skein: FATAL: $INFO_DIR is not writable, so a generated secret could" >&2
    echo "       not be recorded anywhere. Refusing to start with a key that" >&2
    echo "       exists only inside this container." >&2
    echo "       Mount a writable volume at $INFO_DIR, or set SKEIN_MASTER_KEY" >&2
    echo "       and SKEIN_JWT_SECRET yourself." >&2
    exit 1
  fi

  # 0600 before anything is written, so the secret is never briefly readable.
  umask 077
  {
    echo "Skein setup"
    echo "==========="
    echo "Written $(date -u '+%Y-%m-%d %H:%M:%SZ') by the container entrypoint."
    echo "Image: ${SKEIN_IMAGE_DIGEST:-unknown}"
    echo "Build target: web (server plus embedded UI)"
    echo
    if [ "$generated_key" -eq 1 ]; then
      echo "!!! MOVE THIS FILE SOMEWHERE SAFE, AND DO NOT LEAVE IT HERE !!!"
      echo
      echo "The master key below was GENERATED because SKEIN_MASTER_KEY was not"
      echo "set. It encrypts every file you upload. This file is currently the"
      echo "ONLY copy. If you lose it, every file Skein has stored becomes"
      echo "permanently unreadable — there is no reset and no recovery."
      echo
      echo "It is sitting next to your Postgres volume, which is the one place"
      echo "it must not stay: a single lost disk would take both the key and"
      echo "the database. Copy it into a password manager, then delete this"
      echo "file or move it off this machine."
      echo
    fi
    echo "SKEIN_MASTER_KEY=$SKEIN_MASTER_KEY"
    if [ "$generated_jwt" -eq 1 ]; then
      echo "SKEIN_JWT_SECRET=$SKEIN_JWT_SECRET"
    fi
    echo
    echo "Postgres data"
    echo "-------------"
    echo "Stored in the named Docker volume 'skein-pgdata'. Find it with:"
    echo "  docker volume inspect skein-pgdata"
    echo "Back it up with:"
    echo "  docker compose exec -T postgres pg_dump -U skein skein | gzip > skein-backup.sql.gz"
    echo
    echo "THE KEY ALONE CANNOT RESTORE YOUR FILES, AND NEITHER CAN THE DATABASE"
    echo "ALONE. The key decrypts shard contents; the database records which"
    echo "shard belongs to which file. Keep both, in different places."
    echo
    echo "Next"
    echo "----"
    echo "1. Open ${SKEIN_PUBLIC_URL:-http://localhost:8080}"
    echo "2. Register an account."
    echo "3. Connect a Google Drive in Settings."
    echo "4. Upload a file, then download it, to confirm the round trip works."
    echo
    echo "Full setup notes: docs/SETUP.md. Backups: docs/BACKUP.md."
  } > "$INFO_FILE"
  chmod 600 "$INFO_FILE" 2>/dev/null || true

  # The banner. Deliberately noisy: this is the one message a user must not
  # scroll past, and it is printed on the run that generated the key rather
  # than on every start, so it does not become background noise.
  echo ""
  echo "============================================================"
  if [ "$generated_key" -eq 1 ]; then
    echo " A MASTER KEY WAS GENERATED FOR YOU."
    echo ""
    echo " It encrypts every file you upload, and the only copy is:"
    echo "   $INFO_FILE"
    echo ""
    echo " LOSE IT AND EVERY FILE BECOMES PERMANENTLY UNREADABLE."
    echo " Copy it somewhere safe now, before you upload anything."
  else
    echo " A session secret was generated. See $INFO_FILE"
  fi
  echo "============================================================"
  echo ""
fi

exec /usr/local/bin/skein "$@"
