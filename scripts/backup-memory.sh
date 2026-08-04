#!/usr/bin/env bash
# Back up PLAN/Memory.md, the sole cross-session state document.
#
# Memory.md is gitignored (/PLAN in .gitignore), so git is not backing it up
# and no commit will ever restore it. Until 2026-08-04 the only copy on disk
# was the file itself: `find . -name "Memory*"` returned exactly one path. One
# bad write, one stray `>`, and every decision, closed issue and design note
# from every prior session is gone with nothing to recover from.
#
# This exists so the backup is a command rather than a habit. A habit is what
# produced the single-copy state it replaces.
#
# Usage:
#   scripts/backup-memory.sh          # write a timestamped copy
#   scripts/backup-memory.sh --list   # show what has been kept
set -euo pipefail

REPO="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
SRC="$REPO/PLAN/Memory.md"
DEST="$REPO/backups/memory"
KEEP=30

if [ "${1:-}" = "--list" ]; then
	if [ -d "$DEST" ]; then
		ls -lh "$DEST" | tail -n +2
	else
		echo "no Memory.md backups yet — run scripts/backup-memory.sh"
	fi
	exit 0
fi

# A missing source is a hard failure, not a no-op that writes an empty file.
# The database `backup` target learned this the expensive way (issue in the
# Makefile header): a backup that fails silently is worse than one that fails
# loudly.
if [ ! -s "$SRC" ]; then
	echo "backup-memory FAILED: $SRC is missing or empty; nothing written" >&2
	exit 1
fi

mkdir -p "$DEST"
out="$DEST/Memory-$(date +%Y%m%d-%H%M%S).md"

# Skip an identical consecutive copy: running this from a hook or a loop
# should not fill the directory with duplicates of an unchanged file.
newest="$(ls -1 "$DEST"/Memory-*.md 2>/dev/null | tail -1 || true)"
if [ -n "$newest" ] && cmp -s "$SRC" "$newest"; then
	echo "unchanged since $(basename "$newest"); nothing written"
	exit 0
fi

cp "$SRC" "$out"

# Verify the copy rather than trusting cp's exit status.
if ! cmp -s "$SRC" "$out"; then
	rm -f "$out"
	echo "backup-memory FAILED: copy did not match source; removed" >&2
	exit 1
fi

# Prune oldest, keeping the last $KEEP. Sorted by filename, which is
# chronological by construction.
count="$(ls -1 "$DEST"/Memory-*.md 2>/dev/null | wc -l)"
if [ "$count" -gt "$KEEP" ]; then
	ls -1 "$DEST"/Memory-*.md | head -n "$((count - KEEP))" | xargs -r rm -f
fi

echo "wrote $(realpath --relative-to="$REPO" "$out") ($(du -h "$out" | cut -f1)), $(ls -1 "$DEST"/Memory-*.md | wc -l) kept"
