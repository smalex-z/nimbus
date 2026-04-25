#!/bin/bash
# Reinstall: build a new binary and hot-swap the running systemd service.
set -e

ROOT="$(cd "$(dirname "$0")/.." && pwd)"
APP_NAME="nimbus"
INSTALL_DIR="/opt/$APP_NAME"

# Require root
if [ "$EUID" -ne 0 ]; then
  echo "Error: This script must be run as root (use: sudo ./scripts/reinstall.sh)"
  exit 1
fi

echo "Rebuilding $APP_NAME..."
"$ROOT/scripts/build.sh"

echo "Swapping binary..."
systemctl daemon-reload
systemctl stop "$APP_NAME"
cp "$ROOT/$APP_NAME" "$INSTALL_DIR/$APP_NAME"
chown "$APP_NAME:$APP_NAME" "$INSTALL_DIR/$APP_NAME"
systemctl start "$APP_NAME"

echo "✅ Reinstall complete"
systemctl status "$APP_NAME" --no-pager
