package main

import (
	"embed"
	"flag"
	"io/fs"
	"log"
	"net/http"
	"os"

	"nimbus/internal/api"
	"nimbus/internal/build"
	"nimbus/internal/config"
	"nimbus/internal/db"
	"nimbus/internal/oauth"
	"nimbus/internal/service"

	"github.com/joho/godotenv"
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

	if err := godotenv.Load(); err != nil && !os.IsNotExist(err) {
		log.Printf("warning: could not load .env: %v", err)
	}

	cfg := config.Load()
	if *port != "" {
		cfg.Port = *port
	}
	if *dbPath != "" {
		cfg.DBPath = *dbPath
	}

	database, err := db.New(cfg.DBPath)
	if err != nil {
		log.Fatalf("failed to open database: %v", err)
	}

	svc := service.NewExampleService(database)
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

	router := api.NewRouter(svc, authSvc, github, google)

	distFS, err := fs.Sub(frontendFS, "frontend/dist")
	if err != nil {
		log.Fatalf("failed to create frontend sub-filesystem: %v", err)
	}

	mux := http.NewServeMux()
	mux.Handle("/api/", router)
	mux.Handle("/", spaHandler(http.FS(distFS)))

	log.Printf("nimbus %s starting on :%s", build.Version, cfg.Port)
	if err := http.ListenAndServe(":"+cfg.Port, mux); err != nil {
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
