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
	TunnelURL  string         // configured Gopher URL; surfaced via /api/tunnels/info
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
	auth := handlers.NewAuth(d.Auth, d.Config.AppURL, d.Reconciler)
	tunnels := handlers.NewTunnels(d.Tunnels, d.TunnelURL)
	settings := handlers.NewSettings(d.Auth).
		WithTunnelAppliers(d.Provision).
		WithTunnelInfoSetter(tunnels)

	r.Route("/api", func(r chi.Router) {
		r.Get("/health", health.Check)
		r.Get("/setup/status", setup.Status)
		r.Post("/setup/admin", setup.CreateAdmin)

		// Tunnel preview info — exposes the configured Gopher host (no
		// secrets) so the SPA can show "<host>:<port>" before submitting.
		// Public so the Provision form can render the disabled-checkbox
		// state with the right hint copy when tunnels aren't configured.
		r.Get("/tunnels/info", tunnels.Info)

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

			// Access-code endpoints — must be reachable WITHOUT being verified,
			// so the unverified user can submit their code from the Verify page.
			r.Get("/access-code/status", auth.VerifyStatus)
			r.Post("/access-code/verify", auth.VerifyAccessCode)

			// Admin-only routes — cluster observability + cluster-wide
			// configuration. Default users never see these endpoints; the
			// Admin and Authentication pages are admin-only in the SPA too.
			r.Group(func(r chi.Router) {
				r.Use(requireAdmin)

				r.Get("/nodes", nodes.List)
				r.Get("/ips", ips.List)
				r.Get("/cluster/vms", cluster.ListVMs)
				r.Get("/cluster/stats", cluster.Stats)

				// Reconcile can run a few seconds on a busy cluster
				// (per-node walks) — give it a longer timeout.
				r.With(middleware.Timeout(60*time.Second)).
					Post("/ips/reconcile", ips.Reconcile)

				// Bootstrap status is read-only; bootstrap-templates can
				// take 10-20 minutes when downloading all 4 OSes across
				// every online node — give it room.
				r.Get("/admin/bootstrap-status", bs.BootstrapStatus)
				r.With(middleware.Timeout(30*time.Minute)).
					Post("/admin/bootstrap-templates", bs.BootstrapTemplates)

				r.Get("/settings/oauth", settings.GetOAuth)
				r.Put("/settings/oauth", settings.SaveOAuth)
				r.Get("/settings/access-code", settings.GetAccessCode)
				r.Post("/settings/access-code/regenerate", settings.RegenerateAccessCode)
				r.Get("/settings/google-domains", settings.GetAuthorizedGoogleDomains)
				r.Put("/settings/google-domains", settings.SaveAuthorizedGoogleDomains)
				r.Get("/settings/github-orgs", settings.GetAuthorizedGitHubOrgs)
				r.Put("/settings/github-orgs", settings.SaveAuthorizedGitHubOrgs)
				r.Get("/settings/gopher", settings.GetGopher)
				r.Put("/settings/gopher", settings.SaveGopher)
				r.Get("/tunnels", tunnels.List)
			})

			// User-scoped routes — non-admins must be verified against the
			// current access code version. Admins always pass requireVerified.
			r.Group(func(r chi.Router) {
				r.Use(requireVerified(d.Auth))

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
