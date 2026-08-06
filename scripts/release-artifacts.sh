#!/usr/bin/env bash
#
# Build the release binaries and the checksum file that covers them.
#
# WHY THIS IS A SCRIPT AND NOT WORKFLOW YAML. The release must be runnable
# locally — to reproduce what CI published, or to cut a release when CI is
# unavailable — and logic that lives only in a workflow can be neither run nor
# tested. The workflow calls this; it does not reimplement it.
#
# THE INVARIANT THIS EXISTS TO ENFORCE: every published artifact appears in
# SHA256SUMS, and the script fails if any does not. A checksum file that
# sometimes omits a binary is worse than none, because it teaches people that
# skipping the check is normal.
set -euo pipefail

OUT=${OUT:-dist}
VERSION=${VERSION:-$(git describe --tags --always --dirty 2>/dev/null || echo dev)}

# The platforms a v1 publishes. Each entry is GOOS/GOARCH.
#
# Linux and macOS only, and that is deliberate rather than an oversight:
# Windows needs its own path handling for the config directory and has never
# been run, so shipping an untested binary would be a claim the project cannot
# support. See README "Not in v1".
PLATFORMS=${PLATFORMS:-"linux/amd64 linux/arm64 darwin/amd64 darwin/arm64"}

rm -rf "$OUT"
mkdir -p "$OUT"

echo "building $VERSION"

for platform in $PLATFORMS; do
  goos=${platform%%/*}
  goarch=${platform##*/}
  name="skein-${VERSION}-${goos}-${goarch}"

  # -trimpath and -buildvcs=false are what make two builds of one commit
  # byte-identical: trimpath removes the build directory from the binary, and
  # buildvcs=false stops Go stamping vcs.modified, which differs between a
  # clean checkout and a dirty working tree. Verified 2026-08-06: two clean
  # clones at different paths produced identical SHA256s.
  #
  # CGO_ENABLED=0 for a static binary with no glibc version floor. The desktop
  # build needs cgo and is not cross-compiled here; it is built on its own
  # platform, by `make desktop`.
  CGO_ENABLED=0 GOOS="$goos" GOARCH="$goarch" go build \
    -trimpath \
    -buildvcs=false \
    -ldflags "-s -w -X main.version=${VERSION}" \
    -o "$OUT/$name" \
    ./cmd/skein

  echo "  $name  $(du -h "$OUT/$name" | cut -f1)"
done

# THE CHECKSUM FILE, GENERATED FROM WHAT IS ACTUALLY THERE rather than from the
# list above. Deriving it from PLATFORMS would let a silently failed build drop
# out of both the directory and the manifest at once, which is precisely the
# case the coverage check below has to catch.
(
  cd "$OUT"
  # Sorted so the file is stable across runs and diffable between releases.
  find . -maxdepth 1 -type f ! -name 'SHA256SUMS*' -printf '%f\n' \
    | LC_ALL=C sort \
    | xargs sha256sum > SHA256SUMS
)

# COVERAGE CHECK. Every artifact must be named in SHA256SUMS. This is the rule
# the brief calls out and it is enforced here rather than trusted: a release
# that publishes a binary with no checksum trains people to skip checking.
missing=0
while IFS= read -r f; do
  if ! grep -qF "  $f" "$OUT/SHA256SUMS"; then
    echo "ERROR: $f is published with no checksum" >&2
    missing=1
  fi
done < <(cd "$OUT" && find . -maxdepth 1 -type f ! -name 'SHA256SUMS*' -printf '%f\n')

if [ "$missing" -ne 0 ]; then
  echo "refusing to release: SHA256SUMS does not cover every artifact" >&2
  exit 1
fi

# And the converse: a checksum naming a file that is not there means the
# manifest is stale, which would fail verification for the wrong reason.
while read -r _ f; do
  if [ ! -f "$OUT/$f" ]; then
    echo "ERROR: SHA256SUMS names $f, which was not built" >&2
    missing=1
  fi
done < "$OUT/SHA256SUMS"

if [ "$missing" -ne 0 ]; then
  echo "refusing to release: SHA256SUMS names a file that does not exist" >&2
  exit 1
fi

count=$(wc -l < "$OUT/SHA256SUMS")
echo "SHA256SUMS covers $count artifact(s)"

# SIGNING, when a signer is available.
#
# COSIGN KEYLESS RATHER THAN GPG, and the reason is maintenance rather than
# cryptography. A GPG release key is a long-lived secret someone has to hold,
# rotate, revoke if it leaks, and remember the passphrase for a year from now;
# the usual failure is not a compromised key but a lost one, after which old
# signatures cannot be extended and users are trained to ignore the warning.
#
# Keyless cosign signs with an ephemeral key bound to the workflow's OIDC
# identity and records it in the public Rekor transparency log. There is no
# secret in the repository and none to lose. What a verifier checks is
# "GitHub Actions, this repository, this workflow, this tag" — provenance,
# which is the actual question, rather than "someone who held a key".
#
# The trade is that verification needs the cosign binary and network access to
# Rekor, where GPG needs neither. For a self-hosted tool whose users are
# already running `sha256sum`, that is acceptable; the checksum file remains
# useful on its own for anyone who will not install cosign.
#
# Only SHA256SUMS is signed, not each binary. It already commits to every
# artifact by hash, so one signature covers the release and verification stays
# two commands rather than 2N.
if [ "${SIGN:-0}" = "1" ]; then
  if ! command -v cosign >/dev/null 2>&1; then
    echo "SIGN=1 but cosign is not installed" >&2
    exit 1
  fi
  cosign sign-blob --yes \
    --output-signature "$OUT/SHA256SUMS.sig" \
    --output-certificate "$OUT/SHA256SUMS.pem" \
    "$OUT/SHA256SUMS"
  echo "signed SHA256SUMS (cosign keyless)"
fi
