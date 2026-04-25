// Package api wires the Chi router and middleware stack.
package api

import (
	"net/http"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/go-chi/chi/v5/middleware"

	"nimbus/internal/api/handlers"
	"nimbus/internal/ippool"
	"nimbus/internal/provision"
	"nimbus/internal/proxmox"
)

// Deps bundles the dependencies the router needs. A struct beats a long
// positional argument list and lets cmd/server compose at construction time.
type Deps struct {
	Provision *provision.Service
	Pool      *ippool.Pool
	Proxmox   *proxmox.Client
}

// NewRouter builds and returns the application router.
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

	r.Route("/api", func(r chi.Router) {
		r.Get("/health", health.Check)

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
	})

	return r
}
