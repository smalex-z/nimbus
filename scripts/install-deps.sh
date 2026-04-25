#!/bin/bash
# Installs the system toolchain needed to build and run Nimbus from source:
# Node.js 18+ (frontend) and Go 1.22+ (backend). Idempotent — skips anything
# already at a satisfactory version. Apt-based hosts only (Ubuntu/Debian).

set -e

ROOT="$(cd "$(dirname "$0")/.." && pwd)"

NODE_MIN_MAJOR=18
GO_MIN_MAJOR=1
GO_MIN_MINOR=22

if ! command -v apt-get >/dev/null 2>&1; then
    echo "❌ This script only supports apt-based distros (Ubuntu/Debian)."
    echo "   Install Node.js 18+ and Go 1.22+ manually for your platform."
    exit 1
fi

if [ "$(id -u)" -ne 0 ]; then
    if ! command -v sudo >/dev/null 2>&1; then
        echo "❌ Not running as root and sudo is not installed."
        echo "   Re-run as root or install sudo first."
        exit 1
    fi
    SUDO="sudo"
else
    SUDO=""
fi

APT_UPDATED=0
apt_update_once() {
    if [ "$APT_UPDATED" -eq 0 ]; then
        echo "→ Updating apt package index..."
        $SUDO apt-get update -qq
        APT_UPDATED=1
    fi
}

# ---- Node.js -----------------------------------------------------------
need_node=1
if command -v node >/dev/null 2>&1; then
    node_major="$(node --version | sed -E 's/^v([0-9]+)\..*/\1/')"
    if [ "$node_major" -ge "$NODE_MIN_MAJOR" ] 2>/dev/null; then
        echo "✓ node $(node --version) already satisfies >= v${NODE_MIN_MAJOR}"
        if command -v npm >/dev/null 2>&1; then
            need_node=0
        else
            echo "  (npm missing — will install)"
        fi
    else
        echo "  node $(node --version) is older than v${NODE_MIN_MAJOR} — will reinstall"
    fi
fi

if [ "$need_node" -eq 1 ]; then
    apt_update_once
    echo "→ Installing nodejs + npm..."
    $SUDO apt-get install -y nodejs npm
fi

# ---- Go ----------------------------------------------------------------
need_go=1
if command -v go >/dev/null 2>&1; then
    go_ver="$(go version | sed -E 's/.*go([0-9]+)\.([0-9]+).*/\1 \2/')"
    go_major="$(echo "$go_ver" | awk '{print $1}')"
    go_minor="$(echo "$go_ver" | awk '{print $2}')"
    if [ "$go_major" -gt "$GO_MIN_MAJOR" ] 2>/dev/null \
       || { [ "$go_major" -eq "$GO_MIN_MAJOR" ] && [ "$go_minor" -ge "$GO_MIN_MINOR" ]; } 2>/dev/null; then
        echo "✓ $(go version | awk '{print $1, $3}') already satisfies >= go${GO_MIN_MAJOR}.${GO_MIN_MINOR}"
        need_go=0
    else
        echo "  $(go version | awk '{print $3}') is older than go${GO_MIN_MAJOR}.${GO_MIN_MINOR} — will reinstall"
    fi
fi

if [ "$need_go" -eq 1 ]; then
    apt_update_once
    echo "→ Installing golang-go..."
    $SUDO apt-get install -y golang-go
fi

echo ""
echo "✅ Dependencies ready. Next: ./scripts/build.sh"
echo "   ROOT=$ROOT"
