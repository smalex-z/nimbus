package main

import (
	"context"
	"embed"
	"flag"
	"io/fs"
	"log"
	"net/http"
	"os"
	"syscall"
	"time"

	"nimbus/internal/api"
	"nimbus/internal/build"
	"nimbus/internal/config"
	"nimbus/internal/db"
	"nimbus/internal/install"
	"nimbus/internal/ippool"
	"nimbus/internal/provision"
	"nimbus/internal/proxmox"
)

//go:embed all:frontend/dist
var frontendFS embed.FS

func main() {
	if len(os.Args) > 1 && os.Args[1] == "install" {
		install.Run()
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

	database, err := db.New(cfg.DBPath, ippool.Model(), &db.VM{})
	if err != nil {
		log.Fatalf("failed to open database: %v", err)
	}

	pool := ippool.New(database.DB)
	if err := pool.Seed(context.Background(), cfg.IPPoolStart, cfg.IPPoolEnd); err != nil {
		log.Fatalf("failed to seed IP pool: %v", err)
	}

	pveClient := proxmox.New(cfg.ProxmoxHost, cfg.ProxmoxTokenID, cfg.ProxmoxTokenSecret, 30*time.Second)

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

	provSvc := provision.New(pveClient, pool, database.DB, provision.Config{
		TemplateBaseVMID: cfg.ProxmoxTemplateBaseVMID,
		ExcludedNodes:    cfg.ExcludedNodes,
		GatewayIP:        cfg.GatewayIP,
		Nameserver:       cfg.Nameserver,
		SearchDomain:     cfg.SearchDomain,
	})
	provSvc.SetIPVerifier(reconciler)

	router := api.NewRouter(api.Deps{
		Provision:  provSvc,
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
	}
	if err := srv.ListenAndServe(); err != nil {
		log.Fatalf("server error: %v", err)
	}
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
