package main

import (
	"context"
	"embed"
	"flag"
	"io/fs"
	"log"
	"net/http"
	"os"
	"time"

	"nimbus/internal/api"
	"nimbus/internal/build"
	"nimbus/internal/config"
	"nimbus/internal/db"
	"nimbus/internal/ippool"
	"nimbus/internal/oauth"
	"nimbus/internal/provision"
	"nimbus/internal/proxmox"
	"nimbus/internal/service"
)

//go:embed all:frontend/dist
var frontendFS embed.FS

func main() {
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
	if err := cfg.Validate(); err != nil {
		log.Fatalf("invalid configuration: %v", err)
	}

	database, err := db.New(cfg.DBPath,
		&db.User{}, &db.Session{}, &db.VM{}, ippool.Model(),
	)
	if err != nil {
		log.Fatalf("failed to open database: %v", err)
	}

	authSvc := service.NewAuthService(database)

	var github oauth.Provider
	if cfg.GitHubClientID != "" && cfg.GitHubClientSecret != "" {
		github = &oauth.GitHub{
			ClientID:     cfg.GitHubClientID,
			ClientSecret: cfg.GitHubClientSecret,
		}
		log.Printf("GitHub OAuth enabled")
	}

	var google oauth.Provider
	if cfg.GoogleClientID != "" && cfg.GoogleClientSecret != "" {
		google = &oauth.Google{
			ClientID:     cfg.GoogleClientID,
			ClientSecret: cfg.GoogleClientSecret,
			RedirectURI:  cfg.AppURL + "/api/auth/google/callback",
		}
		log.Printf("Google OAuth enabled")
	}

	pool := ippool.New(database.DB)
	if err := pool.Seed(context.Background(), cfg.IPPoolStart, cfg.IPPoolEnd); err != nil {
		log.Fatalf("failed to seed IP pool: %v", err)
	}

	pveClient := proxmox.New(cfg.ProxmoxHost, cfg.ProxmoxTokenID, cfg.ProxmoxTokenSecret, 30*time.Second)

	provSvc := provision.New(pveClient, pool, database.DB, provision.Config{
		TemplateBaseVMID: cfg.ProxmoxTemplateBaseVMID,
		ExcludedNodes:    cfg.ExcludedNodes,
		GatewayIP:        cfg.GatewayIP,
		Nameserver:       cfg.Nameserver,
		SearchDomain:     cfg.SearchDomain,
	})

	router := api.NewRouter(api.Deps{
		Auth:      authSvc,
		GitHub:    github,
		Google:    google,
		Provision: provSvc,
		Pool:      pool,
		Proxmox:   pveClient,
	})

	distFS, err := fs.Sub(frontendFS, "frontend/dist")
	if err != nil {
		log.Fatalf("failed to create frontend sub-filesystem: %v", err)
	}

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
