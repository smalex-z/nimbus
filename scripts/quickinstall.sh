#!/usr/bin/env bash
# Nimbus quick installer — downloads the latest release binary to /usr/local/bin/nimbus.
#
# Usage:
#   curl -fsSL https://raw.githubusercontent.com/smalex-z/nimbus/main/scripts/quickinstall.sh | bash
#   curl -fsSL ... | bash -s -- --prerelease   # include pre-releases
#
# After install, run `sudo nimbus install` to launch the configuration wizard.
set -eu

REPO="smalex-z/nimbus"
INSTALL_DIR="/usr/local/bin"
BINARY_NAME="nimbus"
PRERELEASE=0

# ----------------------------------------------------------------------
# Output helpers
# ----------------------------------------------------------------------
if [ -t 1 ]; then
  C_RESET=$'\e[0m'; C_BOLD=$'\e[1m'; C_DIM=$'\e[2m'
  C_GREEN=$'\e[32m'; C_YELLOW=$'\e[33m'; C_RED=$'\e[31m'; C_BLUE=$'\e[34m'
else
  C_RESET=""; C_BOLD=""; C_DIM=""; C_GREEN=""; C_YELLOW=""; C_RED=""; C_BLUE=""
fi
info()  { printf "%s%s%s\n" "$C_BLUE" "$*" "$C_RESET"; }
ok()    { printf "%s✓%s %s\n" "$C_GREEN" "$C_RESET" "$*"; }
warn()  { printf "%s⚠%s %s\n" "$C_YELLOW" "$C_RESET" "$*"; }
err()   { printf "%s✗%s %s\n" "$C_RED" "$C_RESET" "$*" >&2; }
die()   { err "$*"; exit 1; }

usage() {
  cat <<'EOF'
Nimbus quick installer.

Usage:
  curl -fsSL https://raw.githubusercontent.com/smalex-z/nimbus/main/scripts/quickinstall.sh | bash
  curl -fsSL ... | bash -s -- --prerelease   # include pre-releases

After install, run `sudo nimbus install` to launch the configuration wizard.
EOF
  exit 0
}

# ----------------------------------------------------------------------
# Argument parsing
# ----------------------------------------------------------------------
for arg in "$@"; do
  case "$arg" in
    --prerelease) PRERELEASE=1 ;;
    -h|--help)    usage ;;
    *)            die "Unknown argument: $arg (try --help)" ;;
  esac
done

# ----------------------------------------------------------------------
# Platform detection
# ----------------------------------------------------------------------
[ "$(uname -s)" = "Linux" ] || die "Nimbus only runs on Linux."

ARCH=$(uname -m)
case "$ARCH" in
  x86_64)         ARCH="amd64" ;;
  aarch64|arm64)  ARCH="arm64" ;;
  *)              die "Unsupported architecture: $ARCH (supported: x86_64, aarch64)" ;;
esac

# ----------------------------------------------------------------------
# Fetcher selection
# ----------------------------------------------------------------------
if command -v curl >/dev/null 2>&1; then
  fetch()    { curl -fsSL "$1"; }
  fetch_to() { curl -fsSL "$1" -o "$2"; }
elif command -v wget >/dev/null 2>&1; then
  fetch()    { wget -qO- "$1"; }
  fetch_to() { wget -qO "$2" "$1"; }
else
  die "curl or wget is required."
fi

command -v sha256sum >/dev/null 2>&1 || warn "sha256sum not found — checksum verification will be skipped."

# ----------------------------------------------------------------------
# Discover release tag
# ----------------------------------------------------------------------
printf "\n%sNimbus quick installer%s\n" "$C_BOLD" "$C_RESET"
printf "%s%s%s\n\n" "$C_DIM" "Downloads the latest release binary to $INSTALL_DIR/$BINARY_NAME" "$C_RESET"

info "→ Looking up latest release..."
if [ "$PRERELEASE" -eq 1 ]; then
  TAG=$(fetch "https://api.github.com/repos/${REPO}/releases" 2>/dev/null \
    | grep -m1 '"tag_name"' | cut -d'"' -f4)
  TAG_KIND="latest (incl. pre-release)"
else
  TAG=$(fetch "https://api.github.com/repos/${REPO}/releases/latest" 2>/dev/null \
    | grep -m1 '"tag_name"' | cut -d'"' -f4)
  TAG_KIND="latest stable"
fi

if [ -z "${TAG:-}" ]; then
  die "Could not determine release tag. If only pre-releases exist, retry with --prerelease.
See https://github.com/${REPO}/releases"
fi
ok "Found ${TAG_KIND} release: ${TAG}"

# ----------------------------------------------------------------------
# Download + verify
# ----------------------------------------------------------------------
ASSET="nimbus-linux-${ARCH}"
BIN_URL="https://github.com/${REPO}/releases/download/${TAG}/${ASSET}"
SUMS_URL="https://github.com/${REPO}/releases/download/${TAG}/SHA256SUMS.txt"

TMPDIR=$(mktemp -d)
trap 'rm -rf "$TMPDIR"' EXIT

info "→ Downloading ${ASSET}..."
fetch_to "$BIN_URL" "$TMPDIR/$ASSET" || die "Failed to download $BIN_URL"

if command -v sha256sum >/dev/null 2>&1; then
  info "→ Verifying SHA256..."
  if fetch_to "$SUMS_URL" "$TMPDIR/SHA256SUMS.txt" 2>/dev/null; then
    EXPECTED=$(grep "${ASSET}" "$TMPDIR/SHA256SUMS.txt" | awk '{print $1}' | head -1 || true)
    if [ -z "$EXPECTED" ]; then
      warn "${ASSET} not listed in SHA256SUMS.txt — skipping verification."
    else
      ACTUAL=$(sha256sum "$TMPDIR/$ASSET" | awk '{print $1}')
      [ "$EXPECTED" = "$ACTUAL" ] || die "SHA256 mismatch! Expected $EXPECTED, got $ACTUAL"
      ok "SHA256 verified"
    fi
  else
    warn "Could not download SHA256SUMS.txt — skipping verification."
  fi
fi

chmod +x "$TMPDIR/$ASSET"

# ----------------------------------------------------------------------
# Install
# ----------------------------------------------------------------------
DEST="$INSTALL_DIR/$BINARY_NAME"
info "→ Installing to ${DEST}..."

if [ -w "$INSTALL_DIR" ]; then
  mv "$TMPDIR/$ASSET" "$DEST"
elif command -v sudo >/dev/null 2>&1; then
  sudo mv "$TMPDIR/$ASSET" "$DEST"
else
  die "$INSTALL_DIR is not writable and 'sudo' is not available. Re-run as root."
fi

ok "Nimbus ${TAG} installed to ${DEST}"

# ----------------------------------------------------------------------
# Next steps
# ----------------------------------------------------------------------
echo
printf "%sNext steps:%s\n" "$C_BOLD" "$C_RESET"
printf "  %ssudo nimbus install%s   # launch the configuration wizard (Proxmox token, IP pool, gateway)\n" "$C_BOLD" "$C_RESET"
printf "  %snimbus --version%s      # confirm the install\n" "$C_BOLD" "$C_RESET"
echo
printf "%sDocs:%s https://github.com/%s\n\n" "$C_DIM" "$C_RESET" "$REPO"
