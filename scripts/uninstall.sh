#!/usr/bin/env bash
# Removes everything written by `nimbus install`:
#   systemd unit, binary, service user, env file, sudoers rule, data dir.
# Prompts before deleting the database and config unless --yes is passed.
set -euo pipefail

APP_NAME="nimbus"
INSTALL_DIR="/opt/$APP_NAME"
DATA_DIR="/var/lib/$APP_NAME"
ENV_DIR="/etc/$APP_NAME"
SERVICE_FILE="/etc/systemd/system/${APP_NAME}.service"
SUDOERS_FILE="/etc/sudoers.d/$APP_NAME"

YES=0
for arg in "$@"; do
  [[ "$arg" == "--yes" || "$arg" == "-y" ]] && YES=1
done

if [ "$EUID" -ne 0 ]; then
  echo "error: must be run as root (sudo $0)"
  exit 1
fi

confirm() {
  [[ "$YES" -eq 1 ]] && return 0
  read -rp "  $1 [y/N]: " ans
  [[ "$ans" =~ ^[Yy]$ ]]
}

echo ""
echo "Nimbus Uninstaller"
echo "──────────────────────────────────────────────"

# Stop and disable service
if systemctl is-active --quiet "$APP_NAME" 2>/dev/null; then
  echo "  → Stopping service..."
  systemctl stop "$APP_NAME"
fi
if systemctl is-enabled --quiet "$APP_NAME" 2>/dev/null; then
  echo "  → Disabling service..."
  systemctl disable "$APP_NAME"
fi

# Remove systemd unit
if [ -f "$SERVICE_FILE" ]; then
  rm -f "$SERVICE_FILE"
  systemctl daemon-reload
  echo "  ✓ Removed $SERVICE_FILE"
fi

# Remove sudoers rule
if [ -f "$SUDOERS_FILE" ]; then
  rm -f "$SUDOERS_FILE"
  echo "  ✓ Removed $SUDOERS_FILE"
fi

# Remove binary
if [ -d "$INSTALL_DIR" ]; then
  rm -rf "$INSTALL_DIR"
  echo "  ✓ Removed $INSTALL_DIR"
fi

# Config + database — ask unless --yes
if [ -d "$ENV_DIR" ]; then
  if confirm "Delete config & database in $ENV_DIR and $DATA_DIR? (irreversible)"; then
    rm -rf "$ENV_DIR" "$DATA_DIR"
    echo "  ✓ Removed $ENV_DIR and $DATA_DIR"
  else
    echo "  ↷ Kept $ENV_DIR and $DATA_DIR"
  fi
fi

# Remove service user
if id "$APP_NAME" &>/dev/null; then
  userdel "$APP_NAME" 2>/dev/null || true
  echo "  ✓ Removed user $APP_NAME"
fi

echo ""
echo "✅ Nimbus uninstalled."
echo "   Run ./nimbus install to reinstall."
echo ""
