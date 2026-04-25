#!/bin/bash
# Production installation script — installs Homestack as a systemd service.
set -e

APP_NAME="nimbus"
INSTALL_DIR="/opt/$APP_NAME"
SERVICE_USER="$APP_NAME"
DATA_DIR="/var/lib/$APP_NAME"

# Check if running as root
if [ "$EUID" -ne 0 ]; then 
    SUDO="sudo"
else
    SUDO=""
fi

if [ ! -f "./nimbus" ]; then
    echo "Error: './nimbus' binary not found. Run './scripts/build.sh' first."
    exit 1
fi

echo "Installing $APP_NAME..."

# Create system user (ignore error if already exists)
$SUDO useradd -r -s /bin/false "$SERVICE_USER" 2>/dev/null || true

# Create directories
$SUDO mkdir -p "$INSTALL_DIR" "$DATA_DIR"

# Copy binary
$SUDO cp nimbus "$INSTALL_DIR/"
$SUDO chmod 755 "$INSTALL_DIR/nimbus"

# Create systemd service unit
$SUDO tee /etc/systemd/system/"$APP_NAME".service > /dev/null <<EOF
[Unit]
Description=$APP_NAME
Documentation=https://github.com/smalex-z/nimbus
After=network.target

[Service]
Type=simple
User=$SERVICE_USER
WorkingDirectory=$DATA_DIR
ExecStart=$INSTALL_DIR/nimbus
Restart=always
RestartSec=5
Environment=DB_PATH=$DATA_DIR/nimbus.db

[Install]
WantedBy=multi-user.target
EOF

# Set ownership
$SUDO chown -R "$SERVICE_USER:$SERVICE_USER" "$INSTALL_DIR" "$DATA_DIR"

# Reload and enable
$SUDO systemctl daemon-reload
$SUDO systemctl enable "$APP_NAME"
$SUDO systemctl restart "$APP_NAME"

echo "✅ $APP_NAME installed and running"
if [ -z "$SUDO" ]; then
    echo "Check status: systemctl status $APP_NAME"
    echo "View logs:    journalctl -u $APP_NAME -f"
else
    echo "Check status: sudo systemctl status $APP_NAME"
    echo "View logs:    sudo journalctl -u $APP_NAME -f"
fi
