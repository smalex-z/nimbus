// Package api wires the Chi router and middleware stack.
package api

import (
	"io/fs"
	"net/http"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/go-chi/chi/v5/middleware"

	"nimbus/internal/api/handlers"
	"nimbus/internal/bootstrap"
	"nimbus/internal/config"
	"nimbus/internal/gpu"
	"nimbus/internal/ippool"
	"nimbus/internal/provision"
	"nimbus/internal/proxmox"
	"nimbus/internal/s3storage"
	"nimbus/internal/selftunnel"
	"nimbus/internal/service"
	"nimbus/internal/sshkeys"
	"nimbus/internal/tunnel"
)

// Deps bundles the dependencies the router needs.
type Deps struct {
	Auth          *service.AuthService
	Provision     *provision.Service
	Bootstrap     *bootstrap.Service
	Keys          *sshkeys.Service
	Pool          *ippool.Pool
	Reconciler    *ippool.Reconciler
	Proxmox       *proxmox.Client
	Tunnels       *tunnel.Client // optional: nil disables /api/tunnels admin endpoint
	TunnelURL     string         // configured Gopher URL; surfaced via /api/tunnels/info
	SelfBootstrap *selftunnel.Service
	S3            *s3storage.Service
	GPU           *gpu.Service // optional: nil disables /api/gpu/* routes
	GX10Assets    fs.FS        // embedded scripts + worker binary, served via /api/gpu/scripts/{name}
	Config        *config.Config
	Restart       func()
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
	settingsBuilder := handlers.NewSettings(d.Auth).
		WithTunnelAppliers(d.Provision).
		WithTunnelInfoSetter(tunnels).
		WithGPUAppliers(d.Provision).
		WithNimbusAppURL(d.Config.AppURL).
		WithNetworkAppliers(d.Provision).
		WithNetworkOps(d.Provision).
		WithPoolReseeder(d.Pool)
	if d.SelfBootstrap != nil {
		settingsBuilder = settingsBuilder.WithSelfBootstrap(d.SelfBootstrap)
	}
	settings := settingsBuilder
	s3 := handlers.NewS3(d.S3, d.Provision)

	var gpuHandler *handlers.GPU
	var gpuScripts *handlers.ScriptHandler
	if d.GPU != nil {
		gpuHandler = handlers.NewGPU(d.GPU, d.Auth, d.Config.AppURL).
			WithGPUConfigApplier(func(baseURL, model, nimbusGPUAPI string) {
				// Same payload shape settings.SaveGPU pushes — keeps the
				// pairing-flow and manual-edit code paths converged.
				// nimbusGPUAPI=="" means the caller (e.g. Unpair) didn't
				// derive one from a request; fall back to AppURL so manual
				// SaveGPU still works on Nimbus instances with a properly
				// set APP_URL.
				if nimbusGPUAPI == "" {
					nimbusGPUAPI = strings.TrimRight(d.Config.AppURL, "/") + "/api/gpu"
				}
				if d.Provision != nil {
					d.Provision.SetGPUBootstrapConfig(provision.GPUBootstrapConfig{
						BaseURL:        baseURL,
						InferenceModel: model,
						NimbusGPUAPI:   nimbusGPUAPI,
					})
				}
			})
		gpuScripts = handlers.NewScriptHandler(d.GX10Assets)
	}

	r.Route("/api", func(r chi.Router) {
		r.Get("/health", health.Check)
		r.Get("/setup/status", setup.Status)
		r.Post("/setup/admin", setup.CreateAdmin)

		// Tunnel preview info — exposes the configured Gopher host (no
		// secrets) so the SPA can show "<host>:<port>" before submitting.
		// Public so the Provision form can render the disabled-checkbox
		// state with the right hint copy when tunnels aren't configured.
		r.Get("/tunnels/info", tunnels.Info)

		// GPU worker endpoints — bearer-token auth, callable from the GX10
		// worker daemon over the LAN. Mounted OUTSIDE the requireAuth group
		// because the GX10 doesn't have a session cookie; it has a token.
		if gpuHandler != nil {
			r.Group(func(r chi.Router) {
				r.Use(requireGPUWorkerToken(d.Auth))
				r.Post("/gpu/worker/claim", gpuHandler.ClaimNext)
				r.Post("/gpu/worker/jobs/{id}/logs", gpuHandler.AppendLogs)
				r.Post("/gpu/worker/jobs/{id}/status", gpuHandler.ReportStatus)
			})
			// Static script downloads — also outside requireAuth so the
			// GX10's curl-bootstrap can fetch them. Whitelist enforced
			// inside the handler so this isn't a path-traversal vector.
			r.Get("/gpu/scripts/{name}", gpuScripts.Serve)
			// Pairing handshake: install.sh validates ?token= against the
			// active pairing window; register exchanges the pairing token
			// for a permanent worker token. Both public — the pairing
			// token IS the auth.
			r.Get("/gpu/install.sh", gpuHandler.InstallScript)
			r.Post("/gpu/register", gpuHandler.Register)
		}

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

			// Bootstrap status is a read-only yes/no — both admins and
			// regular members need it so the Provision UI can decide whether
			// to render the form (templates ready) or the "admin access
			// required" card (templates missing). The destructive POST stays
			// admin-only below.
			r.Get("/admin/bootstrap-status", bs.BootstrapStatus)

			// Admin-only routes — cluster observability + cluster-wide
			// configuration. Default users never see these endpoints; the
			// Admin and Authentication pages are admin-only in the SPA too.
			r.Group(func(r chi.Router) {
				r.Use(requireAdmin)

				r.Get("/nodes", nodes.List)
				r.Get("/ips", ips.List)
				r.Get("/cluster/vms", cluster.ListVMs)
				r.Delete("/cluster/vms/{id}", cluster.DeleteVM)
				// Power operations on any cluster VM (local / foreign /
				// external). Routed by (node, vmid) so foreign + external
				// rows that have no nimbus DB id are still reachable.
				// Reboot waits on a Proxmox task — give it some headroom.
				r.With(middleware.Timeout(2*time.Minute)).
					Post("/cluster/vms/{node}/{vmid}/{op}", cluster.VMLifecycle)
				r.Get("/cluster/stats", cluster.Stats)

				// Reconcile can run a few seconds on a busy cluster
				// (per-node walks) — give it a longer timeout.
				r.With(middleware.Timeout(60*time.Second)).
					Post("/ips/reconcile", ips.Reconcile)
				// VM-table reconcile: tracks migrations and soft-deletes
				// orphan rows that have been missing from Proxmox for N
				// consecutive runs. Refuses on empty cluster snapshot.
				r.With(middleware.Timeout(60*time.Second)).
					Post("/vms/reconcile", vms.Reconcile)

				// Bootstrap templates can take 10-20 minutes when
				// downloading all 4 OSes across every online node —
				// give it room.
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
				r.Get("/settings/gopher/self-bootstrap", settings.SelfBootstrapStatus)
				r.Post("/settings/gopher/self-bootstrap", settings.SelfBootstrapStart)
				r.Get("/settings/network", settings.GetNetwork)
				r.Put("/settings/network", settings.SaveNetwork)
				// Disruptive batch ops — generous timeout because each VM
				// reboot waits on a Proxmox task.
				r.With(middleware.Timeout(15*time.Minute)).
					Post("/settings/network/renumber-vms", settings.RenumberVMs)
				r.With(middleware.Timeout(15*time.Minute)).
					Post("/settings/network/force-gateway-update", settings.ForceGatewayUpdate)
				r.Get("/tunnels", tunnels.List)

				// S3 storage (singleton MinIO VM) — admin-only.
				// CreateStorage is the long-running deploy: provision flow
				// + SSH bootstrap can take 5-8 minutes on a cold pull.
				r.Get("/s3/storage", s3.GetStorage)
				r.With(middleware.Timeout(15*time.Minute)).
					Post("/s3/storage", s3.CreateStorage)
				r.With(middleware.Timeout(5*time.Minute)).
					Delete("/s3/storage", s3.DeleteStorage)

				// Bucket CRUD on the deployed MinIO. Returns 503 from
				// writeBucketsError when storage is absent or not ready.
				r.Get("/s3/buckets", s3.ListBuckets)
				r.Post("/s3/buckets", s3.CreateBucket)
				r.Delete("/s3/buckets/{name}", s3.DeleteBucket)

				// GPU plane — admin-only configuration + the pairing-token
				// minter. The job submission/list endpoints are exposed
				// to all verified users below.
				if gpuHandler != nil {
					r.Get("/settings/gpu", settings.GetGPU)
					r.Put("/settings/gpu", settings.SaveGPU)
					r.Post("/settings/gpu/pairing", gpuHandler.MintPairing)
					r.Post("/settings/gpu/unpair", gpuHandler.Unpair)
				}
			})

			// User-scoped routes — non-admins must be verified against the
			// current access code version. Admins always pass requireVerified.
			r.Group(func(r chi.Router) {
				r.Use(requireVerified(d.Auth))

				r.Route("/vms", func(r chi.Router) {
					r.Get("/", vms.List)
					// Provision is the slow path: clone + cloud-init + boot
					// takes 30-60s on its own, plus an optional Gopher tunnel
					// bootstrap that can wait on dpkg locks for another
					// 1-3min during early-boot cloud-init contention.
					r.With(middleware.Timeout(6*time.Minute)).Post("/", vms.Create)
					r.Get("/{id}", vms.Get)
					r.Get("/{id}/private-key", vms.GetPrivateKey)
					r.Delete("/{id}", vms.Delete)
					// Power operations on the caller's own VM. Reboot
					// waits on a Proxmox task — give it some room.
					r.With(middleware.Timeout(2*time.Minute)).
						Post("/{id}/{op:start|shutdown|stop|reboot}", vms.Lifecycle)
					// Per-port tunnels on top of the VM's Gopher machine —
					// the post-provision Networks surface.
					r.Get("/{id}/tunnels", vms.ListTunnels)
					r.Post("/{id}/tunnels", vms.CreateTunnel)
					r.Delete("/{id}/tunnels/{tunnelId}", vms.DeleteTunnel)
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

				// GPU plane — submit/list/cancel jobs, inference status.
				// Mounted under requireVerified so the access-code gate
				// applies. Admin gets all jobs; non-admin gets their own.
				if gpuHandler != nil {
					r.Route("/gpu", func(r chi.Router) {
						r.Get("/inference", gpuHandler.Inference)
						r.Route("/jobs", func(r chi.Router) {
							r.Get("/", gpuHandler.ListJobs)
							r.Post("/", gpuHandler.SubmitJob)
							r.Get("/{id}", gpuHandler.GetJob)
							r.Post("/{id}/cancel", gpuHandler.CancelJob)
						})
					})
				}
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
