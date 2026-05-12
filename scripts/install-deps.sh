#!/bin/bash
# Installs the system toolchain needed to build and run Nimbus from source:
# Node.js 18+ (frontend) and Go 1.22+ (backend). Idempotent — skips anything
# already at a satisfactory version. Apt-based hosts only (Ubuntu/Debian).

set -e

ROOT="$(cd "$(dirname "$0")/.." && pwd)"

NODE_MIN_MAJOR=18
GO_MIN_MAJOR=1
GO_MIN_MINOR=22
# Version downloaded from go.dev when apt's golang-go is too old or
# missing. Bump together with go.mod's go directive when needed.
GO_INSTALL_VERSION="1.23.4"

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
# go_satisfies_min returns 0 iff $1 (path to a go binary) reports a
# version >= GO_MIN_MAJOR.GO_MIN_MINOR. Used twice: once for whatever
# `go` is on PATH, once for the canonical /usr/local/go install.
go_satisfies_min() {
    local go_path="$1"
    local v major minor
    v="$($go_path version 2>/dev/null | sed -E 's/.*go([0-9]+)\.([0-9]+).*/\1 \2/')"
    major="$(echo "$v" | awk '{print $1}')"
    minor="$(echo "$v" | awk '{print $2}')"
    if [ "$major" -gt "$GO_MIN_MAJOR" ] 2>/dev/null; then return 0; fi
    if [ "$major" -eq "$GO_MIN_MAJOR" ] && [ "$minor" -ge "$GO_MIN_MINOR" ] 2>/dev/null; then return 0; fi
    return 1
}

need_go=1
# Probe both PATH and /usr/local/go/bin — a previous tarball install
# may have landed at the canonical location without the symlink, so
# `command -v go` misses it.
go_found=""
if command -v go >/dev/null 2>&1; then
    go_found="$(command -v go)"
elif [ -x /usr/local/go/bin/go ]; then
    go_found="/usr/local/go/bin/go"
fi

if [ -n "$go_found" ]; then
    if go_satisfies_min "$go_found"; then
        echo "✓ $($go_found version | awk '{print $1, $3}') at $go_found satisfies >= go${GO_MIN_MAJOR}.${GO_MIN_MINOR}"
        # If go is installed but not on PATH (tarball at /usr/local/go
        # without symlink, or apt elsewhere), drop a symlink at
        # /usr/local/bin/go so subsequent build.sh runs find it
        # without depending on shell-rc PATH edits.
        if ! command -v go >/dev/null 2>&1; then
            echo "  go binary not on PATH — symlinking $go_found → /usr/local/bin/go"
            $SUDO ln -sf "$go_found" /usr/local/bin/go
        fi
        need_go=0
    else
        echo "  $($go_found version 2>/dev/null | awk '{print $3}') is older than go${GO_MIN_MAJOR}.${GO_MIN_MINOR} — installing official tarball"
    fi
fi

if [ "$need_go" -eq 1 ]; then
    # Apt's golang-go is too old on common Debian/Ubuntu LTS releases
    # (Bookworm ships 1.19, Jammy 1.18) and we need ≥1.22. Always
    # install the official tarball at /usr/local/go and symlink the
    # binary into /usr/local/bin/go so it's on the default PATH for
    # every shell — no .profile sourcing required, no race between
    # install-deps.sh and the build.sh that follows.
    arch="$(uname -m)"
    case "$arch" in
        x86_64)  go_arch="amd64" ;;
        aarch64) go_arch="arm64" ;;
        *)
            echo "❌ Unsupported architecture for Go install: $arch" >&2
            echo "   Install Go ${GO_INSTALL_VERSION}+ manually and re-run." >&2
            exit 1
            ;;
    esac
    tarball="go${GO_INSTALL_VERSION}.linux-${go_arch}.tar.gz"
    tmp_dir="$(mktemp -d)"
    echo "→ Downloading ${tarball}..."
    if ! curl -fsSL "https://go.dev/dl/${tarball}" -o "${tmp_dir}/${tarball}"; then
        echo "❌ Failed to download ${tarball}" >&2
        rm -rf "$tmp_dir"
        exit 1
    fi
    echo "→ Installing to /usr/local/go..."
    $SUDO rm -rf /usr/local/go
    $SUDO tar -C /usr/local -xzf "${tmp_dir}/${tarball}"
    rm -rf "$tmp_dir"
    # Symlink the binary onto a PATH that every shell (login or not)
    # already has. /usr/local/bin is on root's + every user's PATH on
    # Debian/Ubuntu by default.
    $SUDO ln -sf /usr/local/go/bin/go /usr/local/bin/go
    # Also drop a profile.d snippet so interactive logins pick up the
    # rest of the toolchain (gofmt, godoc, etc.) at /usr/local/go/bin.
    $SUDO tee /etc/profile.d/golang.sh >/dev/null <<'EOF'
# Added by Nimbus install-deps.sh — gives interactive logins access to
# the full Go toolchain at /usr/local/go/bin. The `go` binary is also
# symlinked at /usr/local/bin/go so non-login shells / scripts work
# without sourcing this file.
export PATH="/usr/local/go/bin:$PATH"
EOF
    echo "✓ Installed: $(/usr/local/go/bin/go version)"
fi

echo ""
echo "✅ Dependencies ready. Next: ./scripts/build.sh"
echo "   ROOT=$ROOT"
