package handlers

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	"nimbus/internal/api/response"
	"nimbus/internal/config"
	"nimbus/internal/proxmox"
)

// Setup handles the first-run configuration wizard API.
type Setup struct {
	cfg     *config.Config
	restart func()
}

func NewSetup(cfg *config.Config, restart func()) *Setup {
	return &Setup{cfg: cfg, restart: restart}
}

// Status handles GET /api/setup/status.
func (h *Setup) Status(w http.ResponseWriter, r *http.Request) {
	response.Success(w, map[string]bool{"configured": h.cfg.IsConfigured()})
}

type testConnRequest struct {
	ProxmoxHost        string `json:"proxmox_host"`
	ProxmoxTokenID     string `json:"proxmox_token_id"`
	ProxmoxTokenSecret string `json:"proxmox_token_secret"`
}

// Test handles POST /api/setup/test — probes Proxmox without persisting anything.
func (h *Setup) Test(w http.ResponseWriter, r *http.Request) {
	var req testConnRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		response.BadRequest(w, "invalid JSON")
		return
	}
	if req.ProxmoxHost == "" || req.ProxmoxTokenID == "" || req.ProxmoxTokenSecret == "" {
		response.BadRequest(w, "proxmox_host, proxmox_token_id, and proxmox_token_secret are required")
		return
	}

	client := proxmox.New(req.ProxmoxHost, req.ProxmoxTokenID, req.ProxmoxTokenSecret, 10*time.Second)
	ctx, cancel := context.WithTimeout(r.Context(), 8*time.Second)
	defer cancel()

	version, err := client.Version(ctx)
	if err != nil {
		response.Error(w, http.StatusBadGateway, "cannot reach Proxmox: "+err.Error())
		return
	}
	response.Success(w, map[string]string{"proxmox_version": version})
}

type saveConfigRequest struct {
	ProxmoxHost        string `json:"proxmox_host"`
	ProxmoxTokenID     string `json:"proxmox_token_id"`
	ProxmoxTokenSecret string `json:"proxmox_token_secret"`
	IPPoolStart        string `json:"ip_pool_start"`
	IPPoolEnd          string `json:"ip_pool_end"`
	GatewayIP          string `json:"gateway_ip"`
	Nameserver         string `json:"nameserver"`
	SearchDomain       string `json:"search_domain"`
	Port               string `json:"port"`
}

// Save handles POST /api/setup/save — validates, writes the env file,
// injects env vars, responds 200, then restarts the process to boot normally.
func (h *Setup) Save(w http.ResponseWriter, r *http.Request) {
	var req saveConfigRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		response.BadRequest(w, "invalid JSON")
		return
	}

	for _, f := range []struct{ name, value string }{
		{"proxmox_host", req.ProxmoxHost},
		{"proxmox_token_id", req.ProxmoxTokenID},
		{"proxmox_token_secret", req.ProxmoxTokenSecret},
		{"ip_pool_start", req.IPPoolStart},
		{"ip_pool_end", req.IPPoolEnd},
		{"gateway_ip", req.GatewayIP},
	} {
		if f.value == "" {
			response.BadRequest(w, f.name+" is required")
			return
		}
	}
	for _, f := range []struct{ name, value string }{
		{"ip_pool_start", req.IPPoolStart},
		{"ip_pool_end", req.IPPoolEnd},
		{"gateway_ip", req.GatewayIP},
	} {
		if net.ParseIP(f.value) == nil {
			response.BadRequest(w, f.name+": invalid IP address")
			return
		}
	}

	if req.Nameserver == "" {
		req.Nameserver = "1.1.1.1 8.8.8.8"
	}
	if req.SearchDomain == "" {
		req.SearchDomain = "local"
	}
	if req.Port == "" {
		req.Port = "8080"
	}

	envPath := config.EnvFilePath()
	if err := config.WriteEnvFile(envPath, config.EnvValues{
		ProxmoxHost:        req.ProxmoxHost,
		ProxmoxTokenID:     req.ProxmoxTokenID,
		ProxmoxTokenSecret: req.ProxmoxTokenSecret,
		IPPoolStart:        req.IPPoolStart,
		IPPoolEnd:          req.IPPoolEnd,
		GatewayIP:          req.GatewayIP,
		Nameserver:         req.Nameserver,
		SearchDomain:       req.SearchDomain,
		Port:               req.Port,
	}); err != nil {
		response.InternalError(w, "failed to write config: "+err.Error())
		return
	}

	// Inject into the current process env so syscall.Exec picks them up.
	_ = os.Setenv("PROXMOX_HOST", req.ProxmoxHost)
	_ = os.Setenv("PROXMOX_TOKEN_ID", req.ProxmoxTokenID)
	_ = os.Setenv("PROXMOX_TOKEN_SECRET", req.ProxmoxTokenSecret)
	_ = os.Setenv("IP_POOL_START", req.IPPoolStart)
	_ = os.Setenv("IP_POOL_END", req.IPPoolEnd)
	_ = os.Setenv("GATEWAY_IP", req.GatewayIP)
	_ = os.Setenv("NAMESERVER", req.Nameserver)
	_ = os.Setenv("SEARCH_DOMAIN", req.SearchDomain)
	_ = os.Setenv("PORT", req.Port)

	response.Success(w, map[string]string{"message": "configuration saved"})

	go func() {
		time.Sleep(500 * time.Millisecond)
		h.restart()
	}()
}

type discoverResult struct {
	IsHypervisor     bool     `json:"is_hypervisor"`
	Endpoints        []string `json:"endpoints"`
	SuggestedGateway string   `json:"suggested_gateway,omitempty"`
}

// Discover handles GET /api/setup/discover.
// It uses two complementary sources and merges the results:
//   - corosync.conf (authoritative cluster membership, present on PVE nodes)
//   - TCP port scan of all local subnets (works from any machine)
func (h *Setup) Discover(w http.ResponseWriter, r *http.Request) {
	result := discoverResult{Endpoints: []string{}}

	if _, err := os.Stat("/etc/pve"); err == nil {
		result.IsHypervisor = true
		result.SuggestedGateway = defaultGateway()
	}

	// Source 1: corosync cluster membership (instant, authoritative on PVE nodes).
	// When on a hypervisor, lead with localhost (IP-independent, survives network changes),
	// then list only remote cluster nodes (skip this machine's own IPs).
	clusterIPs := corosyncNodeIPs()
	if result.IsHypervisor {
		// Always put localhost first on hypervisors
		result.Endpoints = append(result.Endpoints, "https://localhost:8006")
		
		// Add remote cluster nodes, skipping local IPs
		if len(clusterIPs) > 0 {
			localIPs := localIPv4s()
			for _, ip := range clusterIPs {
				if containsStr(localIPs, ip) {
					continue // skip—localhost already covers this machine
				}
				url := "https://" + ip + ":8006"
				if !containsStr(result.Endpoints, url) {
					result.Endpoints = append(result.Endpoints, url)
				}
			}
		}
	} else {
		// On non-hypervisor, add cluster nodes as-is
		for _, ip := range clusterIPs {
			url := "https://" + ip + ":8006"
			if !containsStr(result.Endpoints, url) {
				result.Endpoints = append(result.Endpoints, url)
			}
		}
	}

	// Source 2: subnet scan — supplements corosync and works from any host.
	ctx, cancel := context.WithTimeout(r.Context(), 6*time.Second)
	defer cancel()

	for _, ip := range scanPort8006(ctx) {
		url := "https://" + ip + ":8006"
		if !containsStr(result.Endpoints, url) {
			result.Endpoints = append(result.Endpoints, url)
		}
	}

	response.Success(w, result)
}

// corosyncNodeIPs reads Proxmox cluster member IPs from /etc/pve/corosync.conf.
func corosyncNodeIPs() []string {
	data, err := os.ReadFile("/etc/pve/corosync.conf")
	if err != nil {
		return nil
	}
	var ips []string
	scanner := bufio.NewScanner(strings.NewReader(string(data)))
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if !strings.HasPrefix(line, "ring0_addr:") {
			continue
		}
		addr := strings.TrimSpace(strings.TrimPrefix(line, "ring0_addr:"))
		if net.ParseIP(addr) != nil {
			ips = append(ips, addr)
		}
	}
	return ips
}

// scanPort8006 returns the IPs across all local subnets that have TCP 8006 open.
func scanPort8006(ctx context.Context) []string {
	seen := map[string]bool{}
	var allHosts []string
	for _, subnet := range localSubnets() {
		for _, h := range subnetHosts(subnet) {
			if !seen[h] {
				seen[h] = true
				allHosts = append(allHosts, h)
			}
		}
	}
	if len(allHosts) == 0 {
		return nil
	}
	// Cap to avoid unreasonably long scans on very large subnets.
	if len(allHosts) > 1024 {
		allHosts = allHosts[:1024]
	}

	var (
		mu    sync.Mutex
		found []string
		sem   = make(chan struct{}, 128)
		wg    sync.WaitGroup
	)

	dialer := &net.Dialer{}
outer:
	for _, ip := range allHosts {
		ip := ip
		select {
		case <-ctx.Done():
			break outer
		case sem <- struct{}{}:
		}
		wg.Add(1)
		go func() {
			defer wg.Done()
			defer func() { <-sem }()
			dialCtx, cancel := context.WithTimeout(ctx, 400*time.Millisecond)
			defer cancel()
			conn, err := dialer.DialContext(dialCtx, "tcp", ip+":8006")
			if err == nil {
				_ = conn.Close()
				mu.Lock()
				found = append(found, ip)
				mu.Unlock()
			}
		}()
	}
	wg.Wait()
	return found
}

// localIPv4s returns all non-loopback IPv4 addresses on this host.
func localIPv4s() []string {
	ifaces, err := net.Interfaces()
	if err != nil {
		return nil
	}
	var ips []string
	for _, iface := range ifaces {
		if iface.Flags&net.FlagUp == 0 || iface.Flags&net.FlagLoopback != 0 {
			continue
		}
		addrs, _ := iface.Addrs()
		for _, addr := range addrs {
			if ipNet, ok := addr.(*net.IPNet); ok && ipNet.IP.To4() != nil {
				ips = append(ips, ipNet.IP.String())
			}
		}
	}
	return ips
}

// localSubnets returns the IPv4 subnets of all non-loopback interfaces.
func localSubnets() []*net.IPNet {
	ifaces, err := net.Interfaces()
	if err != nil {
		return nil
	}
	var subnets []*net.IPNet
	for _, iface := range ifaces {
		if iface.Flags&net.FlagUp == 0 || iface.Flags&net.FlagLoopback != 0 {
			continue
		}
		addrs, _ := iface.Addrs()
		for _, addr := range addrs {
			if ipNet, ok := addr.(*net.IPNet); ok && ipNet.IP.To4() != nil {
				subnets = append(subnets, ipNet)
			}
		}
	}
	return subnets
}

// subnetHosts returns all host addresses in subnet (excludes network and broadcast).
func subnetHosts(subnet *net.IPNet) []string {
	var hosts []string
	// Start from the network base address.
	base := subnet.IP.To4()
	if base == nil {
		return nil
	}
	ip := make(net.IP, 4)
	copy(ip, base)
	for subnet.Contains(ip) {
		// Skip network address (.0) and broadcast (.255).
		if ip[3] != 0 && ip[3] != 255 {
			hosts = append(hosts, ip.String())
		}
		// Increment in place.
		for i := 3; i >= 0; i-- {
			ip[i]++
			if ip[i] != 0 {
				break
			}
		}
	}
	return hosts
}

// defaultGateway reads the default route from /proc/net/route.
// The gateway field is a little-endian hex IPv4 address.
func defaultGateway() string {
	f, err := os.Open("/proc/net/route")
	if err != nil {
		return ""
	}
	defer f.Close() //nolint:errcheck

	scanner := bufio.NewScanner(f)
	scanner.Scan() // skip header line
	for scanner.Scan() {
		fields := strings.Fields(scanner.Text())
		if len(fields) < 3 {
			continue
		}
		if fields[1] != "00000000" { // destination 0.0.0.0 = default route
			continue
		}
		gwHex := fields[2]
		if len(gwHex) != 8 {
			continue
		}
		b := make([]byte, 4)
		for i := range 4 {
			v, err := strconv.ParseUint(gwHex[i*2:i*2+2], 16, 8)
			if err != nil {
				return ""
			}
			b[3-i] = byte(v) // little-endian → network order
		}
		return fmt.Sprintf("%d.%d.%d.%d", b[0], b[1], b[2], b[3])
	}
	return ""
}

func containsStr(ss []string, s string) bool {
	for _, v := range ss {
		if v == s {
			return true
		}
	}
	return false
}
