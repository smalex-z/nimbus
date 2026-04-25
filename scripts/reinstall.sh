#!/bin/bash
# Reinstall: build a new binary and hot-swap the running systemd service.
set -e

ROOT="$(cd "$(dirname "$0")/.." && pwd)"
APP_NAME="nimbus"
INSTALL_DIR="/opt/$APP_NAME"

echo "Rebuilding $APP_NAME..."
"$ROOT/scripts/build.sh"

echo "Swapping binary..."
sudo systemctl daemon-reload
sudo systemctl stop "$APP_NAME"
sudo cp "$ROOT/$APP_NAME" "$INSTALL_DIR/$APP_NAME"
sudo chown "$APP_NAME:$APP_NAME" "$INSTALL_DIR/$APP_NAME"
sudo systemctl start "$APP_NAME"

echo "✅ Reinstall complete"
sudo systemctl status "$APP_NAME" --no-pager
