#!/bin/bash
# Production installation script — installs Homestack as a systemd service.
set -e

APP_NAME="homestack"
INSTALL_DIR="/opt/$APP_NAME"
SERVICE_USER="$APP_NAME"
DATA_DIR="/var/lib/$APP_NAME"

if [ ! -f "./homestack" ]; then
    echo "Error: './homestack' binary not found. Run './scripts/build.sh' first."
    exit 1
fi

echo "Installing $APP_NAME..."

# Create system user (ignore error if already exists)
sudo useradd -r -s /bin/false "$SERVICE_USER" 2>/dev/null || true

# Create directories
sudo mkdir -p "$INSTALL_DIR" "$DATA_DIR"

# Copy binary
sudo cp homestack "$INSTALL_DIR/"
sudo chmod 755 "$INSTALL_DIR/homestack"

# Create systemd service unit
sudo tee /etc/systemd/system/"$APP_NAME".service > /dev/null <<EOF
[Unit]
Description=$APP_NAME
Documentation=https://github.com/smalex-z/homestack
After=network.target

[Service]
Type=simple
User=$SERVICE_USER
WorkingDirectory=$DATA_DIR
ExecStart=$INSTALL_DIR/homestack
Restart=always
RestartSec=5
Environment=DB_PATH=$DATA_DIR/homestack.db

[Install]
WantedBy=multi-user.target
EOF

# Set ownership
sudo chown -R "$SERVICE_USER:$SERVICE_USER" "$INSTALL_DIR" "$DATA_DIR"

# Reload and enable
sudo systemctl daemon-reload
sudo systemctl enable "$APP_NAME"
sudo systemctl restart "$APP_NAME"

echo "✅ $APP_NAME installed and running"
echo "Check status: sudo systemctl status $APP_NAME"
echo "View logs:    sudo journalctl -u $APP_NAME -f"
