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

cd "$(dirname "$0")/.."

# GATE ZERO: every Docker base image must be digest-pinned before anything
# else runs. This script only builds Go binaries and touches no Dockerfile,
# but the release AS A WHOLE also publishes a Docker image (release.yml,
# `docker/build-push-action`), and an unpinned base image there breaks the
# same byte-identical-rebuild guarantee this script exists to enforce for the
# binaries. One release, one reproducibility promise - checked in one place
# rather than trusted separately per artifact type.
./scripts/check-pinned-images.sh

OUT=${OUT:-dist}
VERSION=${VERSION:-$(git describe --tags --always --dirty 2>/dev/null || echo dev)}

# The server platforms a v1 publishes. Each entry is GOOS/GOARCH.
PLATFORMS=${PLATFORMS:-"linux/amd64 linux/arm64 darwin/amd64 darwin/arm64"}

# The Windows DESKTOP binary is built separately, below, because it differs from
# the server builds in package, in linker flags, and in what it is claiming.
#
# The earlier note here said Windows "needs its own path handling for the config
# directory". That was investigated and found to be wrong: nothing hardcodes
# ~/.config. internal/db/migrate.go already calls os.UserConfigDir(), which
# returns %AppData% on Windows, and the download directory falls back through
# os.UserHomeDir(). The real question was whether the vendored Wails fork's
# RunWithStartURL patch covers the Windows backend, and it does — the Windows
# frontend reads the "starturl" context key and returns before the asset server
# is ever constructed, leaving f.assets nil so WebView2 handles every request as
# real HTTP. Same property as Linux, reached by an early return instead of an
# if/else. See third_party/wails-v2.13.0/PATCH.md.
BUILD_WINDOWS_DESKTOP=${BUILD_WINDOWS_DESKTOP:-1}

# THE FRONTEND MUST ALREADY BE BUILT before any go build below runs.
# internal/web/embed.go does `//go:embed all:dist`, which captures
# internal/web/dist byte-for-byte at compile time; a fresh checkout carries
# nothing there but the tracked .gitkeep (.gitignore excludes the rest).
# embed.go's own Handler() treats an unbuilt dist as a legitimate state ON
# PURPOSE — "a binary built without running the frontend build still starts and
# serves the API" — which is right for a plain `go build` during development and
# wrong for a release artifact: it produces a binary that builds cleanly,
# launches, and answers every UI request with {"error":"not_found"} until
# someone actually clicks around in it. CI (release.yml) and the Docker desktop
# stage both build the frontend before this script runs; this check exists so a
# UI-less binary cannot ship BECAUSE the script was run standalone, or with that
# upstream step skipped, without anyone noticing until a user did.
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

  # -trimpath and -buildvcs=false are what make two builds of one commit
  # byte-identical: trimpath removes the build directory from the binary, and
  # buildvcs=false stops Go stamping vcs.modified, which differs between a clean
  # checkout and a dirty working tree. Verified 2026-08-06: two clean clones at
  # different paths produced identical SHA256s.
  #
  # THAT GUARANTEE ALSO REQUIRES AN LF CHECKOUT, which no Go flag controls. The
  # embedded frontend is compiled from web/src, and a tree checked out with CRLF
  # feeds different bytes to vite: CRs inside multi-line template literals
  # survive minification as \r escapes, changing the JS bundle hash and so every
  # binary embedding it. .gitattributes pins eol=lf, but it landed in 65315b3
  # and git does not renormalise a tree checked out before it — and the
  # attribute normalises on read, so `git status` stays clean while the bytes on
  # disk are CRLF. Diagnosed 2026-08-11, when a local rebuild of v1.0.0-rc1
  # differed from the published binaries on identical source and an identical
  # toolchain. If a rebuild does not match, check line endings before suspecting
  # the compiler.
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

# ---- Windows desktop --------------------------------------------------------
#
# Not part of the PLATFORMS loop: that loop builds ./cmd/skein, the headless
# server, and this is ./cmd/skein-desktop, the windowed app. It also needs
# -H=windowsgui, which is meaningless for the others.
#
# NO CGO AND NO WAILS CLI, unlike the Linux desktop build. Linux needs cgo to
# link WebKitGTK, which is why `make desktop` shells out to the wails binary and
# cannot cross-compile. Windows drives WebView2 through COM, which Go reaches
# with pure syscall bindings, so a plain `go build` with CGO_ENABLED=0 produces
# a complete binary from a Linux host with no MinGW and no Windows machine.
#
# -tags desktop,production: `production` IS NOT OPTIONAL, and omitting it fails
# at run time rather than at build time. Wails splits CreateApp() by build tag —
# third_party/wails-v2.13.0/internal/app/app_default_windows.go carries
# `!dev && !production && !bindings` and its whole body is a MessageBox reading
# "Wails applications will not build without the correct build tags", followed
# by `return nil, nil`. The real implementation is app_production.go, gated on
# `production` alone. `wails build` never trips this because it appends the tag
# itself (pkg/commands/build/base.go:225, defaulted at cmd/wails/flags/build.go:80);
# a bare `go build` does not, and links the stub instead. That shipped in
# v1.0.0-rc1: the published Windows exe showed the dialog and exited for every
# user, on every launch, until this was found on 2026-08-11.
#
# app_default_unix.go carries the identical guard for linux/darwin, so this is
# not a Windows problem — it is what happens when the Wails CLI is bypassed, and
# Linux is exempt only because `make desktop` goes through it. THE STANDING
# CONSEQUENCE: any requirement the CLI injects automatically has to be tracked
# by hand here. Check base.go's buildCommands() before changing these flags.
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

  # THE FLAG IS VERIFIED, NOT ASSUMED. A typo in the -ldflags string, or a Go
  # release that stops honouring -H, would silently give back the console
  # window: the build still succeeds and the binary still runs, so nothing
  # fails until a user sees a stray terminal. Read the actual byte.
  #
  # PE layout: e_lfanew is a uint32 at 0x3C and points at "PE\0\0"; the COFF
  # header is 20 bytes; Subsystem is a uint16 at offset 68 of the optional
  # header, which is the same offset for PE32 and PE32+ (the two formats
  # diverge only after it).
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
