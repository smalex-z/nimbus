# Nimbus

> Self-hosted VM provisioning portal for Proxmox VE clusters.

A user fills out one form (hostname, tier, OS, SSH key) and within ~60 seconds receives
a freshly-provisioned Linux VM with a static IP and SSH credentials. Like launching an
EC2 instance, but on hardware you control with zero cloud dependency.

Built on a multi-node Proxmox VE cluster. Originally designed for ACM@UCLA's internal
infrastructure, but architected to be general-purpose and deployable on any Proxmox cluster.

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

### Install from a release (recommended)

One-liner for any Linux host (Proxmox node or external VM). Downloads the latest
release binary to `/usr/local/bin/nimbus` and verifies its SHA256:

```bash
curl -fsSL https://raw.githubusercontent.com/smalex-z/nimbus/main/scripts/quickinstall.sh | bash

# While only pre-releases (alphas/betas) exist, add --prerelease:
curl -fsSL https://raw.githubusercontent.com/smalex-z/nimbus/main/scripts/quickinstall.sh | bash -s -- --prerelease

# Then run the wizard (prompts for Proxmox token, IP pool, gateway):
sudo nimbus install
```

Supported: Linux amd64, Linux arm64.

### Install on a Proxmox host (from source)

For unreleased changes or development builds. The wizard auto-detects when run
on a PVE host and defaults to `https://localhost:8006`:

```bash
# 1. Build the binary (cross-compile for the host arch)
./scripts/build.sh

# 2. Install as a systemd service (re-execs with sudo if needed)
sudo ./nimbus install

# 3. Open http://<host-ip>:8080 to complete setup in the web wizard
```

### Install on a guest VM

Same flow — the web wizard prompts for `PROXMOX_HOST`; point it at a cluster
node, e.g. `https://192.168.0.167:8006`.

### Local development

```bash
git clone https://github.com/smalex-z/nimbus.git
cd nimbus

# Install Go (per go.mod), Node 20, golangci-lint, plus jq/curl.
# Supports apt, dnf, pacman, and Homebrew. Idempotent — skips what's already there.
./scripts/install-deps.sh
# Or: ./scripts/install-deps.sh --check     to see what's missing without installing
#     ./scripts/install-deps.sh --dev-only  to skip jq/curl (the wizard prereqs)

# Create .env (copy from .env.example, fill in token + cluster info)
cp .env.example .env
$EDITOR .env

make dev
# Frontend hot reload:  http://localhost:5173
# Backend API:          http://localhost:8080
```

> Requirements: Go 1.22+, Node.js 18+ (the install script provisions newer versions matching `go.mod` and CI).

## Creating a Proxmox API token

In the Proxmox UI: **Datacenter → Permissions → API Tokens → Add**

- **User:** `root@pam` (or a dedicated service account)
- **Token ID:** `nimbus`
- **Privilege Separation:** **uncheck this** — otherwise the token has no permissions
- **Expire:** never (for development)

After clicking Add, copy the secret value **immediately** — Proxmox shows it only once.

The combined token ID for the env file is `root@pam!nimbus`.

## Templates — auto-bootstrap

Nimbus needs cloud-image templates on the cluster to provision VMs (the standard
"clone an image, customize via cloud-init" pattern). **The install wizard handles
this automatically** — you don't have to SSH into nodes or run `qm` commands.

After the install wizard configures and starts the service, it prompts:

```
Bootstrap templates now? [Y/n]:
```

Pressing Enter downloads the cloud images for all four OSes (Ubuntu 24.04/22.04,
Debian 12/11) on every online cluster node and converts them into Proxmox
templates. ~2 GB per node, parallel across nodes. Total time ~10-20 min on a
typical home lab.

The bootstrap creates these template VMIDs:

| OS | VMID |
|---|---|
| Ubuntu 24.04 LTS | 9000 |
| Ubuntu 22.04 LTS | 9001 |
| Debian 12 | 9002 |
| Debian 11 | 9003 |

Each template has cloud-init pre-installed, qemu-guest-agent ready, a cloud-init
drive attached, and is marked immutable. Re-running the bootstrap is idempotent:
already-existing templates are skipped.

### Manual bootstrap (re-runs, adding a new node, etc.)

Either via the API:

```bash
# Defaults: all 4 OSes, all online nodes
curl -X POST http://localhost:8080/api/admin/bootstrap-templates -d '{}'

# Subset
curl -X POST http://localhost:8080/api/admin/bootstrap-templates \
  -d '{"os":["ubuntu-24.04"], "nodes":["motik7"]}'
```

Or via the CLI:

```bash
./nimbus bootstrap                                # all 4 OSes, all online nodes
./nimbus bootstrap --os ubuntu-24.04              # one OS, all online nodes
./nimbus bootstrap --node motik7                  # all OSes, one node
./nimbus bootstrap --force                        # re-create even if exists
```

Both routes share the same code path. The CLI is just a convenience wrapper for
ops; the wizard uses the HTTP endpoint.

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
├── scripts/                    # build.sh, dev.sh, install-deps.sh, quickinstall.sh, reinstall.sh, uninstall.sh
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
| `POST /api/admin/bootstrap-templates` | Download cloud images and create templates (long-running, up to 30 min) |

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
