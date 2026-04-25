package handlers

import (
	"encoding/json"
	"net/http"

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
