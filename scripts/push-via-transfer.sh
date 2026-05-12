#!/bin/bash
# push-via-transfer.sh — build a fresh Nimbus binary and upload it to a
# transfer.sh-compatible relay (transfer.uclaacm.com by default), then
# print a one-liner the operator can run on the target machine to fetch
# and install it.
#
# Use case: testing a build on a remote box without setting up SSH keys
# or publishing a GitHub release. The transfer service stores the
# binary for a short window (default 14 days); past that the URL 404s
# and you re-run the script for a fresh upload.
#
# Override the relay with NIMBUS_TRANSFER_URL=https://your-relay
# (must speak the transfer.sh API: PUT /<name> → URL of fetchable copy).
set -e

ROOT="$(cd "$(dirname "$0")/.." && pwd)"
TRANSFER_URL="${NIMBUS_TRANSFER_URL:-https://transfer.uclaacm.com}"

cd "$ROOT"

if ! command -v curl >/dev/null 2>&1; then
    echo "❌ curl is required" >&2
    exit 1
fi

echo "→ Building nimbus..."
./scripts/build.sh

if [ ! -f ./nimbus ]; then
    echo "❌ build did not produce ./nimbus" >&2
    exit 1
fi

size_bytes=$(stat -c%s ./nimbus 2>/dev/null || stat -f%z ./nimbus)
size_mb=$(( size_bytes / 1024 / 1024 ))
remote_name="nimbus-$(date +%Y%m%d-%H%M%S)"

echo ""
echo "→ Uploading ${size_mb} MiB to ${TRANSFER_URL}/${remote_name}..."
# --upload-file lets the relay set the file name from the URL path;
# --silent --show-error trims curl's progress bar but keeps the final
# URL on stdout. --fail surfaces non-2xx as an exit code.
url=$(curl --silent --show-error --fail --upload-file ./nimbus "${TRANSFER_URL}/${remote_name}")
if [ -z "$url" ]; then
    echo "❌ upload returned an empty URL" >&2
    exit 1
fi

echo "✓ Uploaded: ${url}"
echo ""
echo "On the target machine, run:"
echo ""
# -fL --progress-bar instead of -fsSL: progress bar stays visible so a
# slow/hung transfer.uclaacm.com download is diagnosable at a glance
# (the relay 502s + hangs intermittently — see /root/nimbus history).
# --connect-timeout caps the TCP handshake at 10s so a wedged DNS or
# unreachable IP fails fast instead of hanging forever; --max-time 300
# caps the whole transfer at 5min (binary is ~35MiB).
echo "    curl -fL --progress-bar --connect-timeout 10 --max-time 300 \\"
echo "      '${url}' -o /tmp/nimbus && \\"
echo "      chmod +x /tmp/nimbus && \\"
echo "      sudo /tmp/nimbus install"
echo ""
echo "(Re-running 'install' on a host that already has Nimbus swaps the"
echo "binary, re-writes the systemd unit, and restarts the service.)"
echo ""
echo "If 'install' hangs, the last partial '  → <step>...' line on the"
echo "target tells you which step is stuck; SIGQUIT the process for a"
echo "Go goroutine dump:  sudo pkill -QUIT -f '/tmp/nimbus install'"
