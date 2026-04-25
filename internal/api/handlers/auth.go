package handlers

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/http"
	"net/url"
	"strings"

	"nimbus/internal/api/response"
	"nimbus/internal/ctxutil"
	"nimbus/internal/oauth"
	"nimbus/internal/service"
)

const (
	sessionCookieName = "nimbus_sid"
	oauthStateCookie  = "nimbus_oauth_state"
)

// Auth handles authentication endpoints.
type Auth struct {
	auth   *service.AuthService
	github oauth.Provider
	google oauth.Provider
}

// NewAuth creates a new Auth handler. github and google may be nil if not configured.
func NewAuth(auth *service.AuthService, github, google oauth.Provider) *Auth {
	return &Auth{auth: auth, github: github, google: google}
}

// --- cookie helpers ---------------------------------------------------------

func setSessionCookie(w http.ResponseWriter, sessionID string) {
	http.SetCookie(w, &http.Cookie{
		Name:     sessionCookieName,
		Value:    sessionID,
		Path:     "/",
		HttpOnly: true,
		SameSite: http.SameSiteLaxMode,
		MaxAge:   7 * 24 * 60 * 60,
	})
}

func clearSessionCookie(w http.ResponseWriter) {
	http.SetCookie(w, &http.Cookie{
		Name:     sessionCookieName,
		Value:    "",
		Path:     "/",
		HttpOnly: true,
		MaxAge:   -1,
	})
}

// --- email/password ---------------------------------------------------------

type registerRequest struct {
	Name     string `json:"name"`
	Email    string `json:"email"`
	Password string `json:"password"`
}

// Register handles POST /api/auth/register.
func (a *Auth) Register(w http.ResponseWriter, r *http.Request) {
	var req registerRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		response.BadRequest(w, "Invalid request body")
		return
	}

	req.Name = strings.TrimSpace(req.Name)
	req.Email = strings.TrimSpace(req.Email)

	switch {
	case req.Name == "":
		response.BadRequest(w, "Name is required")
		return
	case req.Email == "":
		response.BadRequest(w, "Email is required")
		return
	case len(req.Password) < 8:
		response.BadRequest(w, "Password must be at least 8 characters")
		return
	}

	user, err := a.auth.Register(service.RegisterParams{
		Name:     req.Name,
		Email:    req.Email,
		Password: req.Password,
	})
	if errors.Is(err, service.ErrEmailTaken) {
		response.Conflict(w, "An account with that email already exists")
		return
	}
	if err != nil {
		response.InternalError(w, "Failed to create account")
		return
	}

	response.Created(w, user)
}

// Login handles POST /api/auth/login.
func (a *Auth) Login(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Email    string `json:"email"`
		Password string `json:"password"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		response.BadRequest(w, "Invalid request body")
		return
	}
	if strings.TrimSpace(req.Email) == "" || req.Password == "" {
		response.BadRequest(w, "Email and password are required")
		return
	}

	sessionID, user, err := a.auth.Login(req.Email, req.Password)
	if errors.Is(err, service.ErrInvalidCredentials) {
		response.Error(w, http.StatusUnauthorized, "Invalid email or password")
		return
	}
	if err != nil {
		response.InternalError(w, "Failed to sign in")
		return
	}

	setSessionCookie(w, sessionID)
	response.Success(w, user)
}

// Me handles GET /api/me.
// The user is guaranteed to be present by the requireAuth middleware.
func (a *Auth) Me(w http.ResponseWriter, r *http.Request) {
	response.Success(w, ctxutil.User(r.Context()))
}

// Logout handles POST /api/auth/logout.
func (a *Auth) Logout(w http.ResponseWriter, r *http.Request) {
	if cookie, err := r.Cookie(sessionCookieName); err == nil {
		_ = a.auth.Logout(cookie.Value)
	}
	clearSessionCookie(w)
	response.NoContent(w)
}

// --- shared OAuth helpers ---------------------------------------------------

// oauthStart generates a CSRF state, stores it in a short-lived cookie, and
// redirects the browser to the provider's authorization URL.
func (a *Auth) oauthStart(provider oauth.Provider) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if provider == nil {
			response.Error(w, http.StatusServiceUnavailable, "OAuth provider not configured")
			return
		}
		state, err := generateState()
		if err != nil {
			response.InternalError(w, "Failed to initiate OAuth")
			return
		}
		http.SetCookie(w, &http.Cookie{
			Name:     oauthStateCookie,
			Value:    state,
			Path:     "/",
			HttpOnly: true,
			SameSite: http.SameSiteLaxMode,
			MaxAge:   600,
		})
		http.Redirect(w, r, provider.AuthURL(state), http.StatusTemporaryRedirect)
	}
}

// oauthCallback validates state, exchanges the code, upserts the user, issues
// a session cookie, and redirects to the frontend handshake page.
func (a *Auth) oauthCallback(provider oauth.Provider, providerName string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		stateCookie, err := r.Cookie(oauthStateCookie)
		if err != nil || stateCookie.Value != r.URL.Query().Get("state") {
			http.Redirect(w, r, "/auth/callback?error=invalid_state", http.StatusTemporaryRedirect)
			return
		}
		http.SetCookie(w, &http.Cookie{Name: oauthStateCookie, Value: "", Path: "/", MaxAge: -1})

		if errParam := r.URL.Query().Get("error"); errParam != "" {
			http.Redirect(w, r, "/auth/callback?error="+url.QueryEscape(errParam), http.StatusTemporaryRedirect)
			return
		}

		code := r.URL.Query().Get("code")
		if code == "" {
			http.Redirect(w, r, "/auth/callback?error=missing_code", http.StatusTemporaryRedirect)
			return
		}

		userInfo, err := provider.Exchange(r.Context(), code)
		if err != nil {
			http.Redirect(w, r, "/auth/callback?error=exchange_failed", http.StatusTemporaryRedirect)
			return
		}

		user, err := a.auth.UpsertOAuthUser(userInfo.Name, userInfo.Email)
		if err != nil {
			http.Redirect(w, r, "/auth/callback?error=user_failed", http.StatusTemporaryRedirect)
			return
		}

		sessionID, err := a.auth.CreateSession(user.ID)
		if err != nil {
			http.Redirect(w, r, "/auth/callback?error=session_failed", http.StatusTemporaryRedirect)
			return
		}

		setSessionCookie(w, sessionID)

		q := url.Values{}
		q.Set("provider", providerName)
		q.Set("login", userInfo.Login)
		http.Redirect(w, r, "/auth/callback?"+q.Encode(), http.StatusTemporaryRedirect)
	}
}

// --- GitHub OAuth -----------------------------------------------------------

func (a *Auth) GitHubStart(w http.ResponseWriter, r *http.Request) {
	a.oauthStart(a.github)(w, r)
}

func (a *Auth) GitHubCallback(w http.ResponseWriter, r *http.Request) {
	a.oauthCallback(a.github, "github")(w, r)
}

// --- Google OAuth -----------------------------------------------------------

func (a *Auth) GoogleStart(w http.ResponseWriter, r *http.Request) {
	a.oauthStart(a.google)(w, r)
}

func (a *Auth) GoogleCallback(w http.ResponseWriter, r *http.Request) {
	a.oauthCallback(a.google, "google")(w, r)
}

// --- misc -------------------------------------------------------------------

func generateState() (string, error) {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}
