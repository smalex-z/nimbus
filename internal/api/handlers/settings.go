package handlers

import (
	"encoding/json"
	"net/http"
	"strings"
	"time"

	"nimbus/internal/api/response"
	"nimbus/internal/db"
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

// Settings handles admin-only configuration endpoints.
type Settings struct {
	auth     *service.AuthService
	appliers []TunnelClientApplier
	tunnels  TunnelInfoSetter
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

type gopherSettingsView struct {
	APIURL     string `json:"api_url"`
	Configured bool   `json:"configured"`
}

// GetGopher handles GET /api/settings/gopher. The API key is never returned;
// the SPA only needs to know whether it's set.
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

// SaveGopher handles PUT /api/settings/gopher. Persists the values, then
// rebuilds the live tunnel.Client and pushes it to every registered applier
// (so provision flow + admin /tunnels endpoint pick up the new credentials
// with no restart). On clear (both fields blank), passes nil to disable.
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
