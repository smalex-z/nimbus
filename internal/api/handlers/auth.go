package handlers

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"log"
	"net/http"
	"net/url"
	"strings"
	"time"

	"nimbus/internal/api/response"
	"nimbus/internal/ctxutil"
	"nimbus/internal/ippool"
	"nimbus/internal/oauth"
	"nimbus/internal/service"
)

const (
	sessionCookieName = "nimbus_sid"
	oauthStateCookie  = "nimbus_oauth_state"
)

// loginReconciler is the small interface the Auth handler uses to kick a
// post-login reconcile. *ippool.Reconciler satisfies it; nil is allowed.
type loginReconciler interface {
	Reconcile(ctx context.Context) (ippool.Report, error)
}

// Auth handles authentication endpoints.
type Auth struct {
	auth       *service.AuthService
	appURL     string
	reconciler loginReconciler
}

// NewAuth creates a new Auth handler. appURL is the env-configured fallback
// for the OAuth redirect URI; the resolver also consults the live
// GopherSettings.CloudTunnelURL and the inbound request host so a properly
// self-bootstrapped Nimbus doesn't need APP_URL set in env.
func NewAuth(auth *service.AuthService, appURL string, reconciler loginReconciler) *Auth {
	return &Auth{auth: auth, appURL: appURL, reconciler: reconciler}
}

// kickReconcile launches a background reconcile, decoupled from the request
// context so it survives the response. Caller should fire-and-forget.
func (a *Auth) kickReconcile() {
	if a.reconciler == nil {
		return
	}
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		if rep, err := a.reconciler.Reconcile(ctx); err != nil {
			log.Printf("post-login reconcile failed: %v", err)
		} else if len(rep.Adopted) > 0 || len(rep.Conflicts) > 0 || len(rep.Freed) > 0 || len(rep.Vacated) > 0 {
			log.Printf("post-login reconcile: adopted=%d conflicts=%d freed=%d vacated=%d",
				len(rep.Adopted), len(rep.Conflicts), len(rep.Freed), len(rep.Vacated))
		}
	}()
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
// Returns nil if credentials are not configured. The redirect URI is
// resolved from the request so the same binary works whether the operator
// reaches Nimbus via raw IP, APP_URL, or the Gopher self-tunnel hostname.
func (a *Auth) googleProvider(r *http.Request) oauth.Provider {
	settings, err := a.auth.GetOAuthSettings()
	if err != nil || settings.GoogleClientID == "" || settings.GoogleClientSecret == "" {
		return nil
	}
	return &oauth.Google{
		ClientID:     settings.GoogleClientID,
		ClientSecret: settings.GoogleClientSecret,
		RedirectURI:  a.ResolveAppURL(r) + "/api/auth/google/callback",
	}
}

// ResolveAppURL returns the public origin Nimbus should use when telling
// external services (Google's OAuth in particular) where to send the user
// back. Resolution order:
//
//  1. db.GopherSettings.CloudTunnelURL — populated by the Gopher self-bootstrap
//     once cloud.<domain> is live; preferred because it survives operator
//     IP changes and is what a public browser will hit.
//  2. cfg.AppURL — env-configured override; ignored when it's the bare default
//     localhost:5173 or any loopback address (those won't roundtrip through
//     a remote OAuth provider).
//  3. The inbound request's scheme + host — last resort that at least matches
//     the URL the admin's browser is currently on.
//
// Always returned without a trailing slash so callers can append paths
// directly.
func (a *Auth) ResolveAppURL(r *http.Request) string {
	if settings, err := a.auth.GetGopherSettings(); err == nil {
		if u := strings.TrimRight(strings.TrimSpace(settings.CloudTunnelURL), "/"); u != "" {
			return u
		}
	}
	if u := strings.TrimRight(strings.TrimSpace(a.appURL), "/"); u != "" && !looksLikeLocalhostURL(u) {
		return u
	}
	if r != nil && r.Host != "" {
		scheme := "http"
		if r.TLS != nil || r.Header.Get("X-Forwarded-Proto") == "https" {
			scheme = "https"
		}
		return scheme + "://" + r.Host
	}
	return strings.TrimRight(a.appURL, "/")
}

// looksLikeLocalhostURL reports whether the URL points at a loopback host.
// Mirrors the helper in provision/gpu_bootstrap.go but lives here so the
// auth handler doesn't take a provision dependency.
func looksLikeLocalhostURL(u string) bool {
	if u == "" {
		return true
	}
	s := strings.ToLower(u)
	s = strings.TrimPrefix(strings.TrimPrefix(s, "https://"), "http://")
	if i := strings.IndexAny(s, "/?#"); i >= 0 {
		s = s[:i]
	}
	if i := strings.LastIndex(s, ":"); i >= 0 && !strings.Contains(s[:i], ":") {
		s = s[:i]
	}
	return s == "localhost" ||
		strings.HasPrefix(s, "127.") ||
		s == "0.0.0.0" ||
		s == "::1" ||
		s == "[::1]"
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
	a.kickReconcile()
	response.Success(w, user)
}

// Me handles GET /api/me.
// The user is guaranteed to be present by the requireAuth middleware.
func (a *Auth) Me(w http.ResponseWriter, r *http.Request) {
	response.Success(w, ctxutil.User(r.Context()))
}

// ListUsers handles GET /api/users.
// Admins receive every account in the richer management shape (with
// signup time, verification status, and provider hints); non-admins
// receive only their own self-view (no list of peers).
func (a *Auth) ListUsers(w http.ResponseWriter, r *http.Request) {
	user := ctxutil.User(r.Context())
	if user.IsAdmin {
		users, err := a.auth.ListAllUsersForManagement()
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

// --- GitHub OAuth -----------------------------------------------------------

func (a *Auth) GitHubStart(w http.ResponseWriter, r *http.Request) {
	a.oauthStart(a.githubProvider())(w, r)
}

// GitHubCallback wraps the OAuth flow with the authorized-orgs check. New
// users whose GitHub org memberships don't intersect the admin's authorized
// list are blocked before any account is created. Every login refreshes the
// user's stored org snapshot so the dynamic bypass in IsUserVerified always
// reflects the most recent login's memberships.
func (a *Auth) GitHubCallback(w http.ResponseWriter, r *http.Request) {
	provider := a.githubProvider()
	if provider == nil {
		http.Redirect(w, r, "/auth/callback?error=exchange_failed&provider=github", http.StatusTemporaryRedirect)
		return
	}

	stateCookie, err := r.Cookie(oauthStateCookie)
	if err != nil || stateCookie.Value != r.URL.Query().Get("state") {
		http.Redirect(w, r, "/auth/callback?error=invalid_state&provider=github", http.StatusTemporaryRedirect)
		return
	}
	http.SetCookie(w, &http.Cookie{Name: oauthStateCookie, Value: "", Path: "/", MaxAge: -1})

	if errParam := r.URL.Query().Get("error"); errParam != "" {
		http.Redirect(w, r, "/auth/callback?provider=github&error="+url.QueryEscape(errParam), http.StatusTemporaryRedirect)
		return
	}

	code := r.URL.Query().Get("code")
	if code == "" {
		http.Redirect(w, r, "/auth/callback?error=missing_code&provider=github", http.StatusTemporaryRedirect)
		return
	}

	userInfo, err := provider.Exchange(r.Context(), code)
	if err != nil {
		http.Redirect(w, r, "/auth/callback?error=exchange_failed&provider=github", http.StatusTemporaryRedirect)
		return
	}

	// Org gate: when the admin has configured ANY authorized orgs, every
	// GitHub OAuth login is restricted to members of those orgs — including
	// users that already exist (e.g. created via email/password). They can
	// still sign in through the password form. Empty list disables the gate.
	hasGate, err := a.auth.HasAuthorizedGitHubOrgs()
	if err != nil {
		http.Redirect(w, r, "/auth/callback?error=user_failed&provider=github", http.StatusTemporaryRedirect)
		return
	}
	if hasGate {
		ok, err := a.auth.IsGitHubOrgAuthorized(userInfo.Orgs)
		if err != nil {
			http.Redirect(w, r, "/auth/callback?error=user_failed&provider=github", http.StatusTemporaryRedirect)
			return
		}
		if !ok {
			// Revoke the just-issued token so the user's next attempt
			// presents GitHub's consent screen — without this, GitHub
			// silently re-issues the same authorization to the same
			// account and the user is stuck on the rejection page.
			if gh, ok := provider.(*oauth.GitHub); ok {
				_ = gh.RevokeToken(r.Context(), userInfo.Token)
			}
			http.Redirect(w, r, "/auth/callback?error=org_not_authorized&provider=github", http.StatusTemporaryRedirect)
			return
		}
	}

	// allowCreate is intentionally permissive here — the gate above already
	// rejected unauthorized users when the gate is on. When the gate is off,
	// new accounts are allowed (and will fall into the access code flow).
	user, _, err := a.auth.UpsertGitHubOAuthUser(userInfo.Name, userInfo.Email, userInfo.Orgs, nil)
	if err != nil {
		http.Redirect(w, r, "/auth/callback?error=user_failed&provider=github", http.StatusTemporaryRedirect)
		return
	}

	sessionID, err := a.auth.CreateSession(user.ID)
	if err != nil {
		http.Redirect(w, r, "/auth/callback?error=session_failed&provider=github", http.StatusTemporaryRedirect)
		return
	}

	setSessionCookie(w, sessionID)
	a.kickReconcile()

	q := url.Values{}
	q.Set("provider", "github")
	q.Set("login", userInfo.Login)
	http.Redirect(w, r, "/auth/callback?"+q.Encode(), http.StatusTemporaryRedirect)
}

// --- Google OAuth -----------------------------------------------------------

func (a *Auth) GoogleStart(w http.ResponseWriter, r *http.Request) {
	a.oauthStart(a.googleProvider(r))(w, r)
}

// GoogleCallback wraps the shared OAuth callback flow with the
// authorized-domain check. New users whose domain is not on the admin's
// authorized-domain list are blocked before any account is created;
// returning users whose domain IS authorized are auto-verified against the
// current access code version so they bypass the /verify form.
func (a *Auth) GoogleCallback(w http.ResponseWriter, r *http.Request) {
	provider := a.googleProvider(r)
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
	a.kickReconcile()

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
