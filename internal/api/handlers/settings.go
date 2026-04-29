package handlers

import (
	"context"
	"encoding/json"
	"net"
	"net/http"
	"strings"
	"time"

	"nimbus/internal/api/response"
	"nimbus/internal/db"
	"nimbus/internal/provision"
	"nimbus/internal/service"
	"nimbus/internal/tunnel"
)

// TunnelClientApplier is implemented by anything that holds a *tunnel.Client
// and wants to be told when settings change. provision.Service satisfies this
// via SetTunnelClient. The settings handler calls each registered applier
// after a save so the new client takes effect without restart.
type TunnelClientApplier interface {
	SetTunnelClient(*tunnel.Client)
}

// TunnelInfoSetter lets the admin tunnels handler refresh its cached client +
// host when settings change. handlers.Tunnels satisfies this.
type TunnelInfoSetter interface {
	SetClient(c *tunnel.Client, apiURL string)
}

// GPUConfigApplier is implemented by anything that holds the live
// provision.GPUBootstrapConfig and needs to be re-pushed when admins edit
// the GPU settings. provision.Service satisfies via SetGPUBootstrapConfig.
type GPUConfigApplier interface {
	SetGPUBootstrapConfig(provision.GPUBootstrapConfig)
}

// NetworkApplier is implemented by anything that needs to be told the live
// gateway IP and cloud-init prefix length after a Settings → Network save.
// provision.Service satisfies via SetGatewayIP / SetPrefixLen.
type NetworkApplier interface {
	SetGatewayIP(string)
	SetPrefixLen(int)
}

// NetworkOps performs the disruptive renumber + force-gateway batch ops.
// provision.Service satisfies this; the handler keeps it as an interface so
// tests can inject a fake without booting the whole orchestrator.
type NetworkOps interface {
	RenumberAllVMs(ctx context.Context, gateway, poolStart, poolEnd string) (provision.NetworkOpReport, error)
	ForceGatewayUpdate(ctx context.Context, gateway string) (provision.NetworkOpReport, error)
}

// PoolReseeder is what the network handler uses to converge the IP pool table
// to the new range after a save. *ippool.Pool satisfies it.
type PoolReseeder interface {
	Reseed(ctx context.Context, start, end string) (added, removedFree, stranded int, err error)
}

// SelfBootstrap is implemented by selftunnel.Service. SaveGopher fires
// Start in a goroutine after a successful credential save, and the
// /self-bootstrap status/start endpoints proxy to it.
//
// SetGopherClient takes the concrete *tunnel.Client to avoid an interface
// re-declaration that selftunnel.Service wouldn't satisfy by name.
type SelfBootstrap interface {
	Start(ctx context.Context) error
	Status() (db.GopherSettings, error)
	SetGopherClient(c *tunnel.Client)
}

// Settings handles admin-only configuration endpoints.
type Settings struct {
	auth            *service.AuthService
	appliers        []TunnelClientApplier
	tunnels         TunnelInfoSetter
	gpuAppliers     []GPUConfigApplier
	nimbusAppURL    string // captured at construction so SaveGPU can build NimbusGPUAPI URL
	selfBootstrap   SelfBootstrap
	networkAppliers []NetworkApplier
	networkOps      NetworkOps
	pool            PoolReseeder
}

func NewSettings(auth *service.AuthService) *Settings {
	return &Settings{auth: auth}
}

// WithTunnelAppliers registers components that should be notified when Gopher
// settings change. Variadic so callers can pass any combination.
func (s *Settings) WithTunnelAppliers(a ...TunnelClientApplier) *Settings {
	s.appliers = append(s.appliers, a...)
	return s
}

// WithTunnelInfoSetter wires the admin tunnels handler so its cached
// {client, apiURL} stay in sync with what's stored.
func (s *Settings) WithTunnelInfoSetter(t TunnelInfoSetter) *Settings {
	s.tunnels = t
	return s
}

// WithGPUAppliers registers components that should be notified when GPU
// settings change. Same shape as WithTunnelAppliers.
func (s *Settings) WithGPUAppliers(a ...GPUConfigApplier) *Settings {
	s.gpuAppliers = append(s.gpuAppliers, a...)
	return s
}

// WithNimbusAppURL captures the configured AppURL so SaveGPU can compose
// the per-VM NIMBUS_GPU_API var without threading config through every
// caller.
func (s *Settings) WithNimbusAppURL(u string) *Settings {
	s.nimbusAppURL = u
	return s
}

// WithSelfBootstrap wires the selftunnel.Service so SaveGopher can kick
// off the self-bootstrap automatically + the modal endpoints have
// something to talk to.
func (s *Settings) WithSelfBootstrap(b SelfBootstrap) *Settings {
	s.selfBootstrap = b
	return s
}

// WithNetworkAppliers registers components that need to be told the live
// gateway IP after a network settings save (provision.Service mainly).
func (s *Settings) WithNetworkAppliers(a ...NetworkApplier) *Settings {
	s.networkAppliers = append(s.networkAppliers, a...)
	return s
}

// WithNetworkOps wires the renumber / force-gateway batch operator.
// provision.Service satisfies it.
func (s *Settings) WithNetworkOps(o NetworkOps) *Settings {
	s.networkOps = o
	return s
}

// WithPoolReseeder wires the IP pool so SaveNetwork can converge the
// allocation table to the new range. *ippool.Pool satisfies it.
func (s *Settings) WithPoolReseeder(p PoolReseeder) *Settings {
	s.pool = p
	return s
}

type oauthSettingsView struct {
	GitHubClientID   string `json:"github_client_id"`
	GoogleClientID   string `json:"google_client_id"`
	GitHubConfigured bool   `json:"github_configured"`
	GoogleConfigured bool   `json:"google_configured"`
}

// GetOAuth handles GET /api/settings/oauth. Secrets are never returned.
func (s *Settings) GetOAuth(w http.ResponseWriter, r *http.Request) {
	settings, err := s.auth.GetOAuthSettings()
	if err != nil {
		response.InternalError(w, "failed to load OAuth settings")
		return
	}
	response.Success(w, oauthSettingsView{
		GitHubClientID:   settings.GitHubClientID,
		GoogleClientID:   settings.GoogleClientID,
		GitHubConfigured: settings.GitHubClientID != "" && settings.GitHubClientSecret != "",
		GoogleConfigured: settings.GoogleClientID != "" && settings.GoogleClientSecret != "",
	})
}

type saveOAuthRequest struct {
	GitHubClientID     string `json:"github_client_id"`
	GitHubClientSecret string `json:"github_client_secret"`
	GoogleClientID     string `json:"google_client_id"`
	GoogleClientSecret string `json:"google_client_secret"`
}

// SaveOAuth handles PUT /api/settings/oauth.
func (s *Settings) SaveOAuth(w http.ResponseWriter, r *http.Request) {
	var req saveOAuthRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		response.BadRequest(w, "invalid JSON")
		return
	}
	if err := s.auth.SaveOAuthSettings(db.OAuthSettings{
		GitHubClientID:     req.GitHubClientID,
		GitHubClientSecret: req.GitHubClientSecret,
		GoogleClientID:     req.GoogleClientID,
		GoogleClientSecret: req.GoogleClientSecret,
	}); err != nil {
		response.InternalError(w, "failed to save OAuth settings")
		return
	}
	response.Success(w, map[string]string{"message": "OAuth settings saved"})
}

type accessCodeView struct {
	AccessCode string `json:"access_code"`
	Version    int    `json:"version"`
}

// GetAccessCode handles GET /api/settings/access-code (admin only).
// Returns the raw 8-digit code so the admin UI can reveal it on demand.
func (s *Settings) GetAccessCode(w http.ResponseWriter, r *http.Request) {
	settings, err := s.auth.GetOAuthSettings()
	if err != nil {
		response.InternalError(w, "failed to load access code")
		return
	}
	response.Success(w, accessCodeView{
		AccessCode: settings.AccessCode,
		Version:    settings.AccessCodeVersion,
	})
}

type authorizedDomainsView struct {
	Domains []string `json:"domains"`
}

type saveAuthorizedDomainsRequest struct {
	Domains []string `json:"domains"`
}

// GetAuthorizedGoogleDomains handles GET /api/settings/google-domains (admin only).
func (s *Settings) GetAuthorizedGoogleDomains(w http.ResponseWriter, r *http.Request) {
	settings, err := s.auth.GetOAuthSettings()
	if err != nil {
		response.InternalError(w, "failed to load authorized domains")
		return
	}
	domains := []string{}
	for _, d := range strings.Split(settings.AuthorizedGoogleDomains, ",") {
		d = strings.TrimSpace(d)
		if d != "" {
			domains = append(domains, d)
		}
	}
	response.Success(w, authorizedDomainsView{Domains: domains})
}

// SaveAuthorizedGoogleDomains handles PUT /api/settings/google-domains (admin only).
func (s *Settings) SaveAuthorizedGoogleDomains(w http.ResponseWriter, r *http.Request) {
	var req saveAuthorizedDomainsRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		response.BadRequest(w, "invalid JSON")
		return
	}
	if err := s.auth.SaveAuthorizedGoogleDomains(req.Domains); err != nil {
		response.InternalError(w, "failed to save authorized domains")
		return
	}
	s.GetAuthorizedGoogleDomains(w, r)
}

type authorizedOrgsView struct {
	Orgs []string `json:"orgs"`
}

type saveAuthorizedOrgsRequest struct {
	Orgs []string `json:"orgs"`
}

// GetAuthorizedGitHubOrgs handles GET /api/settings/github-orgs (admin only).
func (s *Settings) GetAuthorizedGitHubOrgs(w http.ResponseWriter, r *http.Request) {
	settings, err := s.auth.GetOAuthSettings()
	if err != nil {
		response.InternalError(w, "failed to load authorized orgs")
		return
	}
	orgs := []string{}
	for _, o := range strings.Split(settings.AuthorizedGitHubOrgs, ",") {
		o = strings.TrimSpace(o)
		if o != "" {
			orgs = append(orgs, o)
		}
	}
	response.Success(w, authorizedOrgsView{Orgs: orgs})
}

// SaveAuthorizedGitHubOrgs handles PUT /api/settings/github-orgs (admin only).
func (s *Settings) SaveAuthorizedGitHubOrgs(w http.ResponseWriter, r *http.Request) {
	var req saveAuthorizedOrgsRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		response.BadRequest(w, "invalid JSON")
		return
	}
	if err := s.auth.SaveAuthorizedGitHubOrgs(req.Orgs); err != nil {
		response.InternalError(w, "failed to save authorized orgs")
		return
	}
	s.GetAuthorizedGitHubOrgs(w, r)
}

type gopherSettingsView struct {
	APIURL     string `json:"api_url"`
	Configured bool   `json:"configured"`
}

// GetGopher handles GET /api/settings/gopher (admin only). The API key is
// never returned; the SPA only needs to know whether it's set.
func (s *Settings) GetGopher(w http.ResponseWriter, _ *http.Request) {
	settings, err := s.auth.GetGopherSettings()
	if err != nil {
		response.InternalError(w, "failed to load Gopher settings")
		return
	}
	response.Success(w, gopherSettingsView{
		APIURL:     settings.APIURL,
		Configured: settings.APIURL != "" && settings.APIKey != "",
	})
}

type saveGopherRequest struct {
	APIURL string `json:"api_url"`
	APIKey string `json:"api_key"`
}

// SaveGopher handles PUT /api/settings/gopher (admin only). Persists the
// values, then rebuilds the live tunnel.Client and pushes it to every
// registered applier (so provision flow + admin /tunnels endpoint pick up
// the new credentials with no restart). On clear (both fields blank),
// passes nil to disable.
func (s *Settings) SaveGopher(w http.ResponseWriter, r *http.Request) {
	var req saveGopherRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		response.BadRequest(w, "invalid JSON")
		return
	}
	url := strings.TrimSpace(req.APIURL)
	key := strings.TrimSpace(req.APIKey)

	if err := s.auth.SaveGopherSettings(db.GopherSettings{APIURL: url, APIKey: key}); err != nil {
		response.InternalError(w, "failed to save Gopher settings")
		return
	}

	// Re-read the merged settings (Save preserves empty fields) so the live
	// client we build matches what's persisted.
	settings, err := s.auth.GetGopherSettings()
	if err != nil {
		response.InternalError(w, "saved, but failed to reload: "+err.Error())
		return
	}

	var client *tunnel.Client
	if settings.APIURL != "" && settings.APIKey != "" {
		client, err = tunnel.New(settings.APIURL, settings.APIKey, 15*time.Second)
		if err != nil {
			response.BadRequest(w, "invalid Gopher settings: "+err.Error())
			return
		}
	}
	for _, a := range s.appliers {
		a.SetTunnelClient(client)
	}
	if s.tunnels != nil {
		s.tunnels.SetClient(client, settings.APIURL)
	}
	if s.selfBootstrap != nil {
		s.selfBootstrap.SetGopherClient(client)
	}

	// Kick off the self-bootstrap when we now have a usable Gopher client.
	// Errors here are non-fatal for the save itself — the modal will poll
	// /self-bootstrap and surface whatever happens.
	if client != nil && s.selfBootstrap != nil {
		if err := s.selfBootstrap.Start(r.Context()); err != nil {
			// Log via the response — no separate logger threaded in here.
			// Non-fatal: the save itself still succeeded.
			_ = err
		}
	}

	response.Success(w, gopherSettingsView{
		APIURL:     settings.APIURL,
		Configured: client != nil,
	})
}

// gpuSettingsView is what GET /api/settings/gpu returns. The worker token
// is never sent back to clients — operators don't see it; pairing produces
// a fresh one as part of /api/gpu/register.
type gpuSettingsView struct {
	Enabled        bool   `json:"enabled"`
	BaseURL        string `json:"base_url"`
	InferenceModel string `json:"inference_model"`
	Configured     bool   `json:"configured"`
	GX10Hostname   string `json:"gx10_hostname,omitempty"` // self-reported at pairing time
}

// GetGPU handles GET /api/settings/gpu (admin only).
func (s *Settings) GetGPU(w http.ResponseWriter, _ *http.Request) {
	settings, err := s.auth.GetGPUSettings()
	if err != nil {
		response.InternalError(w, "failed to load GPU settings")
		return
	}
	response.Success(w, gpuSettingsView{
		Enabled:        settings.Enabled,
		BaseURL:        settings.BaseURL,
		InferenceModel: settings.InferenceModel,
		Configured:     settings.WorkerToken != "" && settings.BaseURL != "",
		GX10Hostname:   settings.GX10Hostname,
	})
}

type saveGPURequest struct {
	Enabled        bool   `json:"enabled"`
	BaseURL        string `json:"base_url"`
	InferenceModel string `json:"inference_model"`
}

// SaveGPU handles PUT /api/settings/gpu (admin only).
func (s *Settings) SaveGPU(w http.ResponseWriter, r *http.Request) {
	var req saveGPURequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		response.BadRequest(w, "invalid JSON")
		return
	}
	if err := s.auth.SaveGPUSettings(db.GPUSettings{
		Enabled:        req.Enabled,
		BaseURL:        strings.TrimSpace(req.BaseURL),
		InferenceModel: strings.TrimSpace(req.InferenceModel),
	}); err != nil {
		response.InternalError(w, "failed to save GPU settings")
		return
	}
	settings, err := s.auth.GetGPUSettings()
	if err != nil {
		response.InternalError(w, "saved, but failed to reload: "+err.Error())
		return
	}

	// Push the fresh config to anything that holds a live copy
	// (provision.Service for cloud-init env injection). Disabled or empty
	// BaseURL means push a zero config — provision will then skip the
	// GPU bootstrap step on subsequent VMs.
	//
	// nimbusGPUAPI: prefer the request host (the admin's browser just
	// proved it's reachable) over s.nimbusAppURL — if APP_URL is misconfigured
	// (defaults to localhost:5173), this prevents poisoning the bootstrap
	// config with a URL guests can't reach.
	var bootstrapCfg provision.GPUBootstrapConfig
	if settings.Enabled && settings.BaseURL != "" {
		nimbusGPUAPI := strings.TrimRight(s.nimbusAppURL, "/") + "/api/gpu"
		if r.Host != "" && !looksLikeLocalhost(r.Host) {
			scheme := "http"
			if r.TLS != nil || r.Header.Get("X-Forwarded-Proto") == "https" {
				scheme = "https"
			}
			nimbusGPUAPI = scheme + "://" + r.Host + "/api/gpu"
		}
		bootstrapCfg = provision.GPUBootstrapConfig{
			BaseURL:        settings.BaseURL,
			NimbusGPUAPI:   nimbusGPUAPI,
			InferenceModel: settings.InferenceModel,
		}
	}
	for _, a := range s.gpuAppliers {
		a.SetGPUBootstrapConfig(bootstrapCfg)
	}

	response.Success(w, gpuSettingsView{
		Enabled:        settings.Enabled,
		BaseURL:        settings.BaseURL,
		InferenceModel: settings.InferenceModel,
		Configured:     settings.WorkerToken != "" && settings.BaseURL != "",
		GX10Hostname:   settings.GX10Hostname,
	})
}

// looksLikeLocalhost reports whether the bare host (no scheme, may include
// port) refers to a loopback address — those are unreachable from VMs and
// from the GX10, so callers should fall back to AppURL or refuse to bake
// the host into per-VM bootstraps. Mirrors the URL-shaped helper in
// provision/gpu_bootstrap.go but takes a "host:port" instead of a URL.
func looksLikeLocalhost(host string) bool {
	if host == "" {
		return true
	}
	h := strings.ToLower(host)
	if i := strings.LastIndex(h, ":"); i >= 0 && !strings.Contains(h[:i], ":") {
		h = h[:i]
	}
	return h == "localhost" ||
		strings.HasPrefix(h, "127.") ||
		h == "0.0.0.0" ||
		h == "::1" ||
		h == "[::1]"
}

// (Worker-token regeneration is gone — operators don't see the token.
// Re-pairing via Settings → Add GX10 produces a fresh worker token as
// part of the handshake.)

type selfBootstrapStatusView struct {
	State     string `json:"state"`
	Error     string `json:"error,omitempty"`
	TunnelURL string `json:"tunnel_url,omitempty"`
}

// SelfBootstrapStatus handles GET /api/settings/gopher/self-bootstrap —
// the Settings modal polls this to render the phase indicator.
func (s *Settings) SelfBootstrapStatus(w http.ResponseWriter, _ *http.Request) {
	if s.selfBootstrap == nil {
		response.Success(w, selfBootstrapStatusView{State: ""})
		return
	}
	state, err := s.selfBootstrap.Status()
	if err != nil {
		response.InternalError(w, "failed to load bootstrap status")
		return
	}
	response.Success(w, selfBootstrapStatusView{
		State:     state.CloudBootstrapState,
		Error:     state.CloudBootstrapError,
		TunnelURL: state.CloudTunnelURL,
	})
}

// SelfBootstrapStart handles POST /api/settings/gopher/self-bootstrap —
// retry hook for the modal's "Try again" button after a failure. SaveGopher
// invokes this path automatically; this endpoint exists for explicit retry.
func (s *Settings) SelfBootstrapStart(w http.ResponseWriter, r *http.Request) {
	if s.selfBootstrap == nil {
		response.BadRequest(w, "self-bootstrap requires Gopher to be configured")
		return
	}
	if err := s.selfBootstrap.Start(r.Context()); err != nil {
		response.BadRequest(w, err.Error())
		return
	}
	response.Success(w, map[string]string{"state": "started"})
}

// networkSettingsView is what GET / PUT /api/settings/network return.
type networkSettingsView struct {
	IPPoolStart string `json:"ip_pool_start"`
	IPPoolEnd   string `json:"ip_pool_end"`
	GatewayIP   string `json:"gateway_ip"`
	PrefixLen   int    `json:"prefix_len"`
}

// GetNetwork handles GET /api/settings/network (admin only). Returns the live
// IP pool range, gateway, and cloud-init prefix length. Used by the Settings
// → Network panel and as the canonical source for the renumber /
// force-gateway confirmation modals.
func (s *Settings) GetNetwork(w http.ResponseWriter, _ *http.Request) {
	settings, err := s.auth.GetNetworkSettings()
	if err != nil {
		response.InternalError(w, "failed to load network settings")
		return
	}
	response.Success(w, networkSettingsView{
		IPPoolStart: settings.IPPoolStart,
		IPPoolEnd:   settings.IPPoolEnd,
		GatewayIP:   settings.GatewayIP,
		PrefixLen:   settings.PrefixLen,
	})
}

type saveNetworkRequest struct {
	IPPoolStart string `json:"ip_pool_start"`
	IPPoolEnd   string `json:"ip_pool_end"`
	GatewayIP   string `json:"gateway_ip"`
	PrefixLen   int    `json:"prefix_len"`
}

// SaveNetwork handles PUT /api/settings/network (admin only). Persists the
// supplied values, then reseeds the IP pool to converge to the new range and
// pushes the live gateway + prefix to every registered NetworkApplier.
// Existing VMs are NOT touched — that requires the explicit RenumberVMs /
// ForceGatewayUpdate endpoints.
func (s *Settings) SaveNetwork(w http.ResponseWriter, r *http.Request) {
	var req saveNetworkRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		response.BadRequest(w, "invalid JSON")
		return
	}
	start := strings.TrimSpace(req.IPPoolStart)
	end := strings.TrimSpace(req.IPPoolEnd)
	gw := strings.TrimSpace(req.GatewayIP)

	for label, v := range map[string]string{"ip_pool_start": start, "ip_pool_end": end, "gateway_ip": gw} {
		if v == "" {
			continue // empty = preserve existing
		}
		if ip := net.ParseIP(v); ip == nil || ip.To4() == nil {
			response.BadRequest(w, label+" is not a valid IPv4 address")
			return
		}
	}
	// Prefix: 0 = preserve existing (caller didn't include the field).
	// Otherwise must be a sane IPv4 mask length. /1..32 covers everything;
	// callers that want /0 should be loudly told no.
	if req.PrefixLen != 0 && (req.PrefixLen < 1 || req.PrefixLen > 32) {
		response.BadRequest(w, "prefix_len must be between 1 and 32")
		return
	}

	if err := s.auth.SaveNetworkSettings(db.NetworkSettings{
		IPPoolStart: start,
		IPPoolEnd:   end,
		GatewayIP:   gw,
		PrefixLen:   req.PrefixLen,
	}); err != nil {
		response.InternalError(w, "failed to save network settings")
		return
	}

	settings, err := s.auth.GetNetworkSettings()
	if err != nil {
		response.InternalError(w, "saved, but failed to reload: "+err.Error())
		return
	}

	if s.pool != nil && settings.IPPoolStart != "" && settings.IPPoolEnd != "" {
		if _, _, _, err := s.pool.Reseed(r.Context(), settings.IPPoolStart, settings.IPPoolEnd); err != nil {
			response.BadRequest(w, "saved, but pool reseed failed: "+err.Error())
			return
		}
	}
	for _, a := range s.networkAppliers {
		a.SetGatewayIP(settings.GatewayIP)
		a.SetPrefixLen(settings.PrefixLen)
	}

	response.Success(w, networkSettingsView{
		IPPoolStart: settings.IPPoolStart,
		IPPoolEnd:   settings.IPPoolEnd,
		GatewayIP:   settings.GatewayIP,
		PrefixLen:   settings.PrefixLen,
	})
}

// networkOpResponse is the JSON shape returned by the renumber / force-gateway
// endpoints — Updated count + per-VM failures the UI can surface.
type networkOpResponse struct {
	Updated  int                    `json:"updated"`
	Failures []networkOpFailureView `json:"failures"`
}

type networkOpFailureView struct {
	VMRowID  uint   `json:"vm_row_id"`
	VMID     int    `json:"vmid"`
	Hostname string `json:"hostname"`
	Error    string `json:"error"`
}

func toNetworkOpResponse(rep provision.NetworkOpReport) networkOpResponse {
	out := networkOpResponse{
		Updated:  rep.Updated,
		Failures: make([]networkOpFailureView, 0, len(rep.Failures)),
	}
	for _, f := range rep.Failures {
		out.Failures = append(out.Failures, networkOpFailureView{
			VMRowID:  f.VMRowID,
			VMID:     f.VMID,
			Hostname: f.Hostname,
			Error:    f.Err,
		})
	}
	return out
}

// ForceGatewayUpdate handles POST /api/settings/network/force-gateway-update
// (admin only). Pushes the currently-saved gateway to every managed VM via
// `qm set --ipconfig0` and reboots running VMs so the change takes effect.
// Disruptive — every running VM bounces.
func (s *Settings) ForceGatewayUpdate(w http.ResponseWriter, r *http.Request) {
	if s.networkOps == nil {
		response.InternalError(w, "network ops not wired")
		return
	}
	settings, err := s.auth.GetNetworkSettings()
	if err != nil {
		response.InternalError(w, "failed to load network settings")
		return
	}
	if settings.GatewayIP == "" {
		response.BadRequest(w, "gateway_ip is not set; save network settings first")
		return
	}
	rep, err := s.networkOps.ForceGatewayUpdate(r.Context(), settings.GatewayIP)
	if err != nil {
		response.InternalError(w, err.Error())
		return
	}
	response.Success(w, toNetworkOpResponse(rep))
}

// RenumberVMs handles POST /api/settings/network/renumber-vms (admin only).
// Reserves a fresh IP from the saved pool for every managed VM, updates each
// VM's cloud-init, reboots it, and releases the old IP. Disruptive.
//
// Refuses with 400 if the pool has fewer free addresses than the number of
// VMs to renumber — operator must widen the pool first.
func (s *Settings) RenumberVMs(w http.ResponseWriter, r *http.Request) {
	if s.networkOps == nil {
		response.InternalError(w, "network ops not wired")
		return
	}
	settings, err := s.auth.GetNetworkSettings()
	if err != nil {
		response.InternalError(w, "failed to load network settings")
		return
	}
	if settings.GatewayIP == "" || settings.IPPoolStart == "" || settings.IPPoolEnd == "" {
		response.BadRequest(w, "network settings (gateway_ip + ip_pool_start + ip_pool_end) must all be saved before renumbering")
		return
	}
	rep, err := s.networkOps.RenumberAllVMs(r.Context(), settings.GatewayIP, settings.IPPoolStart, settings.IPPoolEnd)
	if err != nil {
		response.BadRequest(w, err.Error())
		return
	}
	response.Success(w, toNetworkOpResponse(rep))
}

// RegenerateAccessCode handles POST /api/settings/access-code/regenerate (admin only).
// Issues a fresh code and bumps the version, invalidating every non-admin
// user's prior verification.
func (s *Settings) RegenerateAccessCode(w http.ResponseWriter, r *http.Request) {
	settings, err := s.auth.RegenerateAccessCode()
	if err != nil {
		response.InternalError(w, "failed to regenerate access code")
		return
	}
	response.Success(w, accessCodeView{
		AccessCode: settings.AccessCode,
		Version:    settings.AccessCodeVersion,
	})
}
