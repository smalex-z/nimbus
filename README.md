# Nimbus

> Self-hosted VM provisioning portal for Proxmox VE clusters.

A user fills out one form (hostname, tier, OS, SSH key) and within ~60 seconds receives
a freshly-provisioned Linux VM with a static IP and SSH credentials. Like launching an
EC2 instance, but on hardware you control with zero cloud dependency.

Built on a multi-node Proxmox VE cluster. Architected to be general-purpose and deployable
on any Proxmox cluster.

A static UI mockup of the full multi-phase product is available at [`nimbusx.html`](./nimbusx.html).

## Phase 1 (this repo, MVP)

- ✅ Self-service VM provisioning via web portal
- ✅ Multi-OS templates: Ubuntu 24.04, Ubuntu 22.04, Debian 12, Debian 11
- ✅ SSH key bring-your-own or one-time Ed25519 generation
- ✅ Static IP allocation from a managed pool (atomic, conflict-free)
- ✅ Automatic node selection by live cluster scoring (60% memory free, 40% CPU free)
- ⏸ XL tier — admin approval required (not enabled until OAuth)
- ⏸ OAuth/SSO auth — Phase 1 ships without authentication
- ⏸ VM deletion endpoint — defer past MVP

Future phases (see `nimbus-design-v03.docx` for full design):
- Phase 2 — SSO login + user accounts; public HTTPS subdomains via reverse proxy + TLS; admin fleet dashboard with approval queue and IP pool visualization
- Phase 3 — Per-user S3 (MinIO) storage
- Phase 4 — GPU compute via ASUS Ascent GX10 bare metal

## Quick start

### Install on a Proxmox host (recommended)

The wizard auto-detects when run on a PVE host and defaults to `https://localhost:8006`:

```bash
# 1. Build the binary (cross-compile for the host arch)
./scripts/build.sh

# 2. Run the install wizard — prompts for token, IP pool, gateway
sudo ./scripts/install.sh

# 3. Open http://<host-ip>:8080 from any machine on your LAN
```

### Install on a guest VM

Same wizard, just answer the `PROXMOX_HOST` prompt with a cluster node IP:
`https://192.168.0.167:8006`.

### Local development

```bash
git clone https://github.com/smalex-z/nimbus.git
cd nimbus

# Create .env (copy from .env.example, fill in token + cluster info)
cp .env.example .env
$EDITOR .env

make dev
# Frontend hot reload:  http://localhost:5173
# Backend API:          http://localhost:8080
```

> Requirements: Go 1.22+, Node.js 18+

## Creating a Proxmox API token

In the Proxmox UI: **Datacenter → Permissions → API Tokens → Add**

- **User:** `root@pam` (or a dedicated service account)
- **Token ID:** `nimbus`
- **Privilege Separation:** **uncheck this** — otherwise the token has no permissions
- **Expire:** never (for development)

After clicking Add, copy the secret value **immediately** — Proxmox shows it only once.

The combined token ID for the env file is `root@pam!nimbus`.

## Templates required on the cluster

Each cluster node should have these template VMIDs available locally:

| OS | VMID |
|---|---|
| Ubuntu 24.04 LTS | 9000 |
| Ubuntu 22.04 LTS | 9001 |
| Debian 12 | 9002 |
| Debian 11 | 9003 |

All templates must:
- Be cloud images (not installer ISOs) — cloud-init pre-installed
- Have `qemu-guest-agent` installed and enabled
- Have a cloud-init drive attached (e.g. `qm set 9000 --ide2 local-lvm:cloudinit`)
- Be converted to Proxmox template format (immutable)

The install wizard checks for missing templates and warns per-node.

## Architecture

```
nimbus/
├── cmd/server/                 # Go entry point — embeds frontend/dist
├── frontend/                   # React 18 + TypeScript + Vite + Tailwind
├── internal/
│   ├── api/                    # Chi router, middleware, handlers
│   ├── proxmox/                # Proxmox REST API client (form-encoded, self-signed TLS)
│   ├── provision/              # 9-step VM lifecycle orchestration
│   ├── ippool/                 # SQLite IP allocation (atomic, transaction-safe)
│   ├── nodescore/              # Cluster node scoring function (pure)
│   ├── db/                     # GORM models
│   ├── config/                 # Env-based configuration with .env loader
│   └── errors/                 # Typed error sentinels
├── scripts/                    # build.sh, dev.sh, install.sh, reinstall.sh
└── .github/workflows/          # test.yml, lint.yml, build.yml, release.yml
```

## Configuration

Set via environment variables — typically loaded from `/etc/nimbus/nimbus.env` (production)
or `./.env` (development). See `.env.example`.

| Variable | Required | Description |
|---|---|---|
| `PROXMOX_HOST` | yes | Proxmox API base URL, e.g. `https://localhost:8006` |
| `PROXMOX_TOKEN_ID` | yes | Format: `user@realm!tokenname` |
| `PROXMOX_TOKEN_SECRET` | yes | UUID shown once when creating the token |
| `PROXMOX_TEMPLATE_BASE_VMID` | no (9000) | Base VMID — Ubuntu 24.04. +1 per OS. |
| `NIMBUS_EXCLUDED_NODES` | no | Comma-separated nodes to skip in scoring |
| `IP_POOL_START` | yes | First IP in the VM pool |
| `IP_POOL_END` | yes | Last IP in the VM pool |
| `GATEWAY_IP` | yes | LAN gateway IP for cloud-init network config |
| `NAMESERVER` | no | DNS servers (default `1.1.1.1 8.8.8.8`) |
| `SEARCH_DOMAIN` | no | DNS search domain (default `local`) |
| `PORT` | no (8080) | HTTP server port |
| `DB_PATH` | no | SQLite database path (default `./nimbus.db`) |

## API

| Endpoint | Purpose |
|---|---|
| `POST /api/vms` | Provision a VM (synchronous, 60–120s) |
| `GET /api/vms` | List all VMs |
| `GET /api/vms/{id}` | Get a single VM |
| `GET /api/nodes` | Live cluster status |
| `GET /api/ips` | IP pool state |
| `GET /api/health` | Liveness + Proxmox reachability |

Provision request:

```json
{
  "hostname": "my-project",
  "tier": "medium",
  "os_template": "ubuntu-24.04",
  "generate_key": true
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
    "ssh_private_key": "-----BEGIN OPENSSH PRIVATE KEY-----\n..."
  }
}
```

## Make commands

| Command | Description |
|---|---|
| `make dev` | Start backend (:8080) + frontend dev server (:5173) |
| `make build` | Build production binary with embedded frontend |
| `make test` | `go test -race ./...` + `npm run type-check` |
| `make lint` | `golangci-lint run` + `npm run lint` |
| `make clean` | Remove build artifacts |

## Releases

```bash
git tag v0.1.0-alpha.1
git push --tags
```

GitHub Actions builds Linux amd64/arm64 + macOS amd64 binaries and creates a release.

## License

[MIT](./LICENSE)
