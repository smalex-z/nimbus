// Package api wires the Chi router and middleware stack.
package api

import (
	"io/fs"
	"net/http"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/go-chi/chi/v5/middleware"
	httpSwagger "github.com/swaggo/http-swagger"

	"nimbus/internal/api/handlers"
	_ "nimbus/internal/api/openapi" // registers the generated swagger spec
	"nimbus/internal/bootstrap"
	"nimbus/internal/config"
	"nimbus/internal/gpu"
	"nimbus/internal/ippool"
	"nimbus/internal/nodemgr"
	"nimbus/internal/provision"
	"nimbus/internal/proxmox"
	"nimbus/internal/s3storage"
	"nimbus/internal/selftunnel"
	"nimbus/internal/service"
	"nimbus/internal/sshkeys"
	"nimbus/internal/tunnel"
	"nimbus/internal/vnetmgr"
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
	UserBuckets   *s3storage.UserBucketService
	GPU           *gpu.Service // optional: nil disables /api/gpu/* routes
	GX10Assets    fs.FS        // embedded scripts + worker binary, served via /api/gpu/scripts/{name}
	NodeMgr       *nodemgr.Service
	VNetMgr       *vnetmgr.Service // P1: SDN bootstrap; P2: per-user VNet allocation
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
	nodes := handlers.NewNodes(d.NodeMgr, d.Config, d.Restart)
	ips := handlers.NewIPs(d.Pool, d.Reconciler)
	cluster := handlers.NewCluster(d.Proxmox, d.Provision, d.NodeMgr)
	bs := handlers.NewBootstrap(d.Bootstrap)
	setup := handlers.NewSetupWithAuth(d.Config, d.Restart, d.Auth)
	auth := handlers.NewAuth(d.Auth, d.Config.AppURL, d.Reconciler).WithVMActor(d.Provision)
	if d.UserBuckets != nil {
		auth = auth.WithBucketPurger(d.UserBuckets)
	}
	tunnels := handlers.NewTunnels(d.Tunnels, d.TunnelURL)
	settingsBuilder := handlers.NewSettings(d.Auth).
		WithTunnelAppliers(d.Provision).
		WithTunnelInfoSetter(tunnels).
		WithGPUAppliers(d.Provision).
		WithNimbusAppURL(d.Config.AppURL).
		WithAppURLResolver(auth).
		WithNetworkAppliers(d.Provision).
		WithNetworkOps(d.Provision).
		WithPoolReseeder(d.Pool)
	if d.VNetMgr != nil {
		settingsBuilder = settingsBuilder.WithSDNBootstrapper(d.VNetMgr)
	}
	if d.SelfBootstrap != nil {
		settingsBuilder = settingsBuilder.WithSelfBootstrap(d.SelfBootstrap)
	}
	settings := settingsBuilder
	s3 := handlers.NewS3(d.S3, d.Provision)
	subnets := handlers.NewSubnets(d.VNetMgr)
	buckets := handlers.NewBuckets(d.UserBuckets)

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
		// Public OpenAPI/SwaggerUI surface. The spec is bundled at build
		// time via `make swagger` (swag init) and registered through the
		// blank-imported `internal/api/openapi` package above. URL is
		// hard-coded to /api/docs/doc.json so the SwaggerUI loads it
		// regardless of how the server is deployed (http-swagger's default
		// uses a relative path that breaks behind proxies).
		r.Get("/docs", func(w http.ResponseWriter, r *http.Request) {
			http.Redirect(w, r, "/api/docs/", http.StatusMovedPermanently)
		})
		r.Get("/docs/*", httpSwagger.Handler(httpSwagger.URL("/api/docs/doc.json")))

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
		// Link-mode entry points share the OAuth callback handlers
		// above; they're separate endpoints only at the start to set
		// the link-intent cookie. Callbacks consult that cookie to
		// branch behavior. Auth check is inline (the dance returns
		// here mid-flight, before the new session would exist).
		r.Get("/auth/github/link", auth.GitHubLinkStart)
		r.Get("/auth/google/link", auth.GoogleLinkStart)
		// Magic-link sign-in for password-only users coming back from
		// a recovery email. Public — the token in the URL IS the auth.
		// Validation is inside the handler; bad/expired/used tokens
		// redirect to /login with an explanatory query param.
		r.Get("/auth/magic/{token}", auth.MagicLinkSignIn)

		// Protected routes — require a valid session cookie
		r.Group(func(r chi.Router) {
			r.Use(requireAuth(d.Auth))

			r.Get("/me", auth.Me)
			r.Get("/users", auth.ListUsers)
			r.Get("/account", auth.Account)
			r.Put("/account/password", auth.ChangePassword)

			// Public SDN status — every verified user reads this so
			// the Provision form's subnet picker can render the right
			// modes (subnet picker vs. greyed Cluster LAN). Admin-only
			// details live on /settings/sdn.
			r.Get("/sdn/status", settings.PublicSDNStatus)

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

				// Admin-only user management — promote/delete. The
				// list endpoint above is shared (admins see everyone,
				// members see themselves); these mutations are admin
				// only.
				r.Post("/users/{id}/promote", auth.PromoteUser)
				r.Post("/users/{id}/suspend-status", auth.SetSuspended)
				r.Post("/users/suspend-unlinked", auth.SuspendUnlinked)
				r.Delete("/users/{id}", auth.DeleteUser)
				r.Get("/settings/oauth/passwordless", auth.PasswordlessStatus)
				r.Put("/settings/oauth/passwordless", auth.SetPasswordlessAuth)
				r.Get("/settings/smtp", auth.GetSMTP)
				r.Put("/settings/smtp", auth.SaveSMTP)
				r.Post("/settings/smtp/test", auth.SendTestEmail)
				r.Post("/users/email-unlinked", auth.EmailUnlinked)
				r.Get("/settings/quotas", auth.GetQuotas)
				r.Put("/settings/quotas", auth.SaveQuotas)
				r.Put("/users/{id}/quota", auth.SetUserQuota)

				r.Get("/nodes", nodes.List)
				// Admin-facing node lifecycle. Drain streams NDJSON and
				// can run for tens of minutes per VM on cold migrations
				// — give it a generous timeout. The other actions are
				// fast DB writes (cordon/uncordon/tags/delete).
				r.Post("/nodes/{name}/cordon", nodes.Cordon)
				r.Post("/nodes/{name}/uncordon", nodes.Uncordon)
				r.Put("/nodes/{name}/tags", nodes.SetTags)
				r.Get("/nodes/{name}/drain-plan", nodes.DrainPlan)
				r.With(middleware.Timeout(60*time.Minute)).
					Post("/nodes/{name}/drain", nodes.Drain)
				r.Delete("/nodes/{name}", nodes.Remove)
				// Per-node score drill-down. /nodes supports
				// ?include_scores=true for the cluster-wide matrix;
				// this endpoint computes one cell with optional
				// host-aggregate constraint via ?tags=.
				r.Get("/nodes/{name}/score", nodes.Score)
				// Cluster-wide scheduling knobs (cpu/ram/disk
				// allocation ratios). Read+write so the Nodes page
				// can render and update them without a restart;
				// changes take effect on the next provision/drain.
				r.Get("/scheduling", nodes.GetSchedulingSettings)
				r.Put("/scheduling", nodes.SaveSchedulingSettings)
				// Proxmox binding — read-only chip + reconfigure.
				// PUT writes the env file with the new triple, then
				// triggers restartSelf so a fresh process picks them
				// up. Same flow the install wizard uses.
				r.Get("/proxmox/binding", nodes.Binding)
				r.Put("/proxmox/binding", nodes.ChangeBinding)
				// Discover Proxmox endpoints on this network — the
				// admin reuses the install wizard's discovery handler
				// from the change-binding modal, so the same scan +
				// corosync logic runs in both contexts.
				r.Get("/proxmox/discover", setup.Discover)
				r.Get("/ips", ips.List)
				r.Get("/cluster/vms", cluster.ListVMs)
				r.Delete("/cluster/vms/{id}", cluster.DeleteVM)
				// Power operations on any cluster VM (local / foreign /
				// external). Routed by (node, vmid) so foreign + external
				// rows that have no nimbus DB id are still reachable.
				// Reboot waits on a Proxmox task — give it some headroom.
				r.With(middleware.Timeout(2*time.Minute)).
					Post("/cluster/vms/{node}/{vmid}/{op}", cluster.VMLifecycle)
				// Migrate-plan is the placement preview the modal fetches on
				// open — fast Proxmox + DB walk, no upper bound headroom
				// needed (default route timeout is fine).
				r.Get("/cluster/vms/{id}/migrate-plan", cluster.MigratePlan)
				// Migration is the long path: an online migration on a busy
				// VM can run several minutes copying memory across nodes;
				// the offline-fallback path adds shutdown + start on top.
				// 35 min covers the worst case.
				r.With(middleware.Timeout(35*time.Minute)).
					Post("/cluster/vms/{id}/migrate", cluster.MigrateVM)
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
				// Per-user SDN VNet isolation. Read returns the live
				// status (zone exists / pending / missing-pkg); Save
				// persists + bootstraps in one shot. P1 scope: zone
				// reconcile only — per-VM provisioning still uses
				// vmbr0 until P2 lands.
				r.Get("/settings/sdn", settings.GetSDN)
				r.Put("/settings/sdn", settings.SaveSDN)
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

				// Admin-side bucket views — cross-user listing with owner
				// info, plus a force-delete that empties non-empty buckets.
				// User-scoped /api/buckets remains the per-user surface
				// (own buckets only, no force-delete).
				r.Get("/s3/buckets", s3.ListBuckets)
				r.With(middleware.Timeout(2*time.Minute)).
					Delete("/s3/buckets/{name}", s3.DeleteBucket)

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

				// Per-user SDN subnets — OCI-style: each user manages
				// their own list of subnets, picks one (or creates new
				// inline) at provision time. Same shape as /keys.
				r.Route("/subnets", func(r chi.Router) {
					r.Get("/", subnets.List)
					r.Post("/", subnets.Create)
					r.Get("/{id}", subnets.Get)
					r.Delete("/{id}", subnets.Delete)
					r.Post("/{id}/default", subnets.SetDefault)
				})

				// User-owned buckets on the shared MinIO host. Each user has
				// a stable name prefix (`<sanitized-name>-u<id>`); composed
				// bucket names look like `kevin-u3-uploads`. Service account
				// is auto-minted on first /buckets visit or first create.
				r.Route("/buckets", func(r chi.Router) {
					r.Get("/", buckets.List)
					r.Post("/", buckets.Create)
					r.Get("/credentials", buckets.Credentials)
					r.Delete("/{name}", buckets.Delete)
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
