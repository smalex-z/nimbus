# Using the Homestack Template

This guide explains how to clone and customize Homestack for your own self-hosted application.

## 1. Getting Started

```bash
# Clone the template
git clone https://github.com/smalex-z/homestack.git my-app
cd my-app

# Remove the template's git history and start fresh
rm -rf .git
git init
git add .
git commit -m "Initial commit from Homestack template"
```

Then update the module name in `go.mod`:

```
module github.com/you/my-app
```

And run a global search-replace of `homestack` → `my-app` in your editor.

## 2. Adding API Routes

Routes are defined in `internal/api/router.go`:

```go
r.Route("/api", func(r chi.Router) {
    r.Get("/health", handlers.Health)

    // Add your routes here:
    r.Get("/items", myHandler.ListItems)
    r.Post("/items", myHandler.CreateItem)
    r.Delete("/items/{id}", myHandler.DeleteItem)
})
```

Add corresponding handler files in `internal/api/handlers/`.

## 3. Adding Database Models

Open `internal/db/models.go` and add a new GORM model:

```go
type Item struct {
    gorm.Model
    Name        string `gorm:"not null" json:"name"`
    Description string `json:"description"`
}
```

Register it in `internal/db/db.go` inside the `AutoMigrate` call:

```go
if err := gormDB.AutoMigrate(&User{}, &Item{}); err != nil {
    return nil, err
}
```

GORM will create/update the table on next startup.

## 4. Adding React Pages

1. Create a file in `frontend/src/pages/`, e.g. `MyPage.tsx`:

```tsx
export default function MyPage() {
  return <h1>My Page</h1>
}
```

2. Register the route in `frontend/src/App.tsx`:

```tsx
import MyPage from '@/pages/MyPage'

<Route path="/my-page" element={<MyPage />} />
```

3. Add a navigation link in `frontend/src/components/Layout.tsx`:

```tsx
const navItems = [
  { label: 'Dashboard', path: '/' },
  { label: 'My Page', path: '/my-page' },
  { label: 'Settings', path: '/settings' },
]
```

## 5. Environment Configuration

All configuration lives in `internal/config/config.go`. Add a new field:

```go
type Config struct {
    // ...existing fields...
    MyFeatureEnabled bool
}

func Load() *Config {
    return &Config{
        // ...
        MyFeatureEnabled: os.Getenv("MY_FEATURE") == "true",
    }
}
```

Pass `cfg` through to the handlers that need it.

## 6. Deploying to Production

```bash
# 1. Build the single binary
./scripts/build.sh

# 2. Transfer to your server
scp homestack user@server:/tmp/

# 3. Install as a systemd service
sudo ./scripts/install.sh
```

The service will:
- Run as a dedicated system user (`homestack`)
- Restart automatically on failure
- Store the database in `/var/lib/homestack/homestack.db`
- Be manageable with standard systemd commands:

```bash
sudo systemctl status homestack
sudo systemctl restart homestack
sudo journalctl -u homestack -f
```

## 7. Customizing the Build

The build script `scripts/build.sh` compiles the frontend and then the Go binary. The Go binary embeds the compiled frontend via `//go:embed`, so deploying is always just a single file.

To cross-compile for a different platform:

```bash
GOOS=linux GOARCH=arm64 CGO_ENABLED=0 go build -o homestack-arm64 ./cmd/server
```

## Key Design Decisions

| Decision | Reason |
|---|---|
| Chi router | Lightweight, fast, 100% compatible with `net/http` |
| Vite | Instant HMR, simple embed-friendly build output |
| SQLite + GORM | Zero-config, portable, perfect for self-hosted |
| No auth built-in | Add your preferred system (JWT, OAuth2, etc.) |
| Tailwind CSS | Rapid styling without custom CSS files |
| Single binary | Simplest possible production deployment |
