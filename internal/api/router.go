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
	"nimbus/internal/service"
	"nimbus/internal/sshkeys"
	"nimbus/internal/tunnel"
)

// Deps bundles the dependencies the router needs.
type Deps struct {
	Auth       *service.AuthService
	Provision  *provision.Service
	Bootstrap  *bootstrap.Service
	Keys       *sshkeys.Service
	Pool       *ippool.Pool
	Reconciler *ippool.Reconciler
	Proxmox    *proxmox.Client
	Tunnels    *tunnel.Client // optional: nil disables /api/tunnels admin endpoint
	Config     *config.Config
	Restart    func()
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
	keys := handlers.NewKeys(d.Keys)
	nodes := handlers.NewNodes(d.Proxmox)
	ips := handlers.NewIPs(d.Pool, d.Reconciler)
	cluster := handlers.NewCluster(d.Proxmox, d.Provision)
	bs := handlers.NewBootstrap(d.Bootstrap)
	setup := handlers.NewSetupWithAuth(d.Config, d.Restart, d.Auth)
	auth := handlers.NewAuth(d.Auth, d.Config.AppURL)
	settings := handlers.NewSettings(d.Auth)
	tunnels := handlers.NewTunnels(d.Tunnels, d.Config.GopherAPIURL)

	r.Route("/api", func(r chi.Router) {
		r.Get("/health", health.Check)
		r.Get("/setup/status", setup.Status)
		r.Post("/setup/admin", setup.CreateAdmin)

		r.Get("/nodes", nodes.List)
		r.Get("/ips", ips.List)
		r.Get("/cluster/vms", cluster.ListVMs)
		r.Get("/cluster/stats", cluster.Stats)

		// Public tunnel preview info — exposes the configured Gopher host (no
		// secrets) so the SPA can show "<subdomain>.<host>" before submitting.
		r.Get("/tunnels/info", tunnels.Info)

		// Reconcile can run a few seconds on a busy cluster (per-node walks)
		// — give it a longer timeout than other read endpoints.
		r.With(middleware.Timeout(60*time.Second)).
			Post("/ips/reconcile", ips.Reconcile)

		// VM provisioning — long-running, gets its own timeout.
		r.Route("/vms", func(r chi.Router) {
			r.Use(middleware.Timeout(180 * time.Second))
			r.Get("/", vms.List)
			r.Post("/", vms.Create)
			r.Get("/{id}", vms.Get)
			r.Get("/{id}/private-key", vms.GetPrivateKey)
		})

		r.Route("/keys", func(r chi.Router) {
			r.Get("/", keys.List)
			r.Post("/", keys.Create)
			r.Get("/{id}", keys.Get)
			r.Delete("/{id}", keys.Delete)
			r.Get("/{id}/private-key", keys.PrivateKey)
			r.Post("/{id}/private-key", keys.AttachPrivateKey)
			r.Post("/{id}/default", keys.SetDefault)
		})

		// Admin operations: template bootstrap. This can take 10-20 minutes
		// when downloading all 4 OSes across all online nodes — give it room.
		r.Route("/admin", func(r chi.Router) {
			r.Get("/bootstrap-status", bs.BootstrapStatus)
			r.With(middleware.Timeout(30*time.Minute)).Post("/bootstrap-templates", bs.BootstrapTemplates)
		})

		// Auth routes (public)
		r.Post("/auth/register", auth.Register)
		r.Post("/auth/login", auth.Login)
		r.Post("/auth/logout", auth.Logout)
		r.Get("/auth/github", auth.GitHubStart)
		r.Get("/auth/github/callback", auth.GitHubCallback)
		r.Get("/auth/google", auth.GoogleStart)
		r.Get("/auth/google/callback", auth.GoogleCallback)
		r.Get("/auth/providers", auth.Providers)

		// Protected routes — require a valid session cookie
		r.Group(func(r chi.Router) {
			r.Use(requireAuth(d.Auth))

			r.Get("/me", auth.Me)
			r.Get("/users", auth.ListUsers)

			// Admin-only routes
			r.Group(func(r chi.Router) {
				r.Use(requireAdmin)
				r.Get("/settings/oauth", settings.GetOAuth)
				r.Put("/settings/oauth", settings.SaveOAuth)
				r.Get("/tunnels", tunnels.List)
			})
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
