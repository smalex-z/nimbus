package config

import (
	"os"
)

// Config holds all application configuration.
type Config struct {
	Port                string
	DBPath              string
	CORSOrigin          string
	Env                 string
	AppURL              string
	GitHubClientID      string
	GitHubClientSecret  string
	GoogleClientID      string
	GoogleClientSecret  string
}

// Load reads configuration from environment variables with sensible defaults.
func Load() *Config {
	return &Config{
		Port:               getEnv("PORT", "8080"),
		DBPath:             getEnv("DB_PATH", "./nimbus.db"),
		CORSOrigin:         getEnv("CORS_ORIGIN", "*"),
		Env:                getEnv("APP_ENV", "production"),
		AppURL:             getEnv("APP_URL", "http://localhost:5173"),
		GitHubClientID:     os.Getenv("GITHUB_CLIENT_ID"),
		GitHubClientSecret: os.Getenv("GITHUB_CLIENT_SECRET"),
		GoogleClientID:     os.Getenv("GOOGLE_CLIENT_ID"),
		GoogleClientSecret: os.Getenv("GOOGLE_CLIENT_SECRET"),
	}
}

func getEnv(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}
