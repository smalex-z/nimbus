#!/usr/bin/env bash
# install-inference.sh — install vLLM as a systemd unit on the GX10.
#
# Idempotent: re-running upgrades pip-installed vLLM, regenerates the unit,
# and reloads systemd. Existing model weights cached under HF_HOME survive.
#
# Required env (passed by the bootstrap curl from /api/gpu/install.sh):
#   NIMBUS_URL                    — informational; recorded in the unit file
#
# Optional env (with sensible defaults):
#   GX10_INFERENCE_MODEL          — HF model id, default meta-llama/Llama-3.1-8B-Instruct
#   GX10_INFERENCE_PORT           — default 8000
#   GX10_INFERENCE_USER           — system user that runs vLLM, default 'vllm'
#   GX10_INFERENCE_HF_HOME        — HF cache dir, default /var/lib/vllm/hf
#   GX10_INFERENCE_EXTRA_ARGS     — passed verbatim to `vllm serve` (e.g. quantization)
set -euo pipefail

if [ "$(id -u)" -ne 0 ]; then
  echo "must run as root (use sudo)" >&2
  exit 1
fi

MODEL="${GX10_INFERENCE_MODEL:-meta-llama/Llama-3.1-8B-Instruct}"
PORT="${GX10_INFERENCE_PORT:-8000}"
USER_NAME="${GX10_INFERENCE_USER:-vllm}"
HF_HOME="${GX10_INFERENCE_HF_HOME:-/var/lib/vllm/hf}"
EXTRA_ARGS="${GX10_INFERENCE_EXTRA_ARGS:-}"

echo "==> ensuring system user '$USER_NAME' exists"
if ! id -u "$USER_NAME" >/dev/null 2>&1; then
  useradd --system --create-home --shell /usr/sbin/nologin "$USER_NAME"
fi

echo "==> ensuring HF cache dir $HF_HOME"
mkdir -p "$HF_HOME"
chown -R "$USER_NAME:$USER_NAME" "$HF_HOME"

echo "==> installing python toolchain"
apt-get update -y
apt-get install -y --no-install-recommends python3 python3-pip python3-venv git curl

VENV="/opt/vllm/venv"
if [ ! -d "$VENV" ]; then
  echo "==> creating venv at $VENV"
  python3 -m venv "$VENV"
  chown -R "$USER_NAME:$USER_NAME" /opt/vllm
fi

echo "==> installing/upgrading vLLM"
sudo -u "$USER_NAME" "$VENV/bin/pip" install --upgrade pip wheel setuptools
# vLLM ARM/Grace builds: prefer the official wheel index when present, fall
# back to PyPI which now has aarch64 wheels for recent versions.
sudo -u "$USER_NAME" "$VENV/bin/pip" install --upgrade vllm

echo "==> writing systemd unit /etc/systemd/system/nimbus-vllm.service"
cat > /etc/systemd/system/nimbus-vllm.service <<EOF
[Unit]
Description=Nimbus inference server (vLLM)
After=network-online.target
Wants=network-online.target

[Service]
Type=simple
User=${USER_NAME}
Group=${USER_NAME}
Environment=HF_HOME=${HF_HOME}
Environment=NIMBUS_URL=${NIMBUS_URL:-unknown}
ExecStart=${VENV}/bin/vllm serve ${MODEL} --host 0.0.0.0 --port ${PORT} ${EXTRA_ARGS}
Restart=on-failure
RestartSec=10
TimeoutStartSec=15min
# vLLM downloads the model on first start; allow it time. Subsequent starts
# hit the HF cache and come up in seconds.

[Install]
WantedBy=multi-user.target
EOF

systemctl daemon-reload
systemctl enable --now nimbus-vllm.service

echo "==> nimbus-vllm.service status (give the model a minute to load on first run):"
systemctl --no-pager --lines=5 status nimbus-vllm.service || true

echo ""
echo "vLLM is starting at http://0.0.0.0:${PORT}"
echo "  test: curl http://localhost:${PORT}/v1/models"
