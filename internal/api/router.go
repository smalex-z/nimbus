// Package api wires the Chi router and middleware stack.
package api

import (
	"net/http"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/go-chi/chi/v5/middleware"

	"nimbus/internal/api/handlers"
	"nimbus/internal/ippool"
	"nimbus/internal/oauth"
	"nimbus/internal/provision"
	"nimbus/internal/proxmox"
	"nimbus/internal/service"
)

// Deps bundles the dependencies the router needs.
type Deps struct {
	Auth      *service.AuthService
	GitHub    oauth.Provider
	Google    oauth.Provider
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

		// VM provisioning — long-running, gets its own timeout.
		r.Route("/vms", func(r chi.Router) {
			r.Use(middleware.Timeout(180 * time.Second))
			r.Get("/", vms.List)
			r.Post("/", vms.Create)
			r.Get("/{id}", vms.Get)
		})

		// Auth routes (public)
		auth := handlers.NewAuth(d.Auth, d.GitHub, d.Google)
		r.Post("/auth/register", auth.Register)
		r.Post("/auth/login", auth.Login)
		r.Post("/auth/logout", auth.Logout)
		r.Get("/auth/github", auth.GitHubStart)
		r.Get("/auth/github/callback", auth.GitHubCallback)
		r.Get("/auth/google", auth.GoogleStart)
		r.Get("/auth/google/callback", auth.GoogleCallback)

		// Admin status (public — needed by frontend before login)
		admin := handlers.NewAdmin(d.Auth)
		r.Get("/admin/status", admin.Status)

		// Protected routes — require a valid session cookie
		r.Group(func(r chi.Router) {
			r.Use(requireAuth(d.Auth))

			r.Get("/me", auth.Me)
			r.Get("/users", auth.ListUsers)
			r.Post("/admin/claim", admin.Claim)
		})
	})

	return r
}
