# CLAUDE.md

Operating manual for AI coding agents working on this repo. Read this in full before
making changes; it captures the conventions and gotchas that aren't obvious from the
code alone. The user-facing docs live in [README.md](./README.md) — don't duplicate
them here.

## Agent rules

These come first because they govern everything else.

### Always ask clarifying questions when intent is unclear

If a request has more than one reasonable interpretation, **stop and ask** before
writing code. Cheap to ask; expensive to undo a wrong direction.

Concrete triggers:
- Multiple architectural approaches with different tradeoffs (e.g. "shared DB" vs
  "Proxmox-as-truth" — these belong in a question, not a coin flip).
- Scope ambiguity: "fix this" when "this" could be a one-line patch or a refactor.
- Behavior choices the code can't pick alone: defaults, retry counts, cache TTLs,
  whether to auto-resolve conflicts vs. surface them, opt-in vs. always-on.
- Anything destructive or "visible to others" (force-push, merging into main,
  deleting branches, posting to external services).

Use `AskUserQuestion` with concrete options when the choice is bounded; use plain
text questions for open-ended ones. **Don't proceed until you have an answer.**

### Keep this file current

When you learn something the next agent should know, update CLAUDE.md in the same PR.
Examples:
- A new convention the user has corrected you on twice.
- A gotcha that bit you (a Proxmox API quirk, a GORM pitfall, a flaky test).
- A new package or major architectural change.
- A workflow the user prefers (e.g. "merge main into feature branches, never rebase
  pushed branches").

Do **not** put ephemeral state here (current task, session notes, in-progress
plans). Those go in `~/.claude/plans/` or session memory.

### Stay in scope

A bug fix doesn't ship a refactor. A one-shot operation doesn't ship a helper.
Don't add error handling for impossible cases, comments that re-state the code,
or backwards-compat shims for code that hasn't been released. If you find drift
in adjacent code, mention it and ask before fixing it in the same PR.

---

## Project orientation

Nimbus is a self-hosted VM provisioning portal for Proxmox VE. A single Go binary
embeds a React SPA, exposes a small REST API, and orchestrates a 9-step provision
flow against the Proxmox API. State lives in a local SQLite file (`nimbus.db`).
See [README.md](./README.md) for what it does and how to run it.

The architectural invariant worth defending: **one binary, one SQLite file, no
external infrastructure**. Resist proposals to add Postgres, Redis, etcd, etc.
unless the user explicitly asks for them.

## Repository layout

```
cmd/server/             entry point + frontend embed
internal/
  api/                  Chi router, middleware, handlers
  config/               env-based config + .env loader
  db/                   GORM models, SQLite single-writer setup
  ippool/               atomic IP allocation + Proxmox reconciliation
  proxmox/              Proxmox REST client (form-encoded, self-signed TLS)
  provision/            9-step VM lifecycle orchestrator
  nodescore/            pure node scoring (60% mem free, 40% cpu free)
  install/              installer + setup-wizard mode
  service/              auth service (sessions, password hashing)
  oauth/                GitHub + Google OAuth flows
  tunnel/               Gopher reverse-tunnel HTTP client (Phase 2)
  errors/               typed error sentinels (ValidationError, ConflictError, …)
  ctxutil/              request-context helpers (current user, …)
  build/                build-time version info (ldflags)
frontend/               React 18 + TS + Vite + Tailwind SPA
scripts/                build.sh, dev.sh, install-deps.sh, quickinstall.sh, reinstall.sh, uninstall.sh
.github/workflows/      build, test, lint, release
```

## Coding conventions

### Go style

- **gofmt + go vet + golangci-lint v2.11.4** must all be clean before committing.
  Linters enabled: `govet`, `errcheck`, `staticcheck`, `ineffassign`, plus `gofmt` as
  formatter. Config in `.golangci.yml`.
- Wrap errors with context: `fmt.Errorf("descriptive context: %w", err)`. Never
  `return err` from a function whose name doesn't already explain the failure.
- **Small interfaces defined at the consumer** (Go's "accept interfaces, return
  structs"). Examples: `provision.ProxmoxClient`, `provision.IPVerifier`,
  `ippool.ClusterIPLister`, `handlers.reconcileRunner`.
- **Functional options for constructors with > 2 optional knobs.** Example:
  `ippool.NewReconciler(pool, px, WithStaleAfter(...), WithCacheTTL(...))`.
- Don't write multi-line doc comments unless the function has non-obvious
  behavior. One short line is the default.
- `_ = someFunc()` is the accepted way to discard errors when the failure is
  truly fine to ignore (e.g. defer-time `Release` on an already-free row).
  Never silently drop an error from the happy path.

### Tests

- **Table-driven**, `t.Parallel()` at every level that's safe, `-race` always.
- Test files live in `*_test.go` next to the code, in package `<name>_test` (external
  test package) so tests use only the exported surface. The reconciler has both styles —
  exposed helpers in `pool_test.go` use `_test` package; internal helpers stay in the
  same package.
- For SQLite-backed tests, use `t.TempDir()` + a fresh `db.New()` per test —
  matches production single-writer behavior. See `ippool/pool_test.go:newTestPool`.
- For Proxmox-backed tests, use `httptest.NewTLSServer` and the `newMockPVE`
  helper in `proxmox/client_test.go`. The client's `InsecureSkipVerify=true`
  makes self-signed certs work transparently.
- Coverage target is 80% on the package, but **the new code path** is what matters.
  Don't pad coverage on legacy code in the same PR.

### Frontend

- React 18 + TypeScript strict + Vite. State via `AuthContext` for auth-y things,
  local component state otherwise. There is no Redux/Zustand and the user has not
  asked for one.
- Tailwind + bespoke `nimbus/` component primitives (NimbusBlobs, NimbusBrand, …)
  — prefer composing those over importing a UI library.
- Run `npm run type-check` and `npm run lint` (ESLint flat config) before
  committing frontend changes.

## Build, test, lint workflow

The standard loop after a change:

```bash
go build ./...                                         # cheap sanity check
go test -race ./...                                    # full suite, ~3s
go vet ./...                                           # catches more than you'd expect
gofmt -l .                                             # must print nothing
$(go env GOPATH)/bin/golangci-lint run ./...           # final gate
```

For frontend changes also:

```bash
cd frontend && npm run type-check && npm run lint
```

`make test` and `make lint` run the equivalent.

If lint isn't installed locally:

```bash
go install github.com/golangci/golangci-lint/v2/cmd/golangci-lint@v2.11.4
```

## Git workflow

- **Branches:** feature work goes on `feat/<name>` (or `fix/<name>`) branched from
  `main`. The branch name is freeform — see the existing log for examples.
- **Commits:** short imperative subject + optional body explaining the *why*. No
  attribution / co-author lines (disabled globally per the user's
  `~/.claude/settings.json`).
- **Integrating main into a feature branch:** use **merge**, not rebase. The repo
  has prior `Merge branch 'main' into <feature>` commits — don't break the pattern.
  Rebase on a pushed branch needs a force-push and is discouraged.
- **Commit only when the user asks.** Same for push, merge, tag, release. These
  are "visible to others" actions; ask first.

## Architectural invariants worth defending

These are easy to violate by accident — push back if a change would erode them.

1. **Local SQLite is a cache, Proxmox is the source of truth for IP claims.** The
   reconciler converges the cache to Proxmox, never the reverse. If you find
   yourself writing "trust the local DB" logic for IP allocation, you're going
   the wrong way.
2. **`Pool.Reserve` returns the lowest-free IP.** If the verify-after-Reserve
   loop ever wants a *different* IP after a race-loss, it must **leapfrog** —
   `Reserve next` first, then `Release contested` — otherwise Reserve hands
   back the same IP it just released. See `provision/service.go:verifyAndRetryReserve`.
3. **SQLite single writer.** `db.New` caps `MaxOpenConns=1`. Don't add a separate
   `*gorm.DB` or worker pool; serial writes are the contract.
4. **Reservations are atomic via GORM transactions, not application-level locks.**
   If you're tempted to add a `sync.Mutex` around pool ops, check whether a
   tighter `WHERE status = 'free'` clause solves the race instead.
5. **Setup-wizard fork at startup.** When `!cfg.IsConfigured()`, only the setup
   router is mounted. Don't add code that assumes Proxmox is reachable before
   `IsConfigured()` returns true.

## Schema changes and data backfills

`db.New` runs `gormDB.AutoMigrate(...)` on every server boot, so adding a new
column to a struct in `internal/db/models.go` is enough to roll the schema
forward — no SQL migration files. **But existing rows get the column's zero
value**, which can quietly break things for pre-existing data:

- `is_admin bool default:false` was added later → every old user became a
  member, leaving deployments with no admin at all (`HasAnyUsers()` returned
  true so the setup wizard never re-prompted). The fix lives in
  `service.AuthService.PromoteFirstUserIfNoAdmin()` and runs from `main.go`
  on every startup. Idempotent — safe to call repeatedly.

When you add a column whose **zero value would mis-classify existing rows**
(default-deny flags, type discriminators, ownership pointers), pair the
struct change with a one-shot backfill in the relevant service package and
call it from `main.go` after `db.New`. Pattern:

```go
func (s *X) MigrateFooBackfill() (changed bool, err error) { … }   // idempotent
```

```go
// in main.go
if changed, err := svc.MigrateFooBackfill(); err != nil {
    log.Printf("warning: foo backfill failed: %v", err)
} else if changed {
    log.Printf("backfill: …")
}
```

`nimbus install --upgrade` replaces the binary and restarts the systemd unit,
so any backfill in `main()` runs on the first post-upgrade boot for free.

## Gotchas the codebase already documents (read before editing)

- **Proxmox API is form-encoded, not JSON.** `client.go:do` sets
  `application/x-www-form-urlencoded`; SSH keys must NOT be pre-URL-encoded.
- **Proxmox returns 500 (not 404) when a VMID is missing on a node** — the body
  contains "does not exist". `proxmox.GetVMConfig` normalizes both to
  `ErrNotFound`.
- **Cloud-init silently fails to apply if the template lacks a `cloudinit` drive.**
  `proxmox.TemplateExists` checks for it; the install wizard surfaces missing
  drives per-node.
- **`/nodes/{n}/qemu/{vmid}/clone` requires `target=` in the body** — without it,
  Proxmox clones onto the source node, defeating node selection.
- **Fake `time.Now()` in tests by injecting `WithClock`** — never mix injected
  clock domains with real-time stamps from `pool.Reserve` (which always uses
  `time.Now().UTC()`).

## When you change reconciliation, verification, or the IP pool

These three packages co-evolve. After any non-trivial edit:

1. Re-run the truth-table tests in `ippool/reconcile_test.go`.
2. Re-run the verify-loop tests in `provision/service_test.go`
   (`TestProvision_Verify*`).
3. Sanity-check the leapfrog invariant: a verify rejection must leave the pool
   in a state where the *next* `Reserve` cannot return the rejected IP.
4. Update the truth table in `~/.claude/plans/can-you-confirm-something-rustling-fog.md`
   if the decision policy changed.

## Gopher tunnel integration (Phase 2)

Gopher is ACM@UCLA's reverse-tunnel gateway (rathole + Caddy). Nimbus uses its
external API to expose VMs at public hostnames. The integration is optional —
when no credentials are configured, every tunnel code path silently no-ops.

### Configuration source of truth: the DB, not env vars

Credentials live in `db.GopherSettings` (singleton, ID=1). Admins manage them
from **Settings → Gopher tunnels**; saving rebuilds the live `tunnel.Client`
and pushes it to every registered consumer (`provision.Service` via
`SetTunnelClient`, the admin tunnels handler via `SetClient`) — **no restart
required**.

Backwards compat: on startup, if the DB row is empty AND `GOPHER_API_URL` /
`GOPHER_API_KEY` env vars are set, `main.go` seeds the DB once and logs the
migration. After that, env vars are ignored. Old `.env`-based deployments
continue to work without operator intervention.

Plumbing the live-reload uses two small interfaces in
`internal/api/handlers/settings.go`:

```go
type TunnelClientApplier interface { SetTunnelClient(*tunnel.Client) }
type TunnelInfoSetter   interface { SetClient(c *tunnel.Client, apiURL string) }
```

`Settings.WithTunnelAppliers(...)` and `WithTunnelInfoSetter(...)` register
them at construction time in the router.

### Gopher API model: machines vs tunnels

Gopher exposes two related but distinct objects, and confusing them was
expensive to find out:

- **Machine** (`/api/v1/machines`) — a registered host running the rathole
  client. Created with `{public_ssh: true}` to flip on SSH exposure. The
  response carries the **bootstrap_url** (one-shot URL the VM runs to link
  itself). When the VM finishes the bootstrap, the machine flips from
  `pending` → `active` and Gopher exposes SSH at the gateway on an
  assigned port.

- **Tunnel** (`/api/v1/tunnels`) — a *per-port* exposure on top of an
  active machine. Body is `{machine_id, target_port}`. Used to expose
  HTTP/custom services (port 80, 8080, etc.) on top of a machine that
  already has its rathole link established. Returns 404 "machine not
  found" if the machine is still `pending`.

**Provision-time SSH exposure uses the machines API only.** Per-port
tunnels are a future post-provision surface; we don't touch
`POST /tunnels` at provision time. The earlier design doc described a
single-tier model (`POST /tunnels {subdomain, target_ip, target_port}`)
that doesn't match the live API — we discovered the discrepancy on the
first real provision (HTTP 400 "machine_id and target_port are required").

### Provision-time exposure UX

One checkbox on the Provision form: **Expose SSH publicly**. No subdomain,
no port input. The form only has the toggle.

- Subdomains are HTTP-only and aren't accepted by `POST /machines`. The
  `Request.Subdomain` and `Request.TunnelPort` fields persist on the
  Go-side type for forward-compat with the post-provision tunnel
  surface, but the provision flow ignores them.
- The SSH connection lands at `<host>:<port>` where both are assigned
  by Gopher when the machine becomes active.

### Routing-host preview (`/api/tunnels/info`)

`GET /api/tunnels/info` is public (no auth) and returns `{enabled, host}`
where `host` is the **routable hostname** Gopher will expose SSH on:

- Compares DNS for the apex domain (e.g. `altsuite.co`) against the API
  host (e.g. `router.altsuite.co`). If they resolve to the same IP, the
  apex doubles as the gateway → return apex (shorter, friendlier). If
  they diverge — operator runs a personal site on the apex — return the
  API host. Any DNS failure → fall back to the API host.
- Apex extraction is naive: strip the leftmost label. Fine for
  `router.example.com` deployments; would need a public-suffix list to
  handle `example.co.uk` correctly.

### Provision flow with tunnel enabled

1. **After Reserve + Verify**: `POST /machines {public_ssh: true}` →
   `{id, bootstrap_url, status: pending}`. Failures are soft (logged,
   `tunnel_error` set, VM still provisioned). On success: defer-delete
   armed for any later failure.
2. **After WaitForIP**: if the VM is reachable (no warning), Nimbus SSHes
   in with the resolved private key and runs
   `curl -fsSL <bootstrap_url> | sh`. Dial+handshake retries 3× with 5 s
   back-off; the remote command itself is single-shot. On soft-success
   (VM unreachable from Nimbus), the bootstrap is **skipped** and
   `tunnel_error` carries the manual recovery command.
3. **Poll** `GET /machines/:id` every 3 s for up to 60 s. Status `active`
   → use `public_ssh_host` + `public_ssh_port` to build the result URL,
   persist on the VM row. Otherwise → `tunnel_error` set, machine **left
   registered** so the user can retry the bootstrap manually.
4. **VM-side never fails for tunnel reasons** — design §10 invariant.

The bootstrap step needs a private key. If only a public key is in the
linked SSHKey row (user imported a pubkey-only entry, or attached one
later), the bootstrap is skipped with
`tunnel_error="private half not available"`.

The exact field names Gopher returns for an *active* machine
(`public_ssh_host`, `public_ssh_port`) are educated guesses from the
pending-state shape — only confirmable by completing a real bootstrap.
If the names diverge, the decoder will quietly populate them as zero,
and the result-screen URL will be missing/empty until we adjust.

### Gopher API envelope

Gopher wraps every response in `{success, data, error}`. List endpoints
nest pagination inside data: `{items, limit, offset, total}`. The decoder
in `internal/tunnel/client.go` handles both — confirmed against
`https://router.altsuite.co`. Errors carry the envelope's `error` field;
404 on DELETE is treated as success (idempotent retry).

## Configuration knobs added in this branch

| Env var | Default | Effect |
|---|---|---|
| `RECONCILE_INTERVAL_SECONDS` | 60 | Background reconcile cadence; 0 disables the loop |
| `RESERVATION_TTL_SECONDS` | 600 | Stale-reservation cutoff (10 min) |
| `VERIFY_CACHE_TTL_SECONDS` | 5 | How long `ListClusterIPs` snapshot is reused for `VerifyFree` |
| `VACATE_MISS_THRESHOLD` | 3 | Consecutive missing reconciles before auto-vacating an allocated row |
| `NIMBUS_VM_DISK_STORAGE` | `local-lvm` | Proxmox storage pool the disk gate checks for free space; empty disables the disk gate (scorer reverts to mem+cpu) |
| `NIMBUS_MEM_BUFFER_MIB` | 256 | RAM headroom required above the tier's request — avoids packing a node to literal zero free |
| `NIMBUS_CPU_LOAD_FACTOR` | 0.5 | Share of a fresh VM's vCPUs the soft score assumes consumed (range 0.25–1.0) |

When tuning, remember: lower `VERIFY_CACHE_TTL_SECONDS` tightens the race window
at the cost of more Proxmox API calls; higher `VACATE_MISS_THRESHOLD` tolerates
longer migrations at the cost of slower convergence after manual deletions.
