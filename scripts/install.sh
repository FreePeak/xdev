#!/usr/bin/env bash
# install.sh — the one command that installs xdev on Linux or macOS.
#
#   curl -fsSL https://raw.githubusercontent.com/FreePeak/xdev/main/scripts/install.sh | sh
#
# It resolves the newest published release, downloads this platform's
# binary, verifies its SHA-256 against the release's SHA256SUMS manifest,
# and installs it. Re-running the same command upgrades in place, so there
# is no separate "install" vs "update" step: this script always lands the
# newest release (and `xdev update` does the same thing from inside xdev).
#
# Environment (same knobs as `xdev update`, so a mirror works for both):
#   XDEV_UPDATE_REPO    owner/repo to install from   (default FreePeak/xdev)
#   XDEV_UPDATE_API     GitHub API root              (default https://api.github.com)
#   XDEV_INSTALL_DIR    install target directory     (default ~/.local/bin)
#   GITHUB_TOKEN        optional; lifts the anonymous 60 req/h API limit
#                      (unset and no gh CLI means an anonymous install, which
#                       is fine until the per-IP quota is spent)
#
#   NO_COLOR            set (to anything) forces the plain, colourless lines
#                       that a pipe gets anyway
# Windows has no shell here; grab xdev_windows_amd64.exe from the release page.
set -euo pipefail

REPO="${XDEV_UPDATE_REPO:-FreePeak/xdev}"
API="${XDEV_UPDATE_API:-https://api.github.com}"
INSTALL_DIR="${XDEV_INSTALL_DIR:-$HOME/.local/bin}"

# Colour, but only where it belongs: a terminal that asked for it. Captured
# output (CI, a pipe, `xdev update`'s subprocess) gets exactly the plain lines
# it got before — no escape codes to strip, nothing to break a log parser.
# The hues are the terminal's OWN palette (bold magenta title, green ticks, red
# errors, dim grue), not the TUI theme's 24-bit slots: the installer cannot see
# the background, and groknight's own accents are unreadable on a light one
# (measured: success #9ece6a is 1.8:1 on white, dim #585858 is 2.8:1 on black).
if [ -t 1 ] && [ "${TERM:-}" != dumb ] && [ -z "${NO_COLOR:-}" ]; then
  ESC="$(printf '\033')"
  C_MARK="${ESC}[32m"     # green — the ✓
  C_HEAD="${ESC}[1;35m"   # bold magenta — the title
  C_ERR="${ESC}[1;31m"    # bold red — xdev install:
  C_DIM="${ESC}[2m"       # dim — the quiet tail
  C_OFF="${ESC}[0m"
else
  ESC="" C_MARK="" C_HEAD="" C_ERR="" C_DIM="" C_OFF=""
fi

die() { printf '%sxdev install:%s %s\n' "$C_ERR" "$C_OFF" "$*" >&2; exit 1; }

# step <what happened> — one line per milestone, the way the install reads to
# a person: a green tick and the fact, nothing else.
step() { printf ' %s✓%s %s\n' "$C_MARK" "$C_OFF" "$*"; }

command -v curl >/dev/null 2>&1 || die "needs curl"
if command -v sha256sum >/dev/null 2>&1; then HASH_TOOL="sha256sum"
elif command -v shasum >/dev/null 2>&1; then HASH_TOOL="shasum -a 256"
else die "needs sha256sum or shasum to verify the download"
fi

case "$(uname -s)" in
  Darwin) GOOS=darwin ;;
  Linux)  GOOS=linux ;;
  *) die "unsupported OS $(uname -s) — on Windows use xdev_windows_amd64.exe from the release page" ;;
esac
case "$(uname -m)" in
  arm64|aarch64) GOARCH=arm64 ;;
  x86_64|amd64)  GOARCH=amd64 ;;
  *) die "unsupported CPU $(uname -m)" ;;
esac
# The asset name the release job publishes: xdev_<goos>_<goarch>.
ASSET="xdev_${GOOS}_${GOARCH}"

TMP="$(mktemp -d)"
trap 'rm -rf "$TMP"' EXIT

# api_get <api-path> <dest> — authenticated only when a token exists; a stale
# GITHUB_TOKEN is worse than none, so a 401 is reported as the reason.
api_get() {
  if [ -n "${GITHUB_TOKEN:-}" ]; then
    curl -fsSL -H "Authorization: Bearer ${GITHUB_TOKEN}" "${API}$1" -o "$2"
  else
    curl -fsSL "${API}$1" -o "$2"
  fi
}

printf '%s== xdev install (%s/%s) ==%s\n' "$C_HEAD" "$GOOS" "$GOARCH" "$C_OFF"
api_get "/repos/${REPO}/releases/latest" "$TMP/release.json" \
  || die "cannot read releases from ${API}/repos/${REPO} (anonymous rate limit? set GITHUB_TOKEN)"

# The release JSON is one line; pull the three fields we need by key.
TAG="$(sed -n 's/.*"tag_name":[[:space:]]*"\([^"]*\)".*/\1/p' "$TMP/release.json" | head -n 1)"
URL="$(sed -n 's/.*"browser_download_url":[[:space:]]*"\([^"]*\/'"$ASSET"'\)".*/\1/p' "$TMP/release.json" | head -n 1)"
SUMS_URL="$(sed -n 's/.*"browser_download_url":[[:space:]]*"\([^"]*\/SHA256SUMS\)".*/\1/p' "$TMP/release.json" | head -n 1)"

[ -n "$TAG" ] || die "no published release found in ${REPO} yet"
[ -n "$URL" ] || die "release ${TAG} carries no ${ASSET} binary"
# Mirrors `xdev update`: an unverifiable binary is never installed.
[ -n "$SUMS_URL" ] || die "release ${TAG} carries no SHA256SUMS manifest — refusing an unverified binary"
printf '%srelease:%s    %s\n' "$C_DIM" "$C_OFF" "$TAG"

curl -fsSL "$SUMS_URL" -o "$TMP/SHA256SUMS" || die "cannot download the checksum manifest"
WANT_SHA="$(awk -v a="$ASSET" '{ f = $2; sub(/^\*/, "", f); if (f == a) print $1 }' "$TMP/SHA256SUMS" | head -n 1)"
[ -n "$WANT_SHA" ] || die "no SHA-256 entry for ${ASSET} in ${TAG}"

curl -fsSL "$URL" -o "$TMP/xdev" || die "download of ${ASSET} failed"
echo ""
step "Downloaded" "$(awk -v b="$(wc -c <"$TMP/xdev")" 'BEGIN { printf "%.1f MB", b / 1000000 }')"
ACTUAL_SHA="$($HASH_TOOL "$TMP/xdev" | awk '{print $1}')"
[ "$ACTUAL_SHA" = "$WANT_SHA" ] \
  || die "SHA-256 mismatch: got ${ACTUAL_SHA}, manifest says ${WANT_SHA} — nothing installed"
step "Verified" "$ACTUAL_SHA"

mkdir -p "$INSTALL_DIR" || die "cannot create ${INSTALL_DIR}"
TARGET="${INSTALL_DIR}/xdev"
# A Homebrew-style symlink is replaced at the file it points at, not turned
# into a regular file that shadows the managed one (same rule as xdev update).
if [ -L "$TARGET" ]; then
  REAL="$(readlink -f "$TARGET" 2>/dev/null || true)"
  [ -n "$REAL" ] && TARGET="$REAL"
fi
# Remove before the move: replacing a signed Mach-O in place leaves the
# kernel with a cached signature that no longer matches, and macOS answers
# the next exec with SIGKILL ("Killed: 9").
rm -f "$TARGET"
mv "$TMP/xdev" "$TARGET" || die "cannot write ${TARGET} (permissions? try XDEV_INSTALL_DIR=\$HOME/.local/bin)"
chmod 0755 "$TARGET"

step "Installed" "$TARGET"
"$TARGET" version || die "${TARGET} was installed but did not run"

case ":$PATH:" in
  *":$INSTALL_DIR:"*) ;;
  *) echo ""
     printf '%s%s is not on PATH — add this to ~/.zshrc or ~/.bashrc:%s\n' "$C_DIM" "$INSTALL_DIR" "$C_OFF"
     echo "  export PATH=\"${INSTALL_DIR}:\$PATH\"" ;;
esac
echo ""
printf "%sLater upgrades: re-run this command, or \`xdev update\`.\n%s\n" "$C_DIM" "$C_OFF"
