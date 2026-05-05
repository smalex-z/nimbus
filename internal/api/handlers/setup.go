package handlers

import (
	"bufio"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
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
	"nimbus/internal/service"
)

// Setup handles the first-run configuration wizard API.
type Setup struct {
	cfg     *config.Config
	restart func()
	auth    *service.AuthService // nil in setup mode (Proxmox not yet configured)
}

func NewSetup(cfg *config.Config, restart func()) *Setup {
	return &Setup{cfg: cfg, restart: restart}
}

// NewSetupWithAuth is used in normal mode where the DB is available.
func NewSetupWithAuth(cfg *config.Config, restart func(), auth *service.AuthService) *Setup {
	return &Setup{cfg: cfg, restart: restart, auth: auth}
}

// setupStatusView is the JSON shape returned by GET /api/setup/status.
type setupStatusView struct {
	Configured      bool `json:"configured"`
	NeedsAdminSetup bool `json:"needs_admin_setup"`
}

// Status handles GET /api/setup/status.
//
// @Summary     First-run setup status
// @Description Public endpoint the SPA polls before the wizard renders.
// @Description `configured` reflects whether Proxmox + IP-pool config is
// @Description present; `needs_admin_setup` reflects whether the user table
// @Description is empty (so the wizard should prompt for an admin account).
// @Tags        setup
// @Produce     json
// @Success     200 {object} EnvelopeOK{data=setupStatusView}
// @Router      /setup/status [get]
func (h *Setup) Status(w http.ResponseWriter, r *http.Request) {
	needsAdminSetup := true
	if h.auth != nil {
		has, err := h.auth.HasAnyUsers()
		if err == nil {
			needsAdminSetup = !has
		}
	}
	response.Success(w, setupStatusView{
		Configured:      h.cfg.IsConfigured(),
		NeedsAdminSetup: needsAdminSetup,
	})
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
	// VMPrefixLen is the netmask length the wizard captures so the operator
	// picks /16 vs /24 vs whatever upfront. 0 falls back to the historical
	// default of 24 in the env writer below.
	VMPrefixLen  int    `json:"vm_prefix_len"`
	Nameserver   string `json:"nameserver"`
	SearchDomain string `json:"search_domain"`
	Port         string `json:"port"`
	GopherAPIURL string `json:"gopher_api_url"`
	GopherAPIKey string `json:"gopher_api_key"`
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
	if req.VMPrefixLen == 0 {
		req.VMPrefixLen = 24
	} else if req.VMPrefixLen < 1 || req.VMPrefixLen > 32 {
		response.BadRequest(w, "vm_prefix_len must be between 1 and 32")
		return
	}

	envPath := config.EnvFilePath()
	if err := config.WriteEnvFile(envPath, config.EnvValues{
		ProxmoxHost:        req.ProxmoxHost,
		ProxmoxTokenID:     req.ProxmoxTokenID,
		ProxmoxTokenSecret: req.ProxmoxTokenSecret,
		IPPoolStart:        req.IPPoolStart,
		IPPoolEnd:          req.IPPoolEnd,
		GatewayIP:          req.GatewayIP,
		VMPrefixLen:        req.VMPrefixLen,
		Nameserver:         req.Nameserver,
		SearchDomain:       req.SearchDomain,
		Port:               req.Port,
		GopherAPIURL:       req.GopherAPIURL,
		GopherAPIKey:       req.GopherAPIKey,
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
	_ = os.Setenv("VM_PREFIX_LEN", strconv.Itoa(req.VMPrefixLen))
	_ = os.Setenv("NAMESERVER", req.Nameserver)
	_ = os.Setenv("SEARCH_DOMAIN", req.SearchDomain)
	_ = os.Setenv("PORT", req.Port)
	_ = os.Setenv("GOPHER_API_URL", req.GopherAPIURL)
	_ = os.Setenv("GOPHER_API_KEY", req.GopherAPIKey)

	response.Success(w, map[string]string{"message": "configuration saved"})

	go func() {
		time.Sleep(500 * time.Millisecond)
		h.restart()
	}()
}

type createAdminRequest struct {
	Name     string `json:"name"`
	Email    string `json:"email"`
	Password string `json:"password"`
}

// CreateAdmin handles POST /api/setup/admin — creates the first admin account
// and opens a session. Returns 409 if any user already exists.
//
// @Summary     Create the first admin account
// @Description One-shot endpoint used by the install wizard. 409 once any
// @Description user exists. On success, sets the session cookie inline.
// @Tags        setup
// @Accept      json
// @Produce     json
// @Param       body body     createAdminRequest true "Admin account"
// @Success     200  {object} EnvelopeOK{data=service.UserView}
// @Failure     400  {object} EnvelopeError
// @Failure     409  {object} EnvelopeError
// @Failure     500  {object} EnvelopeError
// @Failure     503  {object} EnvelopeError
// @Router      /setup/admin [post]
func (h *Setup) CreateAdmin(w http.ResponseWriter, r *http.Request) {
	if h.auth == nil {
		response.Error(w, http.StatusServiceUnavailable, "server is in setup mode")
		return
	}

	var req createAdminRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		response.BadRequest(w, "invalid JSON")
		return
	}

	req.Name = strings.TrimSpace(req.Name)
	req.Email = strings.TrimSpace(req.Email)

	switch {
	case req.Name == "":
		response.BadRequest(w, "name is required")
		return
	case req.Email == "":
		response.BadRequest(w, "email is required")
		return
	case len(req.Password) < 8:
		response.BadRequest(w, "password must be at least 8 characters")
		return
	}

	user, err := h.auth.RegisterFirstAdmin(service.RegisterParams{
		Name:     req.Name,
		Email:    req.Email,
		Password: req.Password,
	})
	if errors.Is(err, service.ErrUsersExist) {
		response.Conflict(w, "admin account already exists")
		return
	}
	if err != nil {
		response.InternalError(w, "failed to create admin account")
		return
	}

	sessionID, err := h.auth.CreateSession(user.ID)
	if err != nil {
		response.InternalError(w, "failed to create session")
		return
	}

	setSessionCookie(w, sessionID)
	response.Success(w, user)
}

// DiscoveredEndpoint pairs a Proxmox API URL with the node name it points at
// (when known). The SPA uses NodeName as the primary label and falls back to
// the IP/host portion of URL when name resolution failed.
type DiscoveredEndpoint struct {
	URL      string `json:"url"`
	IP       string `json:"ip"`
	NodeName string `json:"node_name,omitempty"`
	// Source tells the SPA where this entry came from so it can group
	// or label appropriately. "localhost" only appears on hypervisor
	// installs; "corosync" comes from /etc/pve/corosync.conf;
	// "scan" comes from the LAN TLS scan (CN extracted from the cert).
	Source string `json:"source"`
}

type discoverResult struct {
	IsHypervisor     bool                 `json:"is_hypervisor"`
	Endpoints        []DiscoveredEndpoint `json:"endpoints"`
	SuggestedGateway string               `json:"suggested_gateway,omitempty"`
}

// Discover handles GET /api/setup/discover (and the admin-side
// /api/proxmox/discover). Two complementary sources merged:
//   - corosync.conf (authoritative cluster membership; only readable on
//     PVE nodes since /etc/pve requires www-data group membership)
//   - TLS handshake on port 8006 across local subnets — works from any
//     box, and the cert CN is the Proxmox node hostname so we get names
//     for free without needing API credentials.
//
// SuggestedGateway is populated from /proc/net/route's default route on
// every install (not just hypervisors) — the LAN VM running Nimbus
// usually shares the gateway with the cluster's VMs.
//
// @Summary     Discover Proxmox endpoints on this network (admin)
// @Description Same handler is also mounted as /setup/discover in the install
// @Description wizard. Merges corosync.conf membership (authoritative on PVE
// @Description nodes) with a TLS handshake scan of port 8006 across local
// @Description subnets (cert CN gives the node hostname for free). Capped at
// @Description 1024 addresses scanned, 6s ctx.
// @Tags        nodes
// @Security    cookieAuth
// @Produce     json
// @Success     200 {object} EnvelopeOK{data=discoverResult}
// @Failure     401 {object} EnvelopeError
// @Failure     403 {object} EnvelopeError
// @Router      /proxmox/discover [get]
func (h *Setup) Discover(w http.ResponseWriter, r *http.Request) {
	result := discoverResult{Endpoints: []DiscoveredEndpoint{}}
	result.SuggestedGateway = defaultGateway()

	if _, err := os.Stat("/etc/pve"); err == nil {
		result.IsHypervisor = true
	}

	// Source 1: corosync cluster membership (instant, authoritative on PVE nodes).
	// On hypervisor, lead with localhost (IP-independent, survives network
	// changes); other cluster nodes follow.
	corosync := corosyncNodes()
	seenURL := map[string]bool{}
	addEndpoint := func(ep DiscoveredEndpoint) {
		if seenURL[ep.URL] {
			return
		}
		seenURL[ep.URL] = true
		result.Endpoints = append(result.Endpoints, ep)
	}

	if result.IsHypervisor {
		// localhost gets the local node's name from corosync if we
		// can identify which corosync entry is "us" by IP intersection.
		localIPs := localIPv4s()
		var localName string
		for _, m := range corosync {
			if containsStr(localIPs, m.IP) {
				localName = m.Name
				break
			}
		}
		addEndpoint(DiscoveredEndpoint{
			URL: "https://localhost:8006", IP: "127.0.0.1",
			NodeName: localName, Source: "localhost",
		})
		for _, m := range corosync {
			if containsStr(localIPs, m.IP) {
				continue // already covered by localhost
			}
			addEndpoint(DiscoveredEndpoint{
				URL: "https://" + m.IP + ":8006", IP: m.IP,
				NodeName: m.Name, Source: "corosync",
			})
		}
	} else {
		for _, m := range corosync {
			addEndpoint(DiscoveredEndpoint{
				URL: "https://" + m.IP + ":8006", IP: m.IP,
				NodeName: m.Name, Source: "corosync",
			})
		}
	}

	// Source 2: subnet TLS scan — finds Proxmox nodes anywhere on the
	// LAN. The cert CN is the node hostname; we extract it without
	// needing API credentials. Skips IPs already covered by corosync
	// (already-named) but still probes them when the corosync entry was
	// IP-only so we backfill names.
	ctx, cancel := context.WithTimeout(r.Context(), 6*time.Second)
	defer cancel()
	for _, hit := range scanPort8006(ctx) {
		url := "https://" + hit.IP + ":8006"
		if seenURL[url] {
			// Backfill the name if the prior entry didn't have one.
			if hit.NodeName != "" {
				for i := range result.Endpoints {
					if result.Endpoints[i].URL == url && result.Endpoints[i].NodeName == "" {
						result.Endpoints[i].NodeName = hit.NodeName
					}
				}
			}
			continue
		}
		addEndpoint(DiscoveredEndpoint{
			URL: url, IP: hit.IP, NodeName: hit.NodeName, Source: "scan",
		})
	}

	response.Success(w, result)
}

// corosyncMember pairs a node's logical name (from `name:` lines in
// /etc/pve/corosync.conf) with its corosync ring address.
type corosyncMember struct {
	Name string
	IP   string
}

// corosyncNodes parses both `name:` and `ring0_addr:` from
// /etc/pve/corosync.conf. Each `node {}` block contains both — we
// pair them by appearance order within the same block (parser is
// brace-aware).
func corosyncNodes() []corosyncMember {
	data, err := os.ReadFile("/etc/pve/corosync.conf")
	if err != nil {
		return nil
	}
	var (
		out          []corosyncMember
		depth        int
		inNodeBlock  bool
		curName      string
		curIP        string
		nodeBraceLvl int
	)
	scanner := bufio.NewScanner(strings.NewReader(string(data)))
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		// Track brace depth to know when a node block opens/closes.
		// `node {` opens; the matching `}` at the same depth closes.
		if strings.HasPrefix(line, "node ") || line == "node {" {
			inNodeBlock = true
			nodeBraceLvl = depth
			curName, curIP = "", ""
		}
		opens := strings.Count(line, "{")
		closes := strings.Count(line, "}")
		depth += opens - closes
		if inNodeBlock && depth <= nodeBraceLvl {
			// Block just closed — emit if we got both fields.
			if curIP != "" {
				out = append(out, corosyncMember{Name: curName, IP: curIP})
			}
			inNodeBlock = false
			curName, curIP = "", ""
			continue
		}
		if !inNodeBlock {
			continue
		}
		switch {
		case strings.HasPrefix(line, "name:"):
			curName = strings.TrimSpace(strings.TrimPrefix(line, "name:"))
		case strings.HasPrefix(line, "ring0_addr:"):
			addr := strings.TrimSpace(strings.TrimPrefix(line, "ring0_addr:"))
			if net.ParseIP(addr) != nil {
				curIP = addr
			}
		}
	}
	return out
}

// scanHit is one IP that responded on port 8006 with a TLS handshake.
// NodeName is populated from the leaf cert's CN (Proxmox self-signed certs
// use the node hostname as CN); empty when the cert can't be parsed or the
// CN doesn't look like a hostname.
type scanHit struct {
	IP       string
	NodeName string
}

// scanPort8006 returns the responding IPs across all local subnets. We do
// a TLS handshake (rather than a raw TCP dial) so the cert CN gives us the
// node hostname for free — no API credentials needed. InsecureSkipVerify
// is set since Proxmox ships with a self-signed cert; we're only reading
// the CN, not validating the chain.
func scanPort8006(ctx context.Context) []scanHit {
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
		found []scanHit
		sem   = make(chan struct{}, 128)
		wg    sync.WaitGroup
	)

	dialer := &net.Dialer{}
	tlsCfg := &tls.Config{InsecureSkipVerify: true} //nolint:gosec
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
			dialCtx, cancel := context.WithTimeout(ctx, 600*time.Millisecond)
			defer cancel()
			rawConn, err := dialer.DialContext(dialCtx, "tcp", ip+":8006")
			if err != nil {
				return
			}
			defer rawConn.Close() //nolint:errcheck
			tlsConn := tls.Client(rawConn, tlsCfg)
			if err := tlsConn.HandshakeContext(dialCtx); err != nil {
				// TCP responded but TLS failed — could still be Proxmox
				// behind a proxy or a non-Proxmox service. Record the
				// IP; the operator can name it by hand.
				mu.Lock()
				found = append(found, scanHit{IP: ip})
				mu.Unlock()
				return
			}
			defer tlsConn.Close() //nolint:errcheck
			name := certNodeName(tlsConn.ConnectionState().PeerCertificates)
			mu.Lock()
			found = append(found, scanHit{IP: ip, NodeName: name})
			mu.Unlock()
		}()
	}
	wg.Wait()
	return found
}

// certNodeName extracts the leaf cert's CN, which Proxmox self-signed
// certs set to the node hostname. Returns empty when the chain is empty
// or the CN isn't a plausible hostname (no dot AND no letters — guards
// against weird CN values from non-Proxmox servers like ".local" or "*").
func certNodeName(chain []*x509.Certificate) string {
	if len(chain) == 0 {
		return ""
	}
	cn := strings.TrimSpace(chain[0].Subject.CommonName)
	if cn == "" || strings.ContainsAny(cn, "*?") {
		return ""
	}
	// Strip a trailing domain suffix Proxmox sometimes adds
	// (e.g. "pve-1.local" → "pve-1"). The corosync `name:` field is
	// always the bare hostname, so strip to align with that.
	if dot := strings.Index(cn, "."); dot > 0 {
		cn = cn[:dot]
	}
	return cn
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
