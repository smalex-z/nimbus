package handlers

import (
	"encoding/json"
	"net/http"
	"strings"

	"nimbus/internal/api/response"
	"nimbus/internal/db"
	"nimbus/internal/service"
)

// Settings handles admin-only configuration endpoints.
type Settings struct {
	auth *service.AuthService
}

func NewSettings(auth *service.AuthService) *Settings {
	return &Settings{auth: auth}
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
