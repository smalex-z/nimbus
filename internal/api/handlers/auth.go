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
	appURL string
}

// NewAuth creates a new Auth handler. appURL is used for the Google OAuth redirect URI.
func NewAuth(auth *service.AuthService, appURL string) *Auth {
	return &Auth{auth: auth, appURL: appURL}
}

// githubProvider loads GitHub OAuth credentials from the DB on each call.
// Returns nil if credentials are not configured.
func (a *Auth) githubProvider() oauth.Provider {
	settings, err := a.auth.GetOAuthSettings()
	if err != nil || settings.GitHubClientID == "" || settings.GitHubClientSecret == "" {
		return nil
	}
	return &oauth.GitHub{
		ClientID:     settings.GitHubClientID,
		ClientSecret: settings.GitHubClientSecret,
	}
}

// googleProvider loads Google OAuth credentials from the DB on each call.
// Returns nil if credentials are not configured.
func (a *Auth) googleProvider() oauth.Provider {
	settings, err := a.auth.GetOAuthSettings()
	if err != nil || settings.GoogleClientID == "" || settings.GoogleClientSecret == "" {
		return nil
	}
	return &oauth.Google{
		ClientID:     settings.GoogleClientID,
		ClientSecret: settings.GoogleClientSecret,
		RedirectURI:  a.appURL + "/api/auth/google/callback",
	}
}

// Providers handles GET /api/auth/providers — public endpoint that tells the
// frontend which OAuth providers have credentials configured in the DB.
func (a *Auth) Providers(w http.ResponseWriter, r *http.Request) {
	settings, err := a.auth.GetOAuthSettings()
	if err != nil {
		response.Success(w, map[string]bool{"github": false, "google": false})
		return
	}
	response.Success(w, map[string]bool{
		"github": settings.GitHubClientID != "" && settings.GitHubClientSecret != "",
		"google": settings.GoogleClientID != "" && settings.GoogleClientSecret != "",
	})
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

// ListUsers handles GET /api/users.
// Admins receive every account; non-admins receive only their own.
func (a *Auth) ListUsers(w http.ResponseWriter, r *http.Request) {
	user := ctxutil.User(r.Context())
	if user.IsAdmin {
		users, err := a.auth.ListAllUsers()
		if err != nil {
			response.InternalError(w, "Failed to list users")
			return
		}
		response.Success(w, users)
		return
	}
	response.Success(w, []*service.UserView{user})
}

// VerifyStatus handles GET /api/access-code/status — returns whether the
// authenticated user is verified against the current access code version.
func (a *Auth) VerifyStatus(w http.ResponseWriter, r *http.Request) {
	user := ctxutil.User(r.Context())
	if user == nil {
		response.Error(w, http.StatusUnauthorized, "Not authenticated")
		return
	}
	verified, err := a.auth.IsUserVerified(user.ID)
	if err != nil {
		response.InternalError(w, "failed to check verification")
		return
	}
	response.Success(w, map[string]bool{"verified": verified})
}

type verifyAccessCodeRequest struct {
	Code string `json:"code"`
}

// VerifyAccessCode handles POST /api/access-code/verify — checks the supplied
// code against the current access code and, on success, marks the user as
// verified.
func (a *Auth) VerifyAccessCode(w http.ResponseWriter, r *http.Request) {
	user := ctxutil.User(r.Context())
	if user == nil {
		response.Error(w, http.StatusUnauthorized, "Not authenticated")
		return
	}
	var req verifyAccessCodeRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		response.BadRequest(w, "invalid JSON")
		return
	}
	err := a.auth.VerifyAccessCode(user.ID, req.Code)
	if errors.Is(err, service.ErrInvalidAccessCode) {
		response.Error(w, http.StatusUnauthorized, "Invalid access code")
		return
	}
	if err != nil {
		response.InternalError(w, "failed to verify access code")
		return
	}
	response.Success(w, map[string]bool{"verified": true})
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
	a.oauthStart(a.githubProvider())(w, r)
}

func (a *Auth) GitHubCallback(w http.ResponseWriter, r *http.Request) {
	a.oauthCallback(a.githubProvider(), "github")(w, r)
}

// --- Google OAuth -----------------------------------------------------------

func (a *Auth) GoogleStart(w http.ResponseWriter, r *http.Request) {
	a.oauthStart(a.googleProvider())(w, r)
}

// GoogleCallback wraps the shared OAuth callback flow with the
// authorized-domain check. New users whose domain is not on the admin's
// authorized-domain list are blocked before any account is created;
// returning users whose domain IS authorized are auto-verified against the
// current access code version so they bypass the /verify form.
func (a *Auth) GoogleCallback(w http.ResponseWriter, r *http.Request) {
	provider := a.googleProvider()
	if provider == nil {
		http.Redirect(w, r, "/auth/callback?error=exchange_failed&provider=google", http.StatusTemporaryRedirect)
		return
	}

	stateCookie, err := r.Cookie(oauthStateCookie)
	if err != nil || stateCookie.Value != r.URL.Query().Get("state") {
		http.Redirect(w, r, "/auth/callback?error=invalid_state&provider=google", http.StatusTemporaryRedirect)
		return
	}
	http.SetCookie(w, &http.Cookie{Name: oauthStateCookie, Value: "", Path: "/", MaxAge: -1})

	if errParam := r.URL.Query().Get("error"); errParam != "" {
		http.Redirect(w, r, "/auth/callback?provider=google&error="+url.QueryEscape(errParam), http.StatusTemporaryRedirect)
		return
	}

	code := r.URL.Query().Get("code")
	if code == "" {
		http.Redirect(w, r, "/auth/callback?error=missing_code&provider=google", http.StatusTemporaryRedirect)
		return
	}

	userInfo, err := provider.Exchange(r.Context(), code)
	if err != nil {
		http.Redirect(w, r, "/auth/callback?error=exchange_failed&provider=google", http.StatusTemporaryRedirect)
		return
	}

	// Domain gate: when the admin has configured ANY authorized domains, the
	// Google OAuth path is restricted to those domains for every login —
	// including users that already exist (e.g. created via email/password).
	// They can still sign in through the password form; Google OAuth itself
	// is the gated path. An empty list means the gate is off.
	hasGate, err := a.auth.HasAuthorizedGoogleDomains()
	if err != nil {
		http.Redirect(w, r, "/auth/callback?error=user_failed&provider=google", http.StatusTemporaryRedirect)
		return
	}
	if hasGate {
		ok, err := a.auth.IsGoogleDomainAuthorized(userInfo.Email)
		if err != nil {
			http.Redirect(w, r, "/auth/callback?error=user_failed&provider=google", http.StatusTemporaryRedirect)
			return
		}
		if !ok {
			http.Redirect(w, r, "/auth/callback?error=domain_not_authorized&provider=google", http.StatusTemporaryRedirect)
			return
		}
	}

	user, err := a.auth.UpsertOAuthUser(userInfo.Name, userInfo.Email)
	if err != nil {
		http.Redirect(w, r, "/auth/callback?error=user_failed&provider=google", http.StatusTemporaryRedirect)
		return
	}

	// No DB write for the domain bypass — IsUserVerified consults the
	// authorized-domain list dynamically on every request, so the bypass
	// follows admin changes (add/remove) without lag.

	sessionID, err := a.auth.CreateSession(user.ID)
	if err != nil {
		http.Redirect(w, r, "/auth/callback?error=session_failed&provider=google", http.StatusTemporaryRedirect)
		return
	}

	setSessionCookie(w, sessionID)

	q := url.Values{}
	q.Set("provider", "google")
	q.Set("login", userInfo.Login)
	http.Redirect(w, r, "/auth/callback?"+q.Encode(), http.StatusTemporaryRedirect)
}

// --- misc -------------------------------------------------------------------

func generateState() (string, error) {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}
