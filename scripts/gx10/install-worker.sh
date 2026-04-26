#!/usr/bin/env bash
# install-worker.sh — install the Nimbus GPU job worker as a systemd unit.
#
# Idempotent: re-running re-downloads the binary (so upgrades land), rewrites
# the env file, restarts the unit. Existing in-flight jobs receive SIGTERM
# from systemd and Nimbus reaps them on the next startup.
#
# Required env:
#   NIMBUS_URL              — base URL of the Nimbus instance (e.g. https://nimbus.example.com)
#   NIMBUS_WORKER_TOKEN     — bearer token from Settings → GPU
#
# Optional env:
#   GX10_WORKER_USER        — system user that runs the worker, default 'nimbus-worker'
#   GX10_WORKER_BIN_DIR     — default /opt/nimbus
#   GX10_WORKER_ID          — display name in /api/gpu/worker/* logs, default hostname
set -euo pipefail

if [ "$(id -u)" -ne 0 ]; then
  echo "must run as root (use sudo)" >&2
  exit 1
fi
: "${NIMBUS_URL:?NIMBUS_URL is required}"
: "${NIMBUS_WORKER_TOKEN:?NIMBUS_WORKER_TOKEN is required}"

USER_NAME="${GX10_WORKER_USER:-nimbus-worker}"
BIN_DIR="${GX10_WORKER_BIN_DIR:-/opt/nimbus}"
WORKER_ID="${GX10_WORKER_ID:-$(hostname)}"

echo "==> ensuring system user '$USER_NAME' exists (in docker group)"
if ! id -u "$USER_NAME" >/dev/null 2>&1; then
  useradd --system --create-home --shell /usr/sbin/nologin --groups docker "$USER_NAME"
else
  usermod -a -G docker "$USER_NAME" || true
fi

echo "==> ensuring $BIN_DIR"
mkdir -p "$BIN_DIR"

echo "==> downloading gx10-worker binary"
curl -fsSL -o "$BIN_DIR/gx10-worker" \
  "${NIMBUS_URL%/}/api/gpu/scripts/gx10-worker"
chmod +x "$BIN_DIR/gx10-worker"
chown "$USER_NAME:$USER_NAME" "$BIN_DIR/gx10-worker"

echo "==> writing /etc/nimbus-gpu-worker.env"
mkdir -p /etc
cat > /etc/nimbus-gpu-worker.env <<EOF
NIMBUS_URL=${NIMBUS_URL}
NIMBUS_WORKER_TOKEN=${NIMBUS_WORKER_TOKEN}
GX10_WORKER_ID=${WORKER_ID}
EOF
chown root:"$USER_NAME" /etc/nimbus-gpu-worker.env
chmod 640 /etc/nimbus-gpu-worker.env

echo "==> writing systemd unit /etc/systemd/system/nimbus-gpu-worker.service"
cat > /etc/systemd/system/nimbus-gpu-worker.service <<EOF
[Unit]
Description=Nimbus GPU job worker
After=network-online.target docker.service
Wants=network-online.target docker.service

[Service]
Type=simple
User=${USER_NAME}
Group=${USER_NAME}
EnvironmentFile=/etc/nimbus-gpu-worker.env
ExecStart=${BIN_DIR}/gx10-worker
Restart=on-failure
RestartSec=5
KillMode=mixed
KillSignal=SIGTERM
TimeoutStopSec=30

[Install]
WantedBy=multi-user.target
EOF

systemctl daemon-reload
systemctl enable --now nimbus-gpu-worker.service

echo "==> nimbus-gpu-worker.service status:"
systemctl --no-pager --lines=5 status nimbus-gpu-worker.service || true

echo ""
echo "Worker installed. Tail logs with:"
echo "  journalctl -u nimbus-gpu-worker -f"
