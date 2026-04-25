package oauth

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strings"
)

// GitHub implements Provider for GitHub OAuth.
type GitHub struct {
	ClientID     string
	ClientSecret string
}

func (g *GitHub) AuthURL(state string) string {
	v := url.Values{}
	v.Set("client_id", g.ClientID)
	v.Set("state", state)
	v.Set("scope", "user:email")
	return "https://github.com/login/oauth/authorize?" + v.Encode()
}

func (g *GitHub) Exchange(ctx context.Context, code string) (*UserInfo, error) {
	token, err := g.exchangeCode(ctx, code)
	if err != nil {
		return nil, err
	}

	user, err := g.getUser(ctx, token)
	if err != nil {
		return nil, err
	}

	if user.Email == "" {
		user.Email, err = g.getPrimaryEmail(ctx, token)
		if err != nil {
			return nil, err
		}
	}

	name := user.Name
	if name == "" {
		name = user.Login
	}

	return &UserInfo{
		ProviderID: fmt.Sprintf("%d", user.ID),
		Login:      user.Login,
		Name:       name,
		Email:      user.Email,
	}, nil
}

// --- internal helpers -------------------------------------------------------

type ghTokenResponse struct {
	AccessToken string `json:"access_token"`
	Error       string `json:"error"`
}

type ghUser struct {
	ID    int64  `json:"id"`
	Login string `json:"login"`
	Name  string `json:"name"`
	Email string `json:"email"`
}

type ghEmail struct {
	Email    string `json:"email"`
	Primary  bool   `json:"primary"`
	Verified bool   `json:"verified"`
}

func (g *GitHub) exchangeCode(ctx context.Context, code string) (string, error) {
	body := url.Values{
		"client_id":     {g.ClientID},
		"client_secret": {g.ClientSecret},
		"code":          {code},
	}
	req, _ := http.NewRequestWithContext(ctx, http.MethodPost,
		"https://github.com/login/oauth/access_token",
		strings.NewReader(body.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Accept", "application/json")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return "", fmt.Errorf("github token request: %w", err)
	}
	defer resp.Body.Close()

	var t ghTokenResponse
	if err := json.NewDecoder(resp.Body).Decode(&t); err != nil {
		return "", fmt.Errorf("github token decode: %w", err)
	}
	if t.Error != "" {
		return "", fmt.Errorf("github token error: %s", t.Error)
	}
	return t.AccessToken, nil
}

func (g *GitHub) getUser(ctx context.Context, token string) (*ghUser, error) {
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, "https://api.github.com/user", nil)
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Accept", "application/vnd.github+json")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("github user request: %w", err)
	}
	defer resp.Body.Close()

	var u ghUser
	if err := json.NewDecoder(resp.Body).Decode(&u); err != nil {
		return nil, fmt.Errorf("github user decode: %w", err)
	}
	return &u, nil
}

func (g *GitHub) getPrimaryEmail(ctx context.Context, token string) (string, error) {
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, "https://api.github.com/user/emails", nil)
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Accept", "application/vnd.github+json")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return "", fmt.Errorf("github emails request: %w", err)
	}
	defer resp.Body.Close()

	var emails []ghEmail
	if err := json.NewDecoder(resp.Body).Decode(&emails); err != nil {
		return "", fmt.Errorf("github emails decode: %w", err)
	}
	for _, e := range emails {
		if e.Primary && e.Verified {
			return e.Email, nil
		}
	}
	return "", fmt.Errorf("no verified primary email on this GitHub account")
}
