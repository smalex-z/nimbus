package config

import (
	"bufio"
	"fmt"
	"os"
	"strconv"
	"strings"
)

// Config holds all Nimbus runtime configuration.
//
// Values are sourced from environment variables. Defaults apply when an env var
// is unset or empty. Required fields (PROXMOX_*, IP_POOL_*, GATEWAY_IP) have no
// default and Validate() reports them as missing.
type Config struct {
	// Server
	Port       string
	DBPath     string
	CORSOrigin string
	Env        string

	// Proxmox
	ProxmoxHost             string
	ProxmoxTokenID          string
	ProxmoxTokenSecret      string
	ProxmoxTemplateBaseVMID int
	ExcludedNodes           []string

	// Networking
	IPPoolStart  string
	IPPoolEnd    string
	GatewayIP    string
	Nameserver   string
	SearchDomain string

	// OAuth
	AppURL             string
	GitHubClientID     string
	GitHubClientSecret string
	GoogleClientID     string
	GoogleClientSecret string

	// Optional integrations
	GopherAPIURL string
	GopherAPIKey string
}

// Load reads configuration from process environment. If `.env` exists in the
// current working directory it is loaded first; existing process env vars take
// precedence over file values.
func Load() (*Config, error) {
	if err := loadDotEnv(".env"); err != nil {
		return nil, fmt.Errorf("load .env: %w", err)
	}

	cfg := &Config{
		Port:                    getEnv("PORT", "8080"),
		DBPath:                  getEnv("DB_PATH", "./nimbus.db"),
		CORSOrigin:              getEnv("CORS_ORIGIN", "*"),
		Env:                     getEnv("APP_ENV", "production"),
		ProxmoxHost:             os.Getenv("PROXMOX_HOST"),
		ProxmoxTokenID:          os.Getenv("PROXMOX_TOKEN_ID"),
		ProxmoxTokenSecret:      os.Getenv("PROXMOX_TOKEN_SECRET"),
		ProxmoxTemplateBaseVMID: getEnvInt("PROXMOX_TEMPLATE_BASE_VMID", 9000),
		ExcludedNodes:           splitCSV(os.Getenv("NIMBUS_EXCLUDED_NODES")),
		IPPoolStart:             os.Getenv("IP_POOL_START"),
		IPPoolEnd:               os.Getenv("IP_POOL_END"),
		GatewayIP:               os.Getenv("GATEWAY_IP"),
		Nameserver:              getEnv("NAMESERVER", "1.1.1.1 8.8.8.8"),
		SearchDomain:            getEnv("SEARCH_DOMAIN", "local"),
		AppURL:             getEnv("APP_URL", "http://localhost:5173"),
		GitHubClientID:     os.Getenv("GITHUB_CLIENT_ID"),
		GitHubClientSecret: os.Getenv("GITHUB_CLIENT_SECRET"),
		GoogleClientID:     os.Getenv("GOOGLE_CLIENT_ID"),
		GoogleClientSecret: os.Getenv("GOOGLE_CLIENT_SECRET"),
		GopherAPIURL:       os.Getenv("GOPHER_API_URL"),
		GopherAPIKey:       os.Getenv("GOPHER_API_KEY"),
	}

	return cfg, nil
}

// Validate returns an error listing any missing required fields. Optional
// integrations (OAuth, Gopher) are not checked.
func (c *Config) Validate() error {
	var missing []string
	check := func(name, value string) {
		if value == "" {
			missing = append(missing, name)
		}
	}
	check("PROXMOX_HOST", c.ProxmoxHost)
	check("PROXMOX_TOKEN_ID", c.ProxmoxTokenID)
	check("PROXMOX_TOKEN_SECRET", c.ProxmoxTokenSecret)
	check("IP_POOL_START", c.IPPoolStart)
	check("IP_POOL_END", c.IPPoolEnd)
	check("GATEWAY_IP", c.GatewayIP)

	if len(missing) > 0 {
		return fmt.Errorf("missing required config: %s", strings.Join(missing, ", "))
	}
	return nil
}

// loadDotEnv parses a simple KEY=VALUE file. Lines beginning with '#' are
// comments. Values may be optionally surrounded by single or double quotes.
// Existing environment variables are NOT overridden — explicit env wins.
//
// Missing file is not an error.
func loadDotEnv(path string) error {
	f, err := os.Open(path) //nolint:gosec // path is configurable by operator
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	defer f.Close() //nolint:errcheck

	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		eq := strings.IndexByte(line, '=')
		if eq <= 0 {
			continue
		}
		key := strings.TrimSpace(line[:eq])
		value := strings.TrimSpace(line[eq+1:])
		// Strip surrounding quotes.
		if len(value) >= 2 {
			first, last := value[0], value[len(value)-1]
			if (first == '"' && last == '"') || (first == '\'' && last == '\'') {
				value = value[1 : len(value)-1]
			}
		}
		if _, set := os.LookupEnv(key); set {
			continue
		}
		if err := os.Setenv(key, value); err != nil {
			return fmt.Errorf("set %s: %w", key, err)
		}
	}
	return scanner.Err()
}

func getEnv(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

func getEnvInt(key string, fallback int) int {
	if v := os.Getenv(key); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			return n
		}
	}
	return fallback
}

func splitCSV(value string) []string {
	if value == "" {
		return nil
	}
	parts := strings.Split(value, ",")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		if t := strings.TrimSpace(p); t != "" {
			out = append(out, t)
		}
	}
	return out
}
