// Package api wires the Chi router and middleware stack.
package api

import (
	"net/http"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/go-chi/chi/v5/middleware"

	"nimbus/internal/api/handlers"
	"nimbus/internal/bootstrap"
	"nimbus/internal/config"
	"nimbus/internal/ippool"
	"nimbus/internal/provision"
	"nimbus/internal/proxmox"
)

// Deps bundles the dependencies the router needs. A struct beats a long
// positional argument list and lets cmd/server compose at construction time.
type Deps struct {
	Provision *provision.Service
	Bootstrap *bootstrap.Service
	Pool      *ippool.Pool
	Proxmox   *proxmox.Client
	Config    *config.Config
	Restart   func()
}

// NewRouter builds and returns the application router for normal (configured) mode.
func NewRouter(d Deps) http.Handler {
	r := chi.NewRouter()

	r.Use(middleware.RequestID)
	r.Use(corsMiddleware)
	r.Use(loggingMiddleware)
	r.Use(recoveryMiddleware)
	r.Use(rateLimiter(100, 200))

	health := handlers.NewHealth(d.Proxmox)
	vms := handlers.NewVMs(d.Provision)
	nodes := handlers.NewNodes(d.Proxmox)
	ips := handlers.NewIPs(d.Pool)
	admin := handlers.NewAdmin(d.Bootstrap)
	setup := handlers.NewSetup(d.Config, d.Restart)

	r.Route("/api", func(r chi.Router) {
		r.Get("/health", health.Check)
		r.Get("/setup/status", setup.Status)

		r.Get("/nodes", nodes.List)
		r.Get("/ips", ips.List)

		// VM provisioning is long-running — bump the timeout on this route only.
		// Keep other routes at the default short timeout.
		r.Route("/vms", func(r chi.Router) {
			r.Use(middleware.Timeout(180 * time.Second))
			r.Get("/", vms.List)
			r.Post("/", vms.Create)
			r.Get("/{id}", vms.Get)
		})

		// Admin operations: template bootstrap. This can take 10-20 minutes
		// when downloading all 4 OSes across all online nodes — give it room.
		r.Route("/admin", func(r chi.Router) {
			r.Get("/bootstrap-status", admin.BootstrapStatus)
			r.With(middleware.Timeout(30*time.Minute)).Post("/bootstrap-templates", admin.BootstrapTemplates)
		})
	})

	return r
}

// NewSetupRouter builds a minimal router for unconfigured (setup) mode.
// Only setup and health routes are registered; all other API calls 404.
func NewSetupRouter(cfg *config.Config, restart func()) http.Handler {
	r := chi.NewRouter()

	r.Use(middleware.RequestID)
	r.Use(corsMiddleware)
	r.Use(loggingMiddleware)
	r.Use(recoveryMiddleware)

	setup := handlers.NewSetup(cfg, restart)

	r.Route("/api", func(r chi.Router) {
		r.Get("/health", func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"success":true,"data":{"status":"setup"}}`))
		})
		r.Get("/setup/status", setup.Status)
		r.Get("/setup/discover", setup.Discover)
		r.Post("/setup/test", setup.Test)
		r.Post("/setup/save", setup.Save)
	})

	return r
}
