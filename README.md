# Nimbus

> Self-hosted VM provisioning portal for Proxmox VE clusters, with optional public SSH tunnels and a GPU job plane.

A user signs in, fills out one form (hostname, tier, OS, SSH key) and ~60 seconds later has a freshly-provisioned Linux VM with a static IP, SSH credentials, and — optionally — a public SSH endpoint exposed via [Gopher](#phase-2--public-tunnels-gopher). Like launching an EC2 instance, but on hardware you control.

A single Go binary embeds the React SPA, talks to Proxmox over its REST API, and stores everything it needs in one local SQLite file. No Postgres, Redis, or external infrastructure — `one binary, one SQLite file` is an architectural invariant we defend on purpose.

Originally built for ACM@UCLA's internal cluster. A static UI mockup of the full multi-phase product is at [`nimbusx.html`](./nimbusx.html); the longer design doc is `nimbus-design-v03.docx`.

## What's shipping

| Phase | Status | What you get |
|---|---|---|
| 1 — VM provisioning | shipping | Web portal, 4 OS templates, atomic IP allocation, live cluster scoring, BYO or generated SSH keys |
| Auth | shipping | Email + password, GitHub OAuth, Google OAuth, access-code gate, admin/member roles |
| 2 — Public SSH tunnels | shipping | Optional Gopher integration — checkbox at provision time exposes the VM at `<host>:<port>` |
| 3 — Per-user S3 | planned | MinIO-backed object storage |
| 4 — GPU plane | shipping | Single GX10 box runs vLLM + a FIFO job worker; every VM gets `OPENAI_BASE_URL` and a `gx10` CLI helper |

VM **deletion** is the notable gap — it's not yet a first-class action in the UI.

## Quick start

### Install from a release (recommended)

One-liner for any Linux host (Proxmox node or external VM). Downloads the latest release binary to `/usr/local/bin/nimbus` and verifies its SHA256:

```bash
curl -fsSL https://raw.githubusercontent.com/smalex-z/nimbus/main/scripts/quickinstall.sh | bash

# While only pre-releases (alphas/betas) exist, add --prerelease:
curl -fsSL https://raw.githubusercontent.com/smalex-z/nimbus/main/scripts/quickinstall.sh | bash -s -- --prerelease

# Then install as a systemd service (re-execs with sudo if needed):
sudo nimbus install
# Add --upgrade to swap the binary in place without touching config or systemd unit
```

Supported: Linux amd64, Linux arm64.

### Install from source

For development builds or unreleased changes:

```bash
./scripts/build.sh        # cross-compiles for the host arch
sudo ./nimbus install     # writes systemd unit + sudoers rule, starts the service
```

The wizard auto-detects when run on a Proxmox host and defaults `PROXMOX_HOST=https://localhost:8006`. On a guest VM, the web wizard prompts for the cluster's address.

After install, open `http://<host-ip>:8080` and complete the **web setup wizard** — it collects Proxmox credentials, IP pool range, gateway, and (optionally) Gopher tunnel credentials, then offers to bootstrap templates on every online node before your first provision.

### Local development

```bash
git clone https://github.com/smalex-z/nimbus.git
cd nimbus

# Installs Go (per go.mod), Node 20, golangci-lint, jq, curl. Idempotent.
./scripts/install-deps.sh
# --check    print what's missing without installing
# --dev-only skip jq/curl (the wizard prereqs)

cp .env.example .env
$EDITOR .env

make dev
# Frontend hot reload:  http://localhost:5173
# Backend API:          http://localhost:8080
```

Requirements: Go 1.25+, Node.js 18+ (the install script provisions versions matching `go.mod` and CI).

### Other commands

| | |
|---|---|
| `sudo nimbus install --upgrade` | Replace binary, restart service. Config and systemd unit untouched. |
| `./scripts/reinstall.sh` | Local-source equivalent: build + hot-swap. Add `--clean` to wipe DB and config first. |
| `./scripts/uninstall.sh` | Remove systemd unit, binary, sudoers rule, service user. Prompts before deleting DB and config (or pass `--yes`). |

## Authentication

Nimbus requires sign-in. On a fresh install:

1. The first user to register is auto-promoted to admin (a startup migration also re-checks this on every boot — it's safe if you delete the admin user).
2. Optional: enable an **access code** in **Settings → Access Code**. When enabled, every non-admin user must enter the rotating code on the Verify page before any protected route works. Bumping the version invalidates everyone in one click — useful for end-of-quarter rotations.
3. Optional: enable OAuth providers in **Settings → Authentication**:
   - **GitHub** — set Client ID + Secret + (optionally) restrict to specific orgs.
   - **Google** — set Client ID + Secret + (optionally) restrict to specific authorized domains.

OAuth credentials live in the DB, not env vars; you can rotate them from the UI without restarting. Sessions are cookie-based.

## Web wizard / template bootstrap

Nimbus needs cloud-image templates on the cluster to provision VMs (the standard "clone an image, customize via cloud-init" pattern). **The setup wizard handles this automatically** — you don't have to SSH into nodes or run `qm` commands.

After the wizard configures and starts the service, it offers:

```
Bootstrap templates now? [Y/n]:
```

Pressing Enter downloads the cloud images for all four OSes on every online cluster node and converts them to Proxmox templates. ~2 GB per node, parallel across nodes; ~10–20 min total on a typical home lab.

Templates created:

| OS | VMID |
|---|---|
| Ubuntu 24.04 LTS | 9000 |
| Ubuntu 22.04 LTS | 9001 |
| Debian 12 | 9002 |
| Debian 11 | 9003 |

Each template has cloud-init pre-installed, qemu-guest-agent ready, a cloud-init drive attached, and is marked immutable. Re-running is idempotent — already-existing templates are skipped.

Manual bootstrap (re-runs, adding a new node, etc.) — same code path either way:

```bash
# CLI
./nimbus bootstrap                                # all 4 OSes, all online nodes
./nimbus bootstrap --os ubuntu-24.04              # one OS, all online nodes
./nimbus bootstrap --node motik7                  # all OSes, one node
./nimbus bootstrap --force                        # re-create even if exists

# HTTP (admin-only; same args)
curl -X POST http://localhost:8080/api/admin/bootstrap-templates -d '{}'
```

## Creating a Proxmox API token

In the Proxmox UI: **Datacenter → Permissions → API Tokens → Add**

- **User:** `root@pam` (or a dedicated service account)
- **Token ID:** `nimbus`
- **Privilege Separation:** **uncheck this** — otherwise the token has no permissions
- **Expire:** never (for development)

Copy the secret value **immediately** — Proxmox shows it only once. The combined token ID for the env file is `root@pam!nimbus`.

## Phase 2 — public tunnels (Gopher)

Gopher is ACM@UCLA's reverse-tunnel gateway (rathole + Caddy). When Gopher credentials are configured, the Provision form gains an **Expose SSH publicly** checkbox; tick it to register a Gopher machine alongside the VM and bootstrap the rathole client at provision time. The result screen and the **My machines** page then show both LAN and WAN SSH commands.

Configure under **Settings → Authentication → Gopher tunnels** (admin-only). The integration is fully optional — without credentials, every tunnel code path silently no-ops and the checkbox is disabled.

## Phase 4 — GPU plane

Two services run on a single GX10 (or any aarch64 NVIDIA box):

- **vLLM inference server** — always-on, OpenAI-compatible. Every Nimbus VM gets `OPENAI_BASE_URL` and a `gx10` CLI helper injected at provision time.
- **Job worker** — pulls queued training jobs from Nimbus, runs each in `docker run --gpus all`, streams logs back. One job at a time, FIFO.

No GPU is ever attached to a VM. The GX10 is not a Proxmox node — it's a peer service Nimbus hands out via env vars + an HTTP API.

### Setup

1. **Provision a GX10** with Docker + the [NVIDIA Container Toolkit](https://docs.nvidia.com/datacenter/cloud-native/container-toolkit/latest/install-guide.html). Verify with `docker run --rm --gpus all nvidia/cuda:12.4.0-base-ubuntu22.04 nvidia-smi`.
2. **In Nimbus**: **Settings → GPU → Add GX10**. Set base URL + model. The page mints a one-line `curl` command with a freshly minted pairing token.
3. **SSH to the GX10** and paste the curl. Two systemd units come up: `nimbus-vllm` and `nimbus-gpu-worker`.
4. **From any provisioned VM**:
   ```bash
   curl $OPENAI_BASE_URL/v1/models                          # inference
   gx10 submit pytorch/pytorch:latest -- python train.py    # training
   ```

Build the worker binary before tagging a release: `make gx10-worker` (cross-compiles ARM64 into `scripts/gx10/gx10-worker`, where the install script fetches it from). See [`scripts/gx10/README.md`](./scripts/gx10/README.md) for more.

## Configuration

Set via environment variables — typically loaded from `/etc/nimbus/nimbus.env` (production) or `./.env` (development). See [`.env.example`](./.env.example).

OAuth, Gopher, and GPU credentials can be set via env vars for first-boot seeding, but the Settings UI is the authoritative editor afterwards — changes there take effect without restart.

### Required

| Variable | Description |
|---|---|
| `PROXMOX_HOST` | Proxmox API base URL, e.g. `https://localhost:8006` |
| `PROXMOX_TOKEN_ID` | Format: `user@realm!tokenname` |
| `PROXMOX_TOKEN_SECRET` | UUID shown once when creating the token |
| `IP_POOL_START` | First IP in the VM pool |
| `IP_POOL_END` | Last IP in the VM pool |
| `GATEWAY_IP` | LAN gateway IP for cloud-init network config |

### Cluster + provisioning

| Variable | Default | Description |
|---|---|---|
| `PROXMOX_TEMPLATE_BASE_VMID` | `9000` | Base template VMID; `+0..+3` per OS |
| `NIMBUS_EXCLUDED_NODES` | — | Comma-separated nodes to skip in scoring |
| `VM_CPU_TYPE` | `x86-64-v3` | CPU type passed to Proxmox at clone time |
| `NAMESERVER` | `1.1.1.1 8.8.8.8` | Cloud-init DNS servers (space-separated) |
| `SEARCH_DOMAIN` | `local` | Cloud-init DNS search domain |
| `NIMBUS_VM_DISK_STORAGE` | `local-lvm` | Storage pool the disk gate checks for free space; empty disables the gate |
| `NIMBUS_MEM_BUFFER_MIB` | `256` | RAM headroom required above the tier's request |
| `NIMBUS_CPU_LOAD_FACTOR` | `0.5` | Share of a fresh VM's vCPUs the soft score assumes consumed (range 0.25–1.0) |

### Reconciliation + IP pool

| Variable | Default | Description |
|---|---|---|
| `RECONCILE_INTERVAL_SECONDS` | `60` | Background reconcile cadence; `0` disables |
| `RESERVATION_TTL_SECONDS` | `600` | Stale-reservation cutoff (10 min) |
| `VERIFY_CACHE_TTL_SECONDS` | `5` | How long `ListClusterIPs` snapshot is reused |
| `VACATE_MISS_THRESHOLD` | `3` | Consecutive missing reconciles before auto-vacating |

### Netscan (LAN host probe)

| Variable | Default | Description |
|---|---|---|
| `NIMBUS_NETSCAN_MODE` | `both` | `off` / `tcp` / `both` |
| `NIMBUS_NETSCAN_INTERVAL_SECONDS` | `300` | Sweep cadence; `0` disables |
| `NIMBUS_NETSCAN_TIMEOUT_MS` | `200` | Per-port TCP dial timeout |
| `NIMBUS_NETSCAN_CONCURRENCY` | `50` | Parallel probes per sweep |

### HTTP server + auth

| Variable | Default | Description |
|---|---|---|
| `PORT` | `8080` | HTTP server port |
| `DB_PATH` | `./nimbus.db` | SQLite database path |
| `APP_URL` | `http://localhost:5173` | Public URL — used as the OAuth redirect base |
| `CORS_ORIGIN` | `*` | CORS allowed origin |
| `APP_ENV` | `production` | Environment label |
| `GITHUB_CLIENT_ID` / `GITHUB_CLIENT_SECRET` | — | First-boot seed for GitHub OAuth (DB takes over) |
| `GOOGLE_CLIENT_ID` / `GOOGLE_CLIENT_SECRET` | — | First-boot seed for Google OAuth |
| `GOPHER_API_URL` / `GOPHER_API_KEY` | — | First-boot seed for Gopher tunnels |

## API

Routes are mounted under `/api`. The full list lives in [`internal/api/router.go`](./internal/api/router.go); the highlights:

| Group | Endpoints |
|---|---|
| Health | `GET /health`, `GET /tunnels/info` |
| Auth | `POST /auth/{register,login,logout}`, `GET /auth/{github,google}{,/callback}`, `GET /auth/providers`, `GET /me` |
| Access code | `GET /access-code/status`, `POST /access-code/verify` |
| VMs | `GET/POST /vms`, `GET /vms/{id}`, `GET /vms/{id}/private-key`, `GET/POST /vms/{id}/tunnels`, `DELETE /vms/{id}/tunnels/{tid}` |
| SSH keys | `GET/POST /keys`, `GET/DELETE /keys/{id}`, `GET/POST /keys/{id}/private-key`, `POST /keys/{id}/default` |
| GPU (user) | `GET /gpu/inference`, `GET/POST /gpu/jobs`, `GET /gpu/jobs/{id}`, `POST /gpu/jobs/{id}/cancel` |
| GPU (worker, bearer-token) | `POST /gpu/worker/{claim, jobs/{id}/logs, jobs/{id}/status}` |
| Admin observability | `GET /nodes`, `GET /ips`, `GET /cluster/{vms,stats}`, `POST /ips/reconcile` |
| Admin templates | `GET /admin/bootstrap-status`, `POST /admin/bootstrap-templates` |
| Admin settings | `GET/PUT /settings/{oauth,gopher,gpu,access-code,google-domains,github-orgs}` |

Provision request:

```json
{
  "hostname": "my-project",
  "tier": "medium",
  "os_template": "ubuntu-24.04",
  "generate_key": true,
  "public_tunnel": false
}
```

Provision response (200):

```json
{
  "success": true,
  "data": {
    "vmid": 342,
    "hostname": "my-project",
    "ip": "192.168.0.142",
    "username": "ubuntu",
    "os": "ubuntu-24.04",
    "tier": "medium",
    "node": "motik7",
    "ssh_private_key": "-----BEGIN OPENSSH PRIVATE KEY-----\n...",
    "key_name": "nimbus-my-project"
  }
}
```

## Architecture

```
nimbus/
├── cmd/
│   ├── server/             Go entry point — embeds frontend/dist
│   └── gx10-worker/        ARM64 GX10 worker daemon (Phase 4)
├── frontend/               React 18 + TS + Vite + Tailwind SPA
├── internal/
│   ├── api/                Chi router, middleware, handlers
│   ├── bootstrap/          Cloud-image download + template creation
│   ├── config/             Env-based config + .env loader
│   ├── ctxutil/            Request-context helpers (current user, …)
│   ├── db/                 GORM models, SQLite single-writer setup
│   ├── errors/             Typed error sentinels (ValidationError, ConflictError, …)
│   ├── gpu/                GX10 job queue + log storage (Phase 4)
│   ├── install/            Installer + setup-wizard mode
│   ├── ippool/             Atomic IP allocation + Proxmox reconciliation
│   ├── netscan/            LAN-host probe — fills the IP-pool with non-VM holders
│   ├── nodescore/          Pure node scoring (60% mem free, 40% cpu free)
│   ├── oauth/              GitHub + Google OAuth flows
│   ├── provision/          9-step VM lifecycle orchestrator
│   ├── proxmox/            Proxmox REST client (form-encoded, self-signed TLS)
│   ├── secrets/            AES-GCM encryption helpers (vault-stored keys)
│   ├── service/            Auth service (sessions, password hashing)
│   ├── sshkeys/            Per-user SSH key vault
│   └── tunnel/             Gopher reverse-tunnel HTTP client (Phase 2)
├── scripts/                build / dev / install / quickinstall / reinstall / uninstall
│   └── gx10/               Inference + worker installers (Phase 4)
└── .github/workflows/      build, test, lint, release
```

## Make commands

| Command | Description |
|---|---|
| `make dev` | Backend (:8080) + frontend dev server (:5173) with hot reload |
| `make build` | Production binary with embedded frontend |
| `make test` | `go test ./...` + `npm run type-check` |
| `make lint` | `golangci-lint run` + `npm run lint` |
| `make gx10-worker` | Cross-compile the GX10 worker for `linux/arm64` into `scripts/gx10/` |
| `make clean` | Remove build artifacts |

## Releases

```bash
make gx10-worker           # don't forget — install scripts fetch this binary
git tag v0.1.0-alpha.1
git push --tags
```

GitHub Actions builds Linux amd64/arm64 + macOS amd64 binaries and creates a release.

## License

[MIT](./LICENSE)
