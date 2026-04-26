#!/usr/bin/env bash
# install-inference.sh — install vLLM as a systemd-managed Docker container.
#
# Why Docker (not pip/venv): pip-built vLLM wheels target sm_70..sm_90 and
# link against libcudart.so.12, which doesn't work on Grace+Blackwell (sm_100,
# CUDA 13 host). The official vllm/vllm-openai image bundles a Blackwell-
# compatible CUDA + PyTorch + kernels, so we let it own the runtime. The
# host only needs Docker + nvidia-container-toolkit (already required for
# the GPU job worker).
#
# Idempotent: re-running pulls the latest image (or pinned tag), rewrites
# the unit, and restarts. Model weights cached under HF_HOME survive.
#
# Required env (passed by the bootstrap curl from /api/gpu/install.sh):
#   NIMBUS_URL                    — informational; recorded in the unit file
#
# Optional env (with sensible defaults):
#   GX10_INFERENCE_MODEL          — HF model id, default microsoft/Phi-3-mini-4k-instruct
#   GX10_INFERENCE_PORT           — default 8000
#   GX10_INFERENCE_HF_HOME        — HF cache dir, default /var/lib/vllm/hf
#   GX10_INFERENCE_IMAGE          — Docker image, default vllm/vllm-openai:latest
#                                   (pin to a tag for reproducibility)
#   GX10_INFERENCE_MAX_MODEL_LEN  — context window, default 4096
#   GX10_INFERENCE_EXTRA_ARGS     — passed verbatim to `vllm serve` after the
#                                   defaults (e.g. --quantization fp8)
set -euo pipefail

if [ "$(id -u)" -ne 0 ]; then
  echo "must run as root (use sudo)" >&2
  exit 1
fi

MODEL="${GX10_INFERENCE_MODEL:-microsoft/Phi-3-mini-4k-instruct}"
PORT="${GX10_INFERENCE_PORT:-8000}"
HF_HOME="${GX10_INFERENCE_HF_HOME:-/var/lib/vllm/hf}"
IMAGE="${GX10_INFERENCE_IMAGE:-vllm/vllm-openai:latest}"
MAX_MODEL_LEN="${GX10_INFERENCE_MAX_MODEL_LEN:-4096}"
EXTRA_ARGS="${GX10_INFERENCE_EXTRA_ARGS:-}"

echo "==> verifying docker + nvidia-container-toolkit"
if ! command -v docker >/dev/null 2>&1; then
  echo "docker not found — install Docker + NVIDIA Container Toolkit first" >&2
  echo "  https://docs.nvidia.com/datacenter/cloud-native/container-toolkit/latest/install-guide.html" >&2
  exit 1
fi
if ! docker info 2>/dev/null | grep -qi 'Runtimes:.*nvidia'; then
  echo "warning: docker daemon doesn't list the 'nvidia' runtime — vLLM will fail to see the GPU." >&2
  echo "         install/configure nvidia-container-toolkit before continuing." >&2
fi

echo "==> ensuring HF cache dir $HF_HOME"
mkdir -p "$HF_HOME"

echo "==> pre-pulling $IMAGE (~15GB, first run only)"
docker pull "$IMAGE"

# Stop any prior container so the new unit definition takes effect cleanly.
# `|| true` keeps re-runs idempotent on a fresh box where the unit doesn't exist.
echo "==> stopping any prior nimbus-vllm.service"
systemctl stop nimbus-vllm.service 2>/dev/null || true
docker rm -f nimbus-vllm 2>/dev/null || true

echo "==> writing systemd unit /etc/systemd/system/nimbus-vllm.service"
cat > /etc/systemd/system/nimbus-vllm.service <<EOF
[Unit]
Description=Nimbus inference server (vLLM, Docker)
After=docker.service network-online.target
Requires=docker.service
Wants=network-online.target

[Service]
Type=simple
Environment=NIMBUS_URL=${NIMBUS_URL:-unknown}
# Pull happens once at install time; ExecStartPre keeps idempotency on
# re-runs (in case the image was garbage-collected). The leading '-' lets
# the rm fail silently when the container isn't there.
ExecStartPre=-/usr/bin/docker rm -f nimbus-vllm
ExecStartPre=/usr/bin/docker pull ${IMAGE}
ExecStart=/usr/bin/docker run --rm --name nimbus-vllm \\
  --gpus all --network host --shm-size 8g --ipc=host \\
  -v ${HF_HOME}:/root/.cache/huggingface \\
  ${IMAGE} \\
    --model ${MODEL} \\
    --host 0.0.0.0 --port ${PORT} \\
    --max-model-len ${MAX_MODEL_LEN} \\
    ${EXTRA_ARGS}
ExecStop=/usr/bin/docker stop -t 30 nimbus-vllm
Restart=on-failure
RestartSec=10
# vLLM downloads weights on first run (multi-minute) — give the unit
# enough budget that systemd doesn't kill it mid-load. Subsequent starts
# hit the cache and come up in seconds.
TimeoutStartSec=20min

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
echo "  logs: journalctl -u nimbus-vllm -f"
