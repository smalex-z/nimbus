#!/bin/bash
# Reinstall: build a new binary and hot-swap the running systemd service.
# --clean  also wipes config and database (runs uninstall --yes then install)
set -e

ROOT="$(cd "$(dirname "$0")/.." && pwd)"
APP_NAME="nimbus"
INSTALL_DIR="/opt/$APP_NAME"

CLEAN=0
for arg in "$@"; do
  [[ "$arg" == "--clean" ]] && CLEAN=1
done

# Require root
if [ "$EUID" -ne 0 ]; then
  echo "Error: This script must be run as root (use: sudo ./scripts/reinstall.sh)"
  exit 1
fi

echo "Rebuilding $APP_NAME..."
"$ROOT/scripts/build.sh"

if [ "$CLEAN" -eq 1 ]; then
  echo "Clean reinstall — wiping existing install..."
  "$ROOT/scripts/uninstall.sh" --yes
  "$ROOT/$APP_NAME" install
else
  echo "Swapping binary..."
  systemctl daemon-reload
  systemctl stop "$APP_NAME"
  cp "$ROOT/$APP_NAME" "$INSTALL_DIR/$APP_NAME"
  chown "$APP_NAME:$APP_NAME" "$INSTALL_DIR/$APP_NAME"
  systemctl start "$APP_NAME"
fi

echo "✅ Reinstall complete"
systemctl status "$APP_NAME" --no-pager
