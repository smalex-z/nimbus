# Nimbus

> **Self-hosted cloud platform for Proxmox VE.** Provision VMs in 60 seconds, expose them on the public internet over your own tunnel gateway, run object storage and GPU inference next to them — all from one Go binary, on hardware you own.

A single Go binary embeds the React SPA, talks to Proxmox over its REST API, and persists to one local SQLite file. **No Postgres. No Redis. No external infrastructure.** That's an architectural invariant, not an oversight.

[![License](https://img.shields.io/badge/license-Apache%202.0-blue)](./LICENSE)
[![Go](https://img.shields.io/badge/go-1.25%2B-00ADD8)](https://go.dev)

---

## Why

- **AWS bills are a tax on hardware you already own.** Nimbus gives you the EC2 experience on your closet cluster.
- **Your data, your room.** Nothing in the data path Nimbus doesn't ship: Caddy for TLS, Gopher for tunnels, MinIO for S3, vLLM for inference. All open source, all running on your gear.
- **Multi-tenant out of the box.** Per-user quotas, per-user isolation (IDOR-audited), admin overrides, audit log. Real teams use it; not a toy.
- **One binary.** `curl | bash`, run the wizard, ship.

## Features

**VM provisioning** — Web portal, 4 OS templates (Ubuntu 22.04/24.04, Debian 11/12), atomic IP allocation, live cluster scoring (memory + CPU + disk + workload weighting), BYO or generated SSH keys with vault-stored private halves.

**Multi-tenancy** — Email + password, GitHub / Google OAuth, magic-link recovery via SMTP, GitHub-org and Google-domain allowlists, access-code gating, OAuth-only / passwordless workspace mode, self-service password change. Per-user quotas with admin overrides. Every privileged action audit-logged.

**Networking** — Two SDN primitives, picked at provision time:
- **Standalone** (default) — per-VM Simple zone with PVE-native SNAT. Each VM gets its own /24, host-local.
- **VPC** — VXLAN zone shared across nodes + dedicated gateway LXC for NAT egress. Multiple VMs in one private subnet, cross-node L2 reachability.

**Public HTTPS tunnels** — Optional [Gopher](https://github.com/smalex-z/gopher) integration. One checkbox at provision exposes SSH publicly. Add per-port HTTP/TCP/UDP tunnels to a running VM from the Networks tab — `subdomain.your-domain.com` with TLS via Caddy. Nimbus exposes its own dashboard at `cloud.your-domain.com` automatically. Works for VPC-isolated VMs (bootstrap runs over qemu-guest-agent, not the network).

**Object storage** — One-click MinIO deploy on a Nimbus VM. Per-user buckets with quota inheritance, native S3 SDK compatibility, `mc admin` from the dashboard.

**GPU plane** — Always-on vLLM inference server (OpenAI-compatible). Every VM gets `OPENAI_BASE_URL` injected at provision time. FIFO job worker for training runs in `docker run --gpus all`, streams logs back. One GPU host, one job at a time — multi-GPU scheduling is a future addition.

**Cluster operations** — Cordon and drain nodes (atomic state machine), live VM migrate (single + multi-select, async with re-attachable progress), per-node binding policy. Background tasks survive request lifetime; close the tab and re-attach.

**Live config** — OAuth, Gopher, GPU, networking, quota, SMTP changes take effect without restart. The DB is the source of truth; env vars seed once.

## Quick start

```bash
# Install the latest release binary (Linux amd64/arm64). Verifies SHA256.
curl -fsSL https://raw.githubusercontent.com/smalex-z/nimbus/main/scripts/quickinstall.sh | bash -s -- --prerelease

# Set up systemd + sudoers (re-execs with sudo if needed).
sudo nimbus install
```

Open `http://<host-ip>:8080`, run the web setup wizard (Proxmox creds → IP pool → gateway → optional Gopher creds), and accept the offer to bootstrap cloud-image templates on every node.

> **You'll need a Proxmox API token first.** In the PVE UI: Datacenter → Permissions → API Tokens → Add. **Uncheck Privilege Separation** (otherwise the token has no permissions). The combined token ID is `user@realm!tokenname`.

To upgrade later: `sudo nimbus install --upgrade` swaps the binary in place. To uninstall: `./scripts/uninstall.sh`.

## Development

```bash
git clone https://github.com/smalex-z/nimbus.git && cd nimbus
./scripts/install-deps.sh   # apt-only; installs Node 18+ and Go 1.25+ if missing
cp .env.example .env && $EDITOR .env
make dev                    # backend :8080, frontend :5173 with hot reload
```

After any change: `./scripts/reinstall.sh` builds, hot-swaps the binary, restarts the service. See [CLAUDE.md](./CLAUDE.md) for repo conventions.

## Configuration

Required env vars: `PROXMOX_HOST`, `PROXMOX_TOKEN_ID`, `PROXMOX_TOKEN_SECRET`, `IP_POOL_START`, `IP_POOL_END`, `GATEWAY_IP`. Everything else has sensible defaults.

The full list with defaults and tuning notes is in [`.env.example`](./.env.example). For OAuth, Gopher, GPU, SMTP, and networking settings, env vars are first-boot seeds — after that the **Settings** UI is the authoritative editor and changes take effect without restart.

## API

124 routes mounted under `/api`. The full OpenAPI spec is generated from [swag](https://github.com/swaggo/swag) annotations and served at **`/api/docs`** (SwaggerUI). The router itself lives in [`internal/api/router.go`](./internal/api/router.go).

## Architecture

```
cmd/server/             Entry point — embeds frontend/dist + GPU-host assets
cmd/gx10-worker/        ARM64 GPU-host worker daemon
frontend/               React 18 + TS + Vite + Tailwind
internal/
  api/                  Chi router + handlers + swagger spec
  provision/            9-step VM lifecycle orchestrator
  proxmox/              Proxmox REST client (form-encoded, self-signed TLS, SDN)
  ippool/               Atomic IP allocation + reconciliation
  standalonenet/        Per-VM Simple-zone networking
  vpcmgr/               VPC primitive — VXLAN + per-VPC gateway LXC
  gateway/              VPC gateway LXC lifecycle
  tunnel/               Gopher reverse-tunnel HTTP client
  selftunnel/           Nimbus's own Gopher self-bootstrap
  s3storage/            MinIO deploy + per-user buckets
  gpu/                  GPU job queue + log storage
  nodemgr/              Cordon / drain state machine
  operations/           Background-task framework (re-attachable async ops)
  service/              Auth, sessions, quotas, settings
  audit/                Audit-log writer + retention reaper
  db/                   GORM models, SQLite single-writer setup
  ...                   bootstrap, config, mail, oauth, sshkeys, secrets, …
```

Architectural invariants worth defending (more in [CLAUDE.md](./CLAUDE.md)):

- **One binary, one SQLite file, no external infra.** Resist proposals to add Postgres / Redis / etcd.
- **Local SQLite is a cache, Proxmox is the source of truth for IP claims.** The reconciler converges the cache to PVE, never the reverse.
- **SQLite single writer.** `db.New` caps `MaxOpenConns=1`; serial writes are the contract.

## Releases

```bash
make gx10-worker           # cross-compile the ARM64 worker before tagging
git tag v0.1.0-alpha.7
git push --tags
```

GitHub Actions builds Linux amd64/arm64 binaries and creates a release.

## License

[Apache 2.0](./LICENSE)
