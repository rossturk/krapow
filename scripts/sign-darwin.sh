#!/usr/bin/env bash
# Codesign + notarize a single Darwin binary built by goreleaser.
#
# goreleaser invokes this as a builds.hooks.post step for every binary it
# produces (linux + darwin × amd64 + arm64). The first check exits early for
# anything non-Darwin so the linux builds aren't affected.
#
# Required env (from the release workflow's secrets):
#   APPLE_DEVELOPER_ID            — Apple ID email associated with the cert
#   APPLE_TEAM_ID                 — 10-char team identifier
#   APPLE_APP_SPECIFIC_PASSWORD   — app-specific password for notarytool
#
# The signing identity itself is matched by "Developer ID Application" — the
# apple-actions/import-codesign-certs step in CI imports the .p12 into a
# temporary keychain and codesign picks it up by that prefix. We don't have
# to thread the full identity string through here.
#
# Required tools (preinstalled on macos-latest runners):
#   codesign, ditto, xcrun notarytool
#
# Local-dev escape hatch: set KRAPOW_SKIP_NOTARIZE=1 to skip notarization
# (still codesigns). Useful when running `goreleaser release --snapshot`
# from a dev machine without notarytool credentials.

set -euo pipefail

BINARY="$1"   # absolute path to the built binary
TARGET="$2"   # goreleaser target string, e.g. "darwin_arm64_v8.0"

# Linux builds get this hook too — quietly skip them.
case "$TARGET" in
  darwin_*) ;;
  *) exit 0 ;;
esac

: "${APPLE_DEVELOPER_ID:?must be set}"
: "${APPLE_TEAM_ID:?must be set}"

echo "==> codesigning $BINARY ($TARGET)"
codesign --force --options runtime --timestamp \
    --sign "Developer ID Application" \
    "$BINARY"

if [[ "${KRAPOW_SKIP_NOTARIZE:-}" == "1" ]]; then
    echo "==> KRAPOW_SKIP_NOTARIZE=1, skipping notarization"
    exit 0
fi

: "${APPLE_APP_SPECIFIC_PASSWORD:?must be set}"

# notarytool accepts .zip/.dmg/.pkg, not raw Mach-O — wrap, submit, wait,
# discard. The notarization ticket is registered with Apple against this
# binary's signature; Gatekeeper consults Apple online on first launch.
# (Stapling isn't possible for raw binaries — it only applies to bundles.)
ZIP="${BINARY}.notarize.zip"
echo "==> notarizing $BINARY"
ditto -c -k --keepParent "$BINARY" "$ZIP"
xcrun notarytool submit "$ZIP" \
    --apple-id "$APPLE_DEVELOPER_ID" \
    --team-id "$APPLE_TEAM_ID" \
    --password "$APPLE_APP_SPECIFIC_PASSWORD" \
    --wait
rm "$ZIP"
echo "==> notarization accepted for $BINARY"
