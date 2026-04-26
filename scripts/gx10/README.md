# GX10 install scripts

Phase 4 of Nimbus adds a single-host GPU plane backed by an ASUS Ascent GX10
(or any equivalent NVIDIA Grace+Blackwell box running DGX OS / Ubuntu 22.04+
on aarch64). Two pieces install on the GX10:

- **`nimbus-vllm.service`** — vLLM serving an OpenAI-compatible inference
  API on port 8000. Always-on; every Nimbus VM gets `OPENAI_BASE_URL` set
  to point here.
- **`nimbus-gpu-worker.service`** — small Go daemon that polls Nimbus for
  queued training jobs, runs each in `docker run --gpus all`, and posts the
  exit status back. One job at a time, FIFO.

## One-line install (recommended)

From the Nimbus admin UI: **Settings → GPU → Add GX10**. The button hands
back a one-line `curl` that pre-fills `NIMBUS_URL` + the freshly minted
worker token. SSH into the GX10 and paste it:

```bash
sudo bash <(curl -fsSL https://nimbus.example.com/api/gpu/install.sh)
```

This downloads + runs `install-inference.sh` and `install-worker.sh` in
order.

## Prerequisites (one-time)

The GX10 must already have:

1. **Docker** with the **NVIDIA Container Toolkit** installed and configured
   so `docker run --rm --gpus all nvidia/cuda:12.4.0-base-ubuntu22.04 nvidia-smi`
   works. NVIDIA's [installation guide](https://docs.nvidia.com/datacenter/cloud-native/container-toolkit/latest/install-guide.html)
   covers this; on DGX OS it's pre-installed.
2. **Network reachability** to the Nimbus instance — the worker polls
   `${NIMBUS_URL}/api/gpu/worker/claim` every few seconds. Same LAN as the
   Proxmox cluster is the typical setup.
3. **Outbound HTTPS** from the GX10 — vLLM needs to download model weights
   from Hugging Face on first run.

## Verifying

After install, both units should be `active (running)`:

```bash
systemctl status nimbus-vllm
systemctl status nimbus-gpu-worker
```

From any machine on the LAN:

```bash
curl http://gx10.lan:8000/v1/models       # inference plane is up
journalctl -u nimbus-gpu-worker -f         # worker is polling
```

From inside any Nimbus-provisioned VM (the env vars are set automatically):

```bash
echo $OPENAI_BASE_URL                      # http://gx10.lan:8000/v1
gx10 jobs                                  # list jobs you've submitted
gx10 submit pytorch/pytorch:latest -- python train.py
```

## Re-running the install

Both scripts are idempotent. Re-running `install-worker.sh` re-downloads
the binary (so it picks up worker upgrades from a newer Nimbus) and
restarts the systemd unit. Re-running `install-inference.sh` upgrades the
pip-installed vLLM and reloads the service; weights cached under `HF_HOME`
survive.

## Uninstall

```bash
sudo systemctl disable --now nimbus-vllm nimbus-gpu-worker
sudo rm -f /etc/systemd/system/nimbus-{vllm,gpu-worker}.service
sudo rm -f /etc/nimbus-gpu-worker.env
sudo systemctl daemon-reload
# Optional: leave model weights + venv in place
sudo rm -rf /opt/vllm /opt/nimbus /var/lib/vllm
```

## Tuning

`install-inference.sh` accepts these env overrides (set before running):

| Variable | Default | Notes |
|---|---|---|
| `GX10_INFERENCE_MODEL` | `meta-llama/Llama-3.1-8B-Instruct` | Any HF repo id. Larger models need more RAM (`max-model-len` etc. via EXTRA_ARGS). |
| `GX10_INFERENCE_PORT` | `8000` | |
| `GX10_INFERENCE_HF_HOME` | `/var/lib/vllm/hf` | Persistent weight cache. |
| `GX10_INFERENCE_EXTRA_ARGS` | _(empty)_ | Passed verbatim to `vllm serve` (e.g. `--quantization fp8`). |

`install-worker.sh` accepts:

| Variable | Default | Notes |
|---|---|---|
| `GX10_WORKER_USER` | `nimbus-worker` | Must be in the `docker` group; the script handles this. |
| `GX10_WORKER_ID` | `$(hostname)` | Surfaces in Nimbus job records as `worker_id`. |
