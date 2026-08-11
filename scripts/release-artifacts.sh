#!/usr/bin/env bash
#
# Build the release binaries and the checksum file that covers them.
#
set -euo pipefail

OUT=${OUT:-dist}
VERSION=${VERSION:-$(git describe --tags --always --dirty 2>/dev/null || echo dev)}

# The server platforms a v1 publishes. Each entry is GOOS/GOARCH.
PLATFORMS=${PLATFORMS:-"linux/amd64 linux/arm64 darwin/amd64 darwin/arm64"}

BUILD_WINDOWS_DESKTOP=${BUILD_WINDOWS_DESKTOP:-1}

if [ ! -f internal/web/dist/index.html ]; then
  echo "ERROR: internal/web/dist/index.html is missing." >&2
  echo "       internal/web/dist is unbuilt (only .gitkeep is tracked), so" >&2
  echo "       go:embed would ship a binary with no UI - it would build and" >&2
  echo "       run, and 404 on every page." >&2
  echo "       Run 'cd web && npm ci && npm run build' (or 'make web') first." >&2
  exit 1
fi

rm -rf "$OUT"
mkdir -p "$OUT"

echo "building $VERSION"
echo "  internal/web/dist/index.html present - frontend is built"

for platform in $PLATFORMS; do
  goos=${platform%%/*}
  goarch=${platform##*/}
  name="skein-${VERSION}-${goos}-${goarch}"
  CGO_ENABLED=0 GOOS="$goos" GOARCH="$goarch" go build \
    -trimpath \
    -buildvcs=false \
    -ldflags "-s -w -X main.version=${VERSION}" \
    -o "$OUT/$name" \
    ./cmd/skein

  echo "  $name  $(du -h "$OUT/$name" | cut -f1)"
done

# ---- Windows desktop --------------------------------------------------------
if [ "$BUILD_WINDOWS_DESKTOP" = "1" ]; then
  name="skein-desktop-${VERSION}-windows-amd64.exe"

  # -H=windowsgui sets the PE Subsystem field to 2 (WINDOWS_GUI) instead of 3
  # (WINDOWS_CUI). Without it the binary is a console app, and Windows opens a
  # black terminal window behind the app window that stays for the whole
  # session. Verified by reading the PE header, not by trusting the flag — see
  # the check immediately below.
  CGO_ENABLED=0 GOOS=windows GOARCH=amd64 go build \
    -trimpath \
    -buildvcs=false \
    -tags desktop,production \
    -ldflags "-s -w -H=windowsgui -X main.version=${VERSION}" \
    -o "$OUT/$name" \
    ./cmd/skein-desktop
  subsystem=$(od -An -tu2 -j "$(( $(od -An -tu4 -j 60 -N 4 "$OUT/$name" | tr -d ' ') + 24 + 68 ))" \
    -N 2 "$OUT/$name" | tr -d ' ')
  if [ "$subsystem" != "2" ]; then
    echo "ERROR: $name has PE Subsystem=$subsystem, expected 2 (WINDOWS_GUI)." >&2
    echo "       -H=windowsgui did not take; the binary would open a console" >&2
    echo "       window alongside the app. Subsystem 3 is WINDOWS_CUI." >&2
    exit 1
  fi

  echo "  $name  $(du -h "$OUT/$name" | cut -f1)  [PE Subsystem=2 GUI, verified]"
fi

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
