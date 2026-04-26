package main

import (
	"context"
	"embed"
	"encoding/json"
	"flag"
	"fmt"
	"io/fs"
	"log"
	"net/http"
	"os"
	"strings"
	"syscall"
	"time"

	"nimbus/internal/api"
	"nimbus/internal/bootstrap"
	"nimbus/internal/build"
	"nimbus/internal/config"
	"nimbus/internal/db"
	"nimbus/internal/install"
	"nimbus/internal/ippool"
	"nimbus/internal/provision"
	"nimbus/internal/proxmox"
	"nimbus/internal/secrets"
	"nimbus/internal/service"
	"nimbus/internal/sshkeys"
)

//go:embed all:frontend/dist
var frontendFS embed.FS

func main() {
	// Subcommand dispatch — `nimbus bootstrap [flags]` runs the template
	// bootstrap once and exits, sharing the same code path as the HTTP
	// endpoint. Useful for ops/wizard, doesn't require the server to be running.
	if len(os.Args) > 1 && os.Args[1] == "bootstrap" {
		if err := runBootstrap(os.Args[2:]); err != nil {
			log.Fatalf("bootstrap failed: %v", err)
		}
		return
	}

	// `nimbus install [--upgrade]` installs the binary to /opt/nimbus, writes
	// the systemd unit, and starts the service. Re-execs via sudo when not root.
	if len(os.Args) > 1 && os.Args[1] == "install" {
		install.Run(os.Args[2:])
		return
	}

	flags := flag.NewFlagSet("nimbus", flag.ExitOnError)
	port := flags.String("port", "", "server port (overrides PORT env var)")
	dbPath := flags.String("db", "", "database path (overrides DB_PATH env var)")
	version := flags.Bool("version", false, "print version and exit")
	_ = flags.Parse(os.Args[1:])

	if *version {
		log.Printf("nimbus %s", build.Version)
		return
	}

	cfg, err := config.Load()
	if err != nil {
		log.Fatalf("failed to load config: %v", err)
	}
	if *port != "" {
		cfg.Port = *port
	}
	if *dbPath != "" {
		cfg.DBPath = *dbPath
	}

	distFS, err := fs.Sub(frontendFS, "frontend/dist")
	if err != nil {
		log.Fatalf("failed to create frontend sub-filesystem: %v", err)
	}

	// Start in setup mode when required config is absent.
	if !cfg.IsConfigured() {
		log.Printf("nimbus %s starting in setup mode on :%s", build.Version, cfg.Port)
		router := api.NewSetupRouter(cfg, restartSelf)

		mux := http.NewServeMux()
		mux.Handle("/api/", router)
		mux.Handle("/", spaHandler(http.FS(distFS)))

		srv := &http.Server{
			Addr:              ":" + cfg.Port,
			Handler:           mux,
			ReadHeaderTimeout: 10 * time.Second,
		}
		if err := srv.ListenAndServe(); err != nil {
			log.Fatalf("server error: %v", err)
		}
		return
	}

	if err := cfg.Validate(); err != nil {
		log.Fatalf("invalid configuration: %v", err)
	}

	database, err := db.New(cfg.DBPath,
		&db.User{}, &db.Session{}, &db.OAuthSettings{}, &db.GopherSettings{},
		&db.VM{}, &db.NodeTemplate{}, &db.SSHKey{},
		ippool.Model(),
	)
	if err != nil {
		log.Fatalf("failed to open database: %v", err)
	}

	authSvc := service.NewAuthService(database)

	// Gopher seed: if env vars are set AND the DB row is still empty, copy
	// env → DB once. After that, the Authentication settings page is the
	// source of truth (admins can rotate creds without touching .env).
	if cfg.GopherAPIURL != "" || cfg.GopherAPIKey != "" {
		if existing, err := authSvc.GetGopherSettings(); err == nil &&
			existing.APIURL == "" && existing.APIKey == "" {
			if err := authSvc.SaveGopherSettings(db.GopherSettings{
				APIURL: cfg.GopherAPIURL,
				APIKey: cfg.GopherAPIKey,
			}); err != nil {
				log.Printf("warning: failed to seed Gopher settings from env: %v", err)
			} else {
				log.Printf("seeded Gopher settings from env (one-time migration)")
			}
		}
	}

	// Backfill: pre-`is_admin` deployments have users that AutoMigrate left
	// at default false. If no admin exists, promote the oldest user so a
	// single-tenant homelab self-heals on the first post-upgrade boot.
	if promoted, err := authSvc.PromoteFirstUserIfNoAdmin(); err != nil {
		log.Printf("warning: admin backfill check failed: %v", err)
	} else if promoted {
		log.Printf("backfill: promoted oldest user to admin (no admin existed)")
	}

	pool := ippool.New(database.DB)
	if err := pool.Seed(context.Background(), cfg.IPPoolStart, cfg.IPPoolEnd); err != nil {
		log.Fatalf("failed to seed IP pool: %v", err)
	}

	// Bootstrap the master encryption key for the SSH key vault. On first run
	// this generates a 32-byte AES-256 key and persists it next to the rest of
	// Nimbus's env config; on subsequent runs the existing key is reused.
	encKey, err := secrets.LoadOrCreateKey(config.EnvFilePath())
	if err != nil {
		log.Fatalf("failed to load/create encryption key: %v", err)
	}
	cipher, err := secrets.New(encKey)
	if err != nil {
		log.Fatalf("failed to init secrets cipher: %v", err)
	}

	// Long timeout: the bootstrap path makes calls that wait on Proxmox tasks
	// (image downloads) which can run for several minutes. The HTTP server
	// route timeout is the real upper bound; this just controls per-request
	// transport timeout for individual API calls.
	pveClient := proxmox.New(cfg.ProxmoxHost, cfg.ProxmoxTokenID, cfg.ProxmoxTokenSecret, 5*time.Minute)

	keysSvc := sshkeys.New(database.DB, cipher)

	// Migrate any pre-existing per-VM vault entries into the ssh_keys table.
	// Idempotent — VMs that already have ssh_key_id set are skipped. Failures
	// are logged but don't abort startup.
	migCtx, migCancel := context.WithTimeout(context.Background(), 30*time.Second)
	if n, err := sshkeys.MigrateLegacyVMKeys(migCtx, database.DB); err != nil {
		log.Printf("warning: ssh-key migration failed: %v", err)
	} else if n > 0 {
		log.Printf("migrated %d legacy per-VM vault entries into ssh_keys", n)
	}
	migCancel()

	reconciler := ippool.NewReconciler(pool, pveClient,
		ippool.WithStaleAfter(time.Duration(cfg.ReservationTTLSeconds)*time.Second),
		ippool.WithCacheTTL(time.Duration(cfg.VerifyCacheTTLSeconds)*time.Second),
		ippool.WithMissThreshold(cfg.VacateMissThreshold),
	)

	// Startup reconcile: bounded so a temporarily unreachable Proxmox doesn't
	// block boot. Failure is logged, not fatal.
	startupCtx, cancelStartup := context.WithTimeout(context.Background(), 30*time.Second)
	if rep, err := reconciler.Reconcile(startupCtx); err != nil {
		log.Printf("startup reconcile failed (continuing): %v", err)
	} else {
		log.Printf("startup reconcile: adopted=%d conflicts=%d freed=%d vacated=%d",
			len(rep.Adopted), len(rep.Conflicts), len(rep.Freed), len(rep.Vacated))
	}
	cancelStartup()

	bgCtx, cancelBg := context.WithCancel(context.Background())
	defer cancelBg()
	go runReconcileLoop(bgCtx, reconciler, time.Duration(cfg.ReconcileIntervalSeconds)*time.Second)

	provSvc := provision.New(pveClient, pool, database.DB, cipher, keysSvc, provision.Config{
		TemplateBaseVMID: cfg.ProxmoxTemplateBaseVMID,
		ExcludedNodes:    cfg.ExcludedNodes,
		GatewayIP:        cfg.GatewayIP,
		Nameserver:       cfg.Nameserver,
		SearchDomain:     cfg.SearchDomain,
		CPUType:          cfg.VMCPUType,
		VMDiskStorage:    cfg.VMDiskStorage,
		MemBufferMiB:     cfg.MemBufferMiB,
		CPULoadFactor:    cfg.CPULoadFactor,
	})
	provSvc.SetIPVerifier(reconciler)

	// Backfill Nimbus marker tags onto VMs provisioned by older builds that
	// didn't tag on creation. Idempotent and best-effort — failures are logged
	// but don't abort startup.
	tagBackfillCtx, tagBackfillCancel := context.WithTimeout(context.Background(), 60*time.Second)
	if n, err := provSvc.BackfillTags(tagBackfillCtx); err != nil {
		log.Printf("warning: tag backfill failed: %v", err)
	} else if n > 0 {
		log.Printf("backfill: stamped nimbus tags onto %d VM(s)", n)
	}
	tagBackfillCancel()

	bootstrapSvc := bootstrap.New(pveClient, database.DB, bootstrap.Config{
		TemplateBaseVMID: cfg.ProxmoxTemplateBaseVMID,
	})

	// Best-effort startup migration: import any pre-existing templates from
	// the cluster into the node_templates table so provision lookups work
	// without requiring a re-bootstrap. Logged on failure but doesn't block
	// startup — the bootstrap endpoint remains the recovery path.
	syncCtx, syncCancel := context.WithTimeout(context.Background(), 30*time.Second)
	if n, err := bootstrap.SyncFromProxmox(syncCtx, database.DB, pveClient); err != nil {
		log.Printf("warning: template sync from proxmox failed: %v", err)
	} else if n > 0 {
		log.Printf("imported %d existing template(s) into node_templates", n)
	}
	syncCancel()

	router := api.NewRouter(api.Deps{
		Auth:       authSvc,
		Provision:  provSvc,
		Bootstrap:  bootstrapSvc,
		Keys:       keysSvc,
		Pool:       pool,
		Reconciler: reconciler,
		Proxmox:    pveClient,
		Config:     cfg,
		Restart:    restartSelf,
	})

	mux := http.NewServeMux()
	mux.Handle("/api/", router)
	mux.Handle("/", spaHandler(http.FS(distFS)))

	log.Printf("nimbus %s starting on :%s (proxmox=%s, pool=%s..%s)",
		build.Version, cfg.Port, cfg.ProxmoxHost, cfg.IPPoolStart, cfg.IPPoolEnd)
	srv := &http.Server{
		Addr:              ":" + cfg.Port,
		Handler:           mux,
		ReadHeaderTimeout: 10 * time.Second,
		// Match the longest route timeout so the server doesn't cut bootstrap
		// connections short.
		WriteTimeout: 35 * time.Minute,
	}
	if err := srv.ListenAndServe(); err != nil {
		log.Fatalf("server error: %v", err)
	}
}

// runBootstrap executes the `nimbus bootstrap` subcommand.
//
// Loads config the same way as the server (env / .env), constructs a Proxmox
// client and bootstrap service, runs the bootstrap, and prints results.
// Exits with code 1 if any template failed.
func runBootstrap(args []string) error {
	fs := flag.NewFlagSet("bootstrap", flag.ContinueOnError)
	osList := fs.String("os", "", "comma-separated OS keys (default: all 4)")
	nodeList := fs.String("node", "", "comma-separated node names (default: all online nodes)")
	force := fs.Bool("force", false, "re-create templates even if they already exist")
	if err := fs.Parse(args); err != nil {
		return err
	}

	cfg, err := config.Load()
	if err != nil {
		return fmt.Errorf("load config: %w", err)
	}
	if err := cfg.Validate(); err != nil {
		return fmt.Errorf("invalid configuration: %w", err)
	}

	pveClient := proxmox.New(cfg.ProxmoxHost, cfg.ProxmoxTokenID, cfg.ProxmoxTokenSecret, 5*time.Minute)
	database, err := db.New(cfg.DBPath, ippool.Model(), &db.VM{}, &db.NodeTemplate{}, &db.SSHKey{})
	if err != nil {
		return fmt.Errorf("open database: %w", err)
	}
	svc := bootstrap.New(pveClient, database.DB, bootstrap.Config{
		TemplateBaseVMID: cfg.ProxmoxTemplateBaseVMID,
	})

	req := bootstrap.Request{Force: *force}
	if *osList != "" {
		req.OS = splitCSV(*osList)
	}
	if *nodeList != "" {
		req.Nodes = splitCSV(*nodeList)
	}

	log.Printf("Bootstrapping templates (proxmox=%s, OS=%v, nodes=%v)",
		cfg.ProxmoxHost, req.OS, req.Nodes)

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Minute)
	defer cancel()

	res, err := svc.Bootstrap(ctx, req)
	if err != nil {
		return err
	}

	for _, o := range res.Created {
		log.Printf("✓ created  %s (vmid %d) on %s in %s", o.OS, o.VMID, o.Node, o.Duration)
	}
	for _, o := range res.Skipped {
		log.Printf("⤳ skipped  %s (vmid %d) on %s — already exists", o.OS, o.VMID, o.Node)
	}
	for _, o := range res.Failed {
		log.Printf("✗ failed   %s (vmid %d) on %s after %s: %s", o.OS, o.VMID, o.Node, o.Duration, o.Error)
	}

	// Pretty JSON to stdout for scripting.
	enc := json.NewEncoder(os.Stdout)
	enc.SetIndent("", "  ")
	if err := enc.Encode(res); err != nil {
		return err
	}

	if len(res.Failed) > 0 {
		os.Exit(1)
	}
	return nil
}

func splitCSV(s string) []string {
	parts := strings.Split(s, ",")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		if t := strings.TrimSpace(p); t != "" {
			out = append(out, t)
		}
	}
	return out
}

// runReconcileLoop runs reconciler.Reconcile every interval until ctx is
// cancelled. Errors are logged at the call site so transient Proxmox issues
// don't fall on the floor; this never panics nor returns.
func runReconcileLoop(ctx context.Context, reconciler *ippool.Reconciler, interval time.Duration) {
	if interval <= 0 {
		log.Printf("reconcile loop disabled (interval=%v)", interval)
		return
	}
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			runCtx, cancel := context.WithTimeout(ctx, interval)
			rep, err := reconciler.Reconcile(runCtx)
			cancel()
			if err != nil {
				log.Printf("background reconcile error: %v", err)
				continue
			}
			if len(rep.Adopted) > 0 || len(rep.Conflicts) > 0 || len(rep.Freed) > 0 || len(rep.Vacated) > 0 {
				log.Printf("background reconcile: adopted=%d conflicts=%d freed=%d vacated=%d",
					len(rep.Adopted), len(rep.Conflicts), len(rep.Freed), len(rep.Vacated))
			}
		}
	}
}

// restartSelf replaces the current process image with a fresh start via exec.
// The new process inherits the current environment (with any os.Setenv changes
// applied by the setup handler before this is called).
func restartSelf() {
	exe, err := os.Executable()
	if err != nil {
		log.Printf("restart: cannot locate executable: %v", err)
		return
	}
	if err := syscall.Exec(exe, os.Args, os.Environ()); err != nil {
		log.Printf("restart: exec failed: %v", err)
	}
}

// spaHandler serves static files and falls back to index.html for unknown paths,
// enabling client-side routing in the React SPA.
func spaHandler(fsys http.FileSystem) http.Handler {
	fileServer := http.FileServer(fsys)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f, err := fsys.Open(r.URL.Path)
		if err != nil {
			r2 := *r
			r2.URL.Path = "/"
			fileServer.ServeHTTP(w, &r2)
			return
		}
		_ = f.Close()
		fileServer.ServeHTTP(w, r)
	})
}
