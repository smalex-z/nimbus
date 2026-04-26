package handlers

import (
	"encoding/json"
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

// Settings handles admin-only configuration endpoints.
type Settings struct {
	auth         *service.AuthService
	appliers     []TunnelClientApplier
	tunnels      TunnelInfoSetter
	gpuAppliers  []GPUConfigApplier
	nimbusAppURL string // captured at construction so SaveGPU can build NimbusGPUAPI URL
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

	response.Success(w, gopherSettingsView{
		APIURL:     settings.APIURL,
		Configured: client != nil,
	})
}

// gpuSettingsView is what GET /api/settings/gpu returns. The worker token
// is sent back masked unless explicitly requested via ?reveal=1, so the
// settings page can display "[CONFIGURED]" without leaking the secret.
type gpuSettingsView struct {
	Enabled        bool   `json:"enabled"`
	BaseURL        string `json:"base_url"`
	InferenceModel string `json:"inference_model"`
	Configured     bool   `json:"configured"`
	WorkerToken    string `json:"worker_token,omitempty"` // populated only on regenerate
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
	var bootstrapCfg provision.GPUBootstrapConfig
	if settings.Enabled && settings.BaseURL != "" {
		bootstrapCfg = provision.GPUBootstrapConfig{
			BaseURL:        settings.BaseURL,
			NimbusGPUAPI:   strings.TrimRight(s.nimbusAppURL, "/") + "/api/gpu",
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
	})
}

// RegenerateGPUWorkerToken handles POST /api/settings/gpu/worker-token/regenerate
// (admin only). Returns the freshly minted token in the response — admins
// must capture it for the GX10 install command. Subsequent GETs of the
// settings page will not return it.
func (s *Settings) RegenerateGPUWorkerToken(w http.ResponseWriter, _ *http.Request) {
	tok, err := s.auth.RegenerateGPUWorkerToken()
	if err != nil {
		response.InternalError(w, "failed to regenerate worker token")
		return
	}
	settings, err := s.auth.GetGPUSettings()
	if err != nil {
		response.InternalError(w, "regenerated, but failed to reload: "+err.Error())
		return
	}
	response.Success(w, gpuSettingsView{
		Enabled:        settings.Enabled,
		BaseURL:        settings.BaseURL,
		InferenceModel: settings.InferenceModel,
		Configured:     true,
		WorkerToken:    tok,
	})
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
