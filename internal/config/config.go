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
	IPPoolStart string
	IPPoolEnd   string
	GatewayIP   string
	// VMPrefixLen is the netmask length applied to every VM's cloud-init
	// ipconfig0. Default 24 matches the historical hardcoded value, so
	// existing deployments keep their current behaviour without touching
	// .env. Set to 16 (or whatever) when the cluster's VM bridge is on a
	// larger network than a single /24. After first boot, the DB owns the
	// effective value (see db.NetworkSettings); env is seed-only.
	VMPrefixLen  int
	Nameserver   string
	SearchDomain string

	// VMCPUType is the Proxmox `cpu` model applied to every provisioned VM.
	// Default x86-64-v3 (Haswell baseline) guarantees AVX2 in the guest while
	// remaining portable across any Haswell-or-newer host. Override via
	// VM_CPU_TYPE — e.g. "host" for max performance on a single-host setup,
	// or "x86-64-v2-AES" if any cluster node predates Haswell.
	VMCPUType string

	// Cross-instance IP reconciliation. Defaults are tuned for the typical
	// "two operators sharing one Proxmox cluster" deployment; raise the
	// vacate threshold if your cluster has long-running migrations.
	ReconcileIntervalSeconds int // background reconcile cadence — default 60
	ReservationTTLSeconds    int // stale-reservation cutoff       — default 600 (10m)
	VerifyCacheTTLSeconds    int // ListClusterIPs cache reuse     — default 5
	VacateMissThreshold      int // consecutive missing cycles before auto-vacate — default 3

	// AuditRetentionDays caps how long audit_events rows live before
	// the daily reaper deletes them. Default 90 (matches what most
	// enterprise audit systems land on). 0 disables the reaper —
	// useful for small homelabs that want forever-retention but most
	// deployments should keep this bounded.
	AuditRetentionDays int

	// Netscan — best-effort detection of non-VM hosts on the LAN that share
	// the IP pool range (gateway, NAS, IoT, statically-assigned workstations).
	// Closes a hole in the Proxmox reconciler, which only sees VM-claimed IPs.
	//
	// NetscanMode:     off | tcp | arp | both — default "arp" (passive ARP cache
	//                  read only). The active TCP probe (`tcp` or `both`) trips
	//                  IDS/IPS rules and reads as a horizontal port scan on
	//                  corporate networks; opt in with `both` on a homelab.
	// NetscanInterval: scan cadence in seconds — default 300 (5min); 0 disables
	// NetscanTimeoutMS: per-port TCP dial timeout — default 200
	// NetscanConcurrency: parallel probes — default 50
	NetscanMode         string
	NetscanIntervalSecs int
	NetscanTimeoutMS    int
	NetscanConcurrency  int

	// Node-scoring knobs. VMDiskStorage names the Proxmox storage pool the
	// disk gate checks; an empty value disables the disk gate. MemBufferMiB
	// adds RAM headroom on top of the tier's request to avoid packing to
	// zero. CPULoadFactor (K) is the share of a fresh VM's vCPUs the soft
	// score assumes consumed (0.5 ≈ "half-busy on average").
	VMDiskStorage string
	MemBufferMiB  uint64
	CPULoadFactor float64

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
		VMPrefixLen:             getEnvInt("VM_PREFIX_LEN", 24),
		Nameserver:              getEnv("NAMESERVER", "1.1.1.1 8.8.8.8"),
		SearchDomain:            getEnv("SEARCH_DOMAIN", "local"),
		VMCPUType:               getEnv("VM_CPU_TYPE", "x86-64-v3"),

		ReconcileIntervalSeconds: getEnvInt("RECONCILE_INTERVAL_SECONDS", 60),
		ReservationTTLSeconds:    getEnvInt("RESERVATION_TTL_SECONDS", 600),
		VerifyCacheTTLSeconds:    getEnvInt("VERIFY_CACHE_TTL_SECONDS", 5),
		VacateMissThreshold:      getEnvInt("VACATE_MISS_THRESHOLD", 3),
		AuditRetentionDays:       getEnvInt("NIMBUS_AUDIT_RETENTION_DAYS", 90),

		NetscanMode:         getEnv("NIMBUS_NETSCAN_MODE", "arp"),
		NetscanIntervalSecs: getEnvInt("NIMBUS_NETSCAN_INTERVAL_SECONDS", 300),
		NetscanTimeoutMS:    getEnvInt("NIMBUS_NETSCAN_TIMEOUT_MS", 200),
		NetscanConcurrency:  getEnvInt("NIMBUS_NETSCAN_CONCURRENCY", 50),

		VMDiskStorage: getEnv("NIMBUS_VM_DISK_STORAGE", "local-lvm"),
		MemBufferMiB:  uint64(getEnvInt("NIMBUS_MEM_BUFFER_MIB", 256)),
		CPULoadFactor: getEnvFloat("NIMBUS_CPU_LOAD_FACTOR", 0.5),

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

// IsConfigured reports whether all required fields are present.
// Used to decide between setup mode and normal startup.
func (c *Config) IsConfigured() bool {
	return c.ProxmoxHost != "" &&
		c.ProxmoxTokenID != "" &&
		c.ProxmoxTokenSecret != "" &&
		c.IPPoolStart != "" &&
		c.IPPoolEnd != "" &&
		c.GatewayIP != ""
}

// EnvValues holds the fields written by the setup wizard.
type EnvValues struct {
	ProxmoxHost        string
	ProxmoxTokenID     string
	ProxmoxTokenSecret string
	IPPoolStart        string
	IPPoolEnd          string
	GatewayIP          string
	// VMPrefixLen is the netmask length applied to every VM's cloud-init
	// ipconfig0 (24, 16, etc.). Captured by the wizard so the operator
	// chooses their subnet upfront. Zero is treated as "use default 24"
	// by callers (the wizard handler defaults; the env loader uses
	// getEnvInt with a 24 fallback).
	VMPrefixLen  int
	Nameserver   string
	SearchDomain string
	Port         string
	// Optional Gopher tunnel credentials — collected from the wizard's
	// "optional" section. Empty when the operator hasn't configured them.
	// Seeded into db.GopherSettings on first boot (see main.go); after that,
	// the DB is the source of truth and admins can rotate from the
	// Authentication page.
	GopherAPIURL string
	GopherAPIKey string
}

// EnvFilePath returns the path where the runtime config should be written.
// Uses /etc/nimbus/nimbus.env when that directory exists and is writable
// (i.e. production after `nimbus install`), otherwise .env in the CWD.
func EnvFilePath() string {
	const prod = "/etc/nimbus/nimbus.env"
	if fi, err := os.Stat("/etc/nimbus"); err == nil && fi.IsDir() {
		if f, err := os.OpenFile(prod, os.O_WRONLY|os.O_CREATE|os.O_APPEND, 0600); err == nil {
			_ = f.Close()
			return prod
		}
	}
	return ".env"
}

// WriteEnvFile writes v to path as KEY=VALUE pairs, replacing any existing file.
func WriteEnvFile(path string, v EnvValues) error {
	prefix := v.VMPrefixLen
	if prefix < 1 || prefix > 32 {
		prefix = 24
	}
	content := fmt.Sprintf(
		"# Nimbus configuration — written by setup wizard.\n"+
			"PROXMOX_HOST=%s\n"+
			"PROXMOX_TOKEN_ID=%s\n"+
			"PROXMOX_TOKEN_SECRET=%s\n"+
			"IP_POOL_START=%s\n"+
			"IP_POOL_END=%s\n"+
			"GATEWAY_IP=%s\n"+
			"VM_PREFIX_LEN=%d\n"+
			"NAMESERVER=%s\n"+
			"SEARCH_DOMAIN=%s\n"+
			"PORT=%s\n"+
			"GOPHER_API_URL=%s\n"+
			"GOPHER_API_KEY=%s\n",
		v.ProxmoxHost, v.ProxmoxTokenID, v.ProxmoxTokenSecret,
		v.IPPoolStart, v.IPPoolEnd, v.GatewayIP,
		prefix,
		v.Nameserver, v.SearchDomain, v.Port,
		v.GopherAPIURL, v.GopherAPIKey,
	)
	return os.WriteFile(path, []byte(content), 0600)
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

func getEnvFloat(key string, fallback float64) float64 {
	if v := os.Getenv(key); v != "" {
		if n, err := strconv.ParseFloat(v, 64); err == nil {
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
