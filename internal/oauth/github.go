package oauth

import (
	"bytes"
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
	// read:org gives us /user/orgs visibility into private memberships too —
	// required for the authorized-orgs bypass to work for users whose org
	// membership is set to private.
	v.Set("scope", "user:email read:org")
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

	// Org list is best-effort: a transient GitHub API hiccup shouldn't block
	// sign-in. The authorized-orgs gate (when enabled) will then deny on an
	// empty org snapshot, which is the safe default.
	orgs, _ := g.getUserOrgs(ctx, token)

	name := user.Name
	if name == "" {
		name = user.Login
	}

	return &UserInfo{
		ProviderID: fmt.Sprintf("%d", user.ID),
		Login:      user.Login,
		Name:       name,
		Email:      user.Email,
		Orgs:       orgs,
		Token:      token,
	}, nil
}

// RevokeToken invalidates an OAuth access token previously issued to this
// app. Used after a rejected callback (e.g. unauthorized org) so that the
// next "Continue with GitHub" attempt presents the consent screen — which
// surfaces a "Sign in with a different account" link, the only way to switch
// GitHub identities without leaving the flow. 204 = revoked, 422 = already
// invalid; both are success.
func (g *GitHub) RevokeToken(ctx context.Context, token string) error {
	if token == "" {
		return nil
	}
	body, _ := json.Marshal(map[string]string{"access_token": token})
	req, _ := http.NewRequestWithContext(ctx, http.MethodDelete,
		fmt.Sprintf("https://api.github.com/applications/%s/token", g.ClientID),
		bytes.NewReader(body))
	req.SetBasicAuth(g.ClientID, g.ClientSecret)
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("Content-Type", "application/json")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return fmt.Errorf("github revoke request: %w", err)
	}
	defer resp.Body.Close() //nolint:errcheck
	if resp.StatusCode != http.StatusNoContent && resp.StatusCode != http.StatusUnprocessableEntity {
		return fmt.Errorf("github revoke status: %d", resp.StatusCode)
	}
	return nil
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
	defer resp.Body.Close() //nolint:errcheck

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
	defer resp.Body.Close() //nolint:errcheck

	var u ghUser
	if err := json.NewDecoder(resp.Body).Decode(&u); err != nil {
		return nil, fmt.Errorf("github user decode: %w", err)
	}
	return &u, nil
}

// getUserOrgs returns the `login` of every org the authenticated user is a
// member of, including private memberships (requires read:org scope).
// Pages through results — GitHub's default page size is 30 and we don't want
// to silently truncate at the first page for users in many orgs.
func (g *GitHub) getUserOrgs(ctx context.Context, token string) ([]string, error) {
	var all []string
	page := 1
	for {
		u := fmt.Sprintf("https://api.github.com/user/orgs?per_page=100&page=%d", page)
		req, _ := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
		req.Header.Set("Authorization", "Bearer "+token)
		req.Header.Set("Accept", "application/vnd.github+json")

		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			return nil, fmt.Errorf("github orgs request: %w", err)
		}
		var orgs []struct {
			Login string `json:"login"`
		}
		if err := json.NewDecoder(resp.Body).Decode(&orgs); err != nil {
			resp.Body.Close() //nolint:errcheck
			return nil, fmt.Errorf("github orgs decode: %w", err)
		}
		resp.Body.Close() //nolint:errcheck

		for _, o := range orgs {
			if o.Login != "" {
				all = append(all, o.Login)
			}
		}
		if len(orgs) < 100 {
			break
		}
		page++
	}
	return all, nil
}

func (g *GitHub) getPrimaryEmail(ctx context.Context, token string) (string, error) {
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, "https://api.github.com/user/emails", nil)
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Accept", "application/vnd.github+json")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return "", fmt.Errorf("github emails request: %w", err)
	}
	defer resp.Body.Close() //nolint:errcheck

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
