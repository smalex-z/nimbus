# Homestack

> Production-ready template for self-hosted Go + React applications.

Single binary deployment, embedded frontend, pre-configured CI/CD, and systemd service setup.

## Using this template

Click **Use this template** on GitHub, then clone your new repo and run the rename script:

```bash
git clone https://github.com/your-username/your-app.git
cd your-app

# Replace every "homestack" reference with your app name (lowercase, no spaces)
APP=your-app
find . -not -path './.git/*' -not -path './node_modules/*' -type f \
  \( -name '*.go' -o -name '*.sh' -o -name '*.yml' -o -name '*.json' -o -name '*.mod' \) \
  -exec sed -i "s/homestack/$APP/g" {} +
mv scripts/build.sh scripts/build.sh  # no rename needed — APP_NAME is read from the variable
```

### What gets updated by the rename

| File | What changes |
|------|--------------|
| `go.mod` | Module name: `module homestack` → `module your-app` |
| `scripts/build.sh` | Binary output name and ldflags module path |
| `scripts/install.sh` | `APP_NAME`, binary path, systemd unit name |
| `scripts/reinstall.sh` | `APP_NAME` |
| `.github/workflows/release.yml` | Binary artifact names (`homestack-linux-amd64` → `your-app-linux-amd64`) |
| All `.go` import paths | `"homestack/internal/..."` → `"your-app/internal/..."` |
| `frontend/package.json` | Package name |

### What to update manually

These are content-specific and won't be right after a rename:

- **`internal/config/config.go`** — env var names (`DB_PATH`, `PORT`, `CORS_ORIGIN`) and defaults
- **`internal/db/`** — replace the example `users` table with your own models
- **`internal/service/example.go`** and **`internal/api/handlers/example.go`** — replace with your business logic
- **`internal/api/router.go`** — replace example routes with your own
- **`README.md`** — replace this file with your project's documentation
- **`frontend/src/`** — replace the example UI with your app

---

## Development

```bash
git clone https://github.com/your-username/your-app.git
cd your-app
make dev
```

- Frontend: <http://localhost:5173>
- Backend API: <http://localhost:8080>

> **Requirements:** Go 1.22+, Node.js 18+

## Production

**Download a pre-built binary from [Releases](../../releases):**

```bash
# Stable (replace your-app and version as appropriate)
wget https://github.com/your-username/your-app/releases/latest/download/your-app-linux-amd64
chmod +x your-app-linux-amd64

# Specific version (including pre-releases)
wget https://github.com/your-username/your-app/releases/download/v0.1.0-alpha.1/your-app-linux-amd64
chmod +x your-app-linux-amd64
```

Verify your download against checksums on the releases page.

**Or build from source:**

```bash
./scripts/build.sh
```

**Run:**

```bash
./your-app                  # start on :8080 (or PORT env var)
./your-app --port 9090      # custom port
./your-app --version        # print version
```

**Install as a systemd service (runs on boot, survives reboots):**

```bash
sudo ./scripts/install.sh

# Service management
sudo systemctl status your-app
sudo systemctl restart your-app
sudo journalctl -u your-app -f
```

**During development — hot-swap without re-installing:**

```bash
./scripts/reinstall.sh    # rebuild + swap binary in the running service
```

## Project Structure

```
homestack/
├── cmd/server/          # Go entry point (embeds frontend/dist)
├── frontend/            # React 18 + TypeScript + Vite + Tailwind CSS
├── internal/
│   ├── api/             # Chi router, middleware, handlers
│   ├── db/              # SQLite + GORM models
│   ├── service/         # Business logic layer
│   └── config/          # Environment-based configuration
├── scripts/             # build.sh, dev.sh, install.sh, reinstall.sh
└── .github/workflows/   # test.yml, lint.yml, build.yml, release.yml, close-issues.yml
```

## Tech Stack

| Layer      | Technology                           |
|------------|--------------------------------------|
| Backend    | Go 1.22+ · Chi router                |
| Frontend   | React 18 · TypeScript · Vite · Tailwind CSS |
| Database   | SQLite · GORM (pure Go, no CGO)      |
| Deployment | Single static binary · systemd       |

## Make Commands

| Command       | Description                                |
|---------------|--------------------------------------------|
| `make dev`    | Start backend + frontend dev servers       |
| `make build`  | Build production binary                    |
| `make test`   | Run Go tests + TypeScript type-check       |
| `make lint`   | Run golangci-lint + ESLint                 |
| `make clean`  | Remove build artifacts                     |

## Configuration

All options are set via environment variables:

| Variable      | Default           | Description                 |
|---------------|-------------------|-----------------------------|
| `PORT`        | `8080`            | HTTP server port            |
| `DB_PATH`     | `./homestack.db`  | SQLite database file path   |
| `CORS_ORIGIN` | `*`               | Allowed CORS origin         |
| `APP_ENV`     | `production`      | Application environment     |

## Releases

The release workflow triggers automatically on any `v*` tag and publishes binaries for `linux/amd64` and `linux/arm64`.

**Versioning scheme:**

| Tag format          | Type               | Notes                       |
|---------------------|--------------------|-----------------------------|
| `v1.0.0`            | Stable             | Published as latest release |
| `v1.0.0-rc.1`       | Release candidate  | Pre-release flag set        |
| `v0.1.0-beta.1`     | Beta               | Pre-release flag set        |
| `v0.1.0-alpha.1`    | Alpha              | Pre-release flag set        |

**To cut a release:**

```bash
git tag v0.1.0-alpha.1
git push --tags
```

CI builds both architectures, generates `SHA256SUMS.txt`, and publishes a GitHub release. Tags containing a hyphen are automatically marked as pre-releases.

## License

[MIT](./LICENSE)
