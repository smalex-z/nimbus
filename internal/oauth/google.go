package oauth

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strings"
)

// Google implements Provider for Google OAuth 2.0 / OpenID Connect.
type Google struct {
	ClientID     string
	ClientSecret string
	RedirectURI  string // must match exactly what is registered in Google Cloud Console
}

func (g *Google) AuthURL(state string) string {
	v := url.Values{}
	v.Set("client_id", g.ClientID)
	v.Set("redirect_uri", g.RedirectURI)
	v.Set("response_type", "code")
	v.Set("scope", "openid email profile")
	v.Set("state", state)
	v.Set("access_type", "online")
	// Always present the account chooser so a user who picked a blocked
	// account on a previous attempt can pick a different one. Without this,
	// Google silently re-uses the active session and re-issues the same
	// (still-unauthorized) email.
	v.Set("prompt", "select_account")
	return "https://accounts.google.com/o/oauth2/v2/auth?" + v.Encode()
}

func (g *Google) Exchange(ctx context.Context, code string) (*UserInfo, error) {
	token, err := g.exchangeCode(ctx, code)
	if err != nil {
		return nil, err
	}

	info, err := g.getUserInfo(ctx, token)
	if err != nil {
		return nil, err
	}

	// Derive a display login from the email prefix (Google has no username).
	login := info.Email
	if idx := strings.Index(login, "@"); idx != -1 {
		login = login[:idx]
	}

	return &UserInfo{
		ProviderID: info.Sub,
		Login:      login,
		Name:       info.Name,
		Email:      info.Email,
	}, nil
}

// --- internal helpers -------------------------------------------------------

type googleTokenResponse struct {
	AccessToken string `json:"access_token"`
	Error       string `json:"error"`
}

type googleUserInfo struct {
	Sub   string `json:"sub"`
	Name  string `json:"name"`
	Email string `json:"email"`
}

func (g *Google) exchangeCode(ctx context.Context, code string) (string, error) {
	body := url.Values{
		"client_id":     {g.ClientID},
		"client_secret": {g.ClientSecret},
		"code":          {code},
		"grant_type":    {"authorization_code"},
		"redirect_uri":  {g.RedirectURI},
	}
	req, _ := http.NewRequestWithContext(ctx, http.MethodPost,
		"https://oauth2.googleapis.com/token",
		strings.NewReader(body.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Accept", "application/json")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return "", fmt.Errorf("google token request: %w", err)
	}
	defer resp.Body.Close() //nolint:errcheck

	var t googleTokenResponse
	if err := json.NewDecoder(resp.Body).Decode(&t); err != nil {
		return "", fmt.Errorf("google token decode: %w", err)
	}
	if t.Error != "" {
		return "", fmt.Errorf("google token error: %s", t.Error)
	}
	return t.AccessToken, nil
}

func (g *Google) getUserInfo(ctx context.Context, token string) (*googleUserInfo, error) {
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet,
		"https://openidconnect.googleapis.com/v1/userinfo", nil)
	req.Header.Set("Authorization", "Bearer "+token)

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("google userinfo request: %w", err)
	}
	defer resp.Body.Close() //nolint:errcheck

	var info googleUserInfo
	if err := json.NewDecoder(resp.Body).Decode(&info); err != nil {
		return nil, fmt.Errorf("google userinfo decode: %w", err)
	}
	if info.Email == "" {
		return nil, fmt.Errorf("google userinfo: no email returned")
	}
	return &info, nil
}
