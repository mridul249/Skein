#!/usr/bin/env bash
#
# Refuse to release if any Docker base image is pinned by tag alone.
#
# WHY THIS EXISTS. golang:1.26-alpine moved from go1.26.4 to go1.26.5 during
# this project with no error, no warning, and no changelog visible from a
# `docker build` — it was caught only because a byte-comparison against a
# previously-published artifact turned up an unexplained diff. A tag can move
# under a rebuild at any time; a digest cannot. Every FROM and every compose
# `image:` in this repository must therefore carry an `@sha256:...` pin, so
# the reproducibility guarantee ("two clean builds are byte-identical") holds
# permanently rather than until the next base-image rebuild upstream.
#
# Run before cutting a release tag. Exits non-zero and names every offending
# line if anything is unpinned.
set -euo pipefail

cd "$(dirname "$0")/.."

fail=0

# Dockerfiles: any FROM naming a real image (not a build stage, not `scratch`)
# without an @sha256 digest.
while IFS=: read -r file line content; do
  echo "ERROR: $file:$line unpinned base image - $content" >&2
  fail=1
done < <(
  grep -rnE '^FROM[[:space:]]+[^[:space:]]+' --include='Dockerfile*' . \
    | grep -viE 'FROM[[:space:]]+scratch([[:space:]]|$)' \
    | grep -vE '@sha256:'
)

# Compose files: any `image:` line without an @sha256 digest. Excludes lines
# whose value is itself a variable (${SKEIN_IMAGE:-skein:local}) - that names
# an image this repository builds locally, not a pullable upstream base image,
# so there is nothing to pin.
while IFS=: read -r file line content; do
  echo "ERROR: $file:$line unpinned image - $content" >&2
  fail=1
done < <(
  grep -rnE '^\s*image:\s*[a-zA-Z0-9]' --include='compose*.yaml' --include='compose*.yml' . \
    | grep -vE '\$\{' \
    | grep -vE '@sha256:'
)

if [ "$fail" -ne 0 ]; then
  echo "" >&2
  echo "refusing to release: pin every base image above to a digest." >&2
  echo "  docker pull <image>:<tag>" >&2
  echo "  docker inspect --format='{{index .RepoDigests 0}}' <image>:<tag>" >&2
  exit 1
fi

echo "all base images are digest-pinned"
