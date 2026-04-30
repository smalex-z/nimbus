package service

import (
	"crypto/rand"
	"crypto/subtle"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"time"

	"nimbus/internal/db"

	"golang.org/x/crypto/bcrypt"
	"gorm.io/gorm"
)

var (
	ErrEmailTaken           = errors.New("email already registered")
	ErrInvalidCredentials   = errors.New("invalid credentials")
	ErrSessionNotFound      = errors.New("session not found or expired")
	ErrAdminAlreadyClaimed  = errors.New("admin already claimed")
	ErrUsersExist           = errors.New("users already exist")
	ErrInvalidAccessCode    = errors.New("invalid access code")
	ErrAccessCodeNotPresent = errors.New("access code not configured")
	ErrDomainNotAuthorized  = errors.New("email domain not authorized")
	ErrOrgNotAuthorized     = errors.New("github org not authorized")
)

const sessionDuration = 7 * 24 * time.Hour

// AuthService handles account creation and credential verification.
type AuthService struct {
	db *db.DB
}

// NewAuthService creates a new AuthService.
func NewAuthService(database *db.DB) *AuthService {
	return &AuthService{db: database}
}

// RegisterParams holds the input for creating a new account.
type RegisterParams struct {
	Name     string
	Email    string
	Password string
}

// UserView is the safe, serialisable projection of a User (no password hash).
type UserView struct {
	ID      uint   `json:"id"`
	Name    string `json:"name"`
	Email   string `json:"email"`
	IsAdmin bool   `json:"is_admin"`
}

func userToView(u *db.User) *UserView {
	return &UserView{ID: u.ID, Name: u.Name, Email: u.Email, IsAdmin: u.IsAdmin}
}

// UserManagementView is the admin-facing user list shape: UserView plus
// signup time, verification status, and provider hints. Returned by
// /api/users so the management page can render a meaningful table without
// N+1 calls. Provider hints are best-effort — Google OAuth doesn't leave
// a per-user marker on the User row, so a user who signed in only via
// Google shows "google" inferred from the absence of password + github.
type UserManagementView struct {
	*UserView
	CreatedAt time.Time `json:"created_at"`
	// Verified follows the same rules as IsUserVerified — admins are
	// always verified, then dynamic Google-domain / GitHub-org bypasses,
	// then the explicit access-code match.
	Verified  bool     `json:"verified"`
	Providers []string `json:"providers"`
}

func userToManagementView(u *db.User, settings *db.OAuthSettings) *UserManagementView {
	v := &UserManagementView{
		UserView:  userToView(u),
		CreatedAt: u.CreatedAt,
		Providers: detectProviders(u),
	}
	v.Verified = isUserVerifiedFromSettings(u, settings)
	return v
}

// detectProviders is a best-effort sniff of how the user has signed in
// at least once. PasswordHash != "" means email/password registration;
// GitHubOrgs is set (even to "-") on every successful GitHub OAuth login.
// A user with neither is presumed Google-only — we don't store a
// per-user Google marker today, so the heuristic is "neither password
// nor github → google". Order is stable: password, github, google.
func detectProviders(u *db.User) []string {
	out := make([]string, 0, 2)
	if u.PasswordHash != "" {
		out = append(out, "password")
	}
	if u.GitHubOrgs != "" {
		out = append(out, "github")
	}
	if len(out) == 0 {
		out = append(out, "google")
	}
	return out
}

// isUserVerifiedFromSettings mirrors IsUserVerified but takes a
// pre-fetched OAuthSettings so a caller iterating over many users avoids
// the N+1. Decision policy is intentionally identical — keep these in
// sync if IsUserVerified ever changes.
func isUserVerifiedFromSettings(u *db.User, settings *db.OAuthSettings) bool {
	if u.IsAdmin {
		return true
	}
	if domain := emailDomain(u.Email); domain != "" {
		for _, d := range splitDomains(settings.AuthorizedGoogleDomains) {
			if d == domain {
				return true
			}
		}
	}
	if hasOrgIntersection(u.GitHubOrgs, settings.AuthorizedGitHubOrgs) {
		return true
	}
	return u.VerifiedCodeVersion == settings.AccessCodeVersion && settings.AccessCodeVersion > 0
}

// Register creates a new user account with a bcrypt-hashed password.
// Returns ErrEmailTaken if the email is already in use.
func (s *AuthService) Register(p RegisterParams) (*UserView, error) {
	email := strings.ToLower(strings.TrimSpace(p.Email))

	var existing db.User
	err := s.db.Where("email = ?", email).First(&existing).Error
	if err == nil {
		return nil, ErrEmailTaken
	}
	if !errors.Is(err, gorm.ErrRecordNotFound) {
		return nil, err
	}

	hash, err := bcrypt.GenerateFromPassword([]byte(p.Password), bcrypt.DefaultCost)
	if err != nil {
		return nil, err
	}

	user := &db.User{
		Name:         strings.TrimSpace(p.Name),
		Email:        email,
		PasswordHash: string(hash),
	}
	if err := s.db.Create(user).Error; err != nil {
		return nil, err
	}

	return userToView(user), nil
}

// Login verifies credentials and returns a new session ID.
// Returns ErrInvalidCredentials if the email is not found or the password is wrong.
func (s *AuthService) Login(email, password string) (string, *UserView, error) {
	email = strings.ToLower(strings.TrimSpace(email))

	var user db.User
	if err := s.db.Where("email = ?", email).First(&user).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return "", nil, ErrInvalidCredentials
		}
		return "", nil, err
	}

	if err := bcrypt.CompareHashAndPassword([]byte(user.PasswordHash), []byte(password)); err != nil {
		return "", nil, ErrInvalidCredentials
	}

	sessionID, err := s.CreateSession(user.ID)
	if err != nil {
		return "", nil, err
	}

	return sessionID, userToView(&user), nil
}

// UpsertOAuthUser finds a user by email or creates one if none exists.
// Used by OAuth providers where a password hash is not applicable.
func (s *AuthService) UpsertOAuthUser(name, email string) (*UserView, error) {
	view, _, err := s.UpsertOAuthUserWithCheck(name, email, nil)
	return view, err
}

// UpsertOAuthUserWithCheck behaves like UpsertOAuthUser but invokes
// allowCreate before creating a brand-new account. Returns
// ErrDomainNotAuthorized when allowCreate returns false. The bool return
// indicates whether the user already existed prior to this call.
func (s *AuthService) UpsertOAuthUserWithCheck(
	name, email string,
	allowCreate func(email string) bool,
) (*UserView, bool, error) {
	email = strings.ToLower(strings.TrimSpace(email))
	name = strings.TrimSpace(name)

	var user db.User
	err := s.db.Where("email = ?", email).First(&user).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		if allowCreate != nil && !allowCreate(email) {
			return nil, false, ErrDomainNotAuthorized
		}
		user = db.User{Name: name, Email: email}
		if err := s.db.Create(&user).Error; err != nil {
			return nil, false, err
		}
		return userToView(&user), false, nil
	}
	if err != nil {
		return nil, false, err
	}
	return userToView(&user), true, nil
}

// HasAuthorizedGoogleDomains reports whether the admin has configured at
// least one authorized Google domain. Used to decide whether the Google
// OAuth path is gated.
func (s *AuthService) HasAuthorizedGoogleDomains() (bool, error) {
	settings, err := s.GetOAuthSettings()
	if err != nil {
		return false, err
	}
	return len(splitDomains(settings.AuthorizedGoogleDomains)) > 0, nil
}

// IsGoogleDomainAuthorized reports whether the given email's domain is in the
// admin-managed authorized-domain list. An empty list disables the bypass and
// always returns false.
func (s *AuthService) IsGoogleDomainAuthorized(email string) (bool, error) {
	settings, err := s.GetOAuthSettings()
	if err != nil {
		return false, err
	}
	domains := splitDomains(settings.AuthorizedGoogleDomains)
	if len(domains) == 0 {
		return false, nil
	}
	domain := emailDomain(email)
	if domain == "" {
		return false, nil
	}
	for _, d := range domains {
		if d == domain {
			return true, nil
		}
	}
	return false, nil
}

// HasAuthorizedGitHubOrgs reports whether the admin has configured at least
// one authorized GitHub org.
func (s *AuthService) HasAuthorizedGitHubOrgs() (bool, error) {
	settings, err := s.GetOAuthSettings()
	if err != nil {
		return false, err
	}
	return len(splitDomains(settings.AuthorizedGitHubOrgs)) > 0, nil
}

// IsGitHubOrgAuthorized reports whether any of the user's orgs is in the
// admin-managed authorized-orgs list. Empty admin list always returns false.
func (s *AuthService) IsGitHubOrgAuthorized(userOrgs []string) (bool, error) {
	settings, err := s.GetOAuthSettings()
	if err != nil {
		return false, err
	}
	authorized := splitDomains(settings.AuthorizedGitHubOrgs)
	if len(authorized) == 0 || len(userOrgs) == 0 {
		return false, nil
	}
	set := make(map[string]struct{}, len(authorized))
	for _, a := range authorized {
		set[strings.ToLower(a)] = struct{}{}
	}
	for _, u := range userOrgs {
		if _, ok := set[strings.ToLower(u)]; ok {
			return true, nil
		}
	}
	return false, nil
}

// SaveAuthorizedGitHubOrgs replaces the authorized-orgs list with the
// supplied entries (lowercased, trimmed, de-duplicated).
func (s *AuthService) SaveAuthorizedGitHubOrgs(orgs []string) error {
	settings, err := s.GetOAuthSettings()
	if err != nil {
		return err
	}
	settings.AuthorizedGitHubOrgs = joinDomains(normalizeDomains(orgs))
	return s.db.Save(settings).Error
}

// UpsertGitHubOAuthUser is the GitHub-specific upsert that also snapshots
// the user's current org memberships into the user row. Mirrors
// UpsertOAuthUserWithCheck for the allowCreate semantics.
func (s *AuthService) UpsertGitHubOAuthUser(
	name, email string,
	orgs []string,
	allowCreate func(orgs []string) bool,
) (*UserView, bool, error) {
	email = strings.ToLower(strings.TrimSpace(email))
	name = strings.TrimSpace(name)
	orgsCSV := joinDomains(normalizeDomains(orgs))

	var user db.User
	err := s.db.Where("email = ?", email).First(&user).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		if allowCreate != nil && !allowCreate(orgs) {
			return nil, false, ErrOrgNotAuthorized
		}
		user = db.User{Name: name, Email: email, GitHubOrgs: orgsCSV}
		if err := s.db.Create(&user).Error; err != nil {
			return nil, false, err
		}
		return userToView(&user), false, nil
	}
	if err != nil {
		return nil, false, err
	}
	// Refresh the org snapshot on every login so the bypass tracks the
	// user's current memberships. Using UpdateColumn with the actual DB
	// column (git_hub_orgs — GORM splits on each uppercase boundary) avoids
	// hooks and zero-value skipping; an empty orgsCSV is a legitimate value
	// when the user has no orgs.
	if err := s.db.Model(&user).UpdateColumn("git_hub_orgs", orgsCSV).Error; err != nil {
		return nil, true, err
	}
	user.GitHubOrgs = orgsCSV
	return userToView(&user), true, nil
}

// SaveAuthorizedGoogleDomains replaces the authorized-domain list with the
// supplied entries. Each entry is lowercased, trimmed, and de-duplicated; any
// blank or "@"-prefixed entries are normalized.
func (s *AuthService) SaveAuthorizedGoogleDomains(domains []string) error {
	settings, err := s.GetOAuthSettings()
	if err != nil {
		return err
	}
	settings.AuthorizedGoogleDomains = joinDomains(normalizeDomains(domains))
	return s.db.Save(settings).Error
}

func splitDomains(joined string) []string {
	if joined == "" {
		return nil
	}
	parts := strings.Split(joined, ",")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		p = strings.TrimSpace(p)
		if p != "" {
			out = append(out, p)
		}
	}
	return out
}

func joinDomains(domains []string) string { return strings.Join(domains, ",") }

func normalizeDomains(in []string) []string {
	seen := make(map[string]struct{}, len(in))
	out := make([]string, 0, len(in))
	for _, d := range in {
		d = strings.ToLower(strings.TrimSpace(d))
		d = strings.TrimPrefix(d, "@")
		if d == "" {
			continue
		}
		if _, ok := seen[d]; ok {
			continue
		}
		seen[d] = struct{}{}
		out = append(out, d)
	}
	return out
}

func emailDomain(email string) string {
	email = strings.ToLower(strings.TrimSpace(email))
	at := strings.LastIndex(email, "@")
	if at < 0 || at == len(email)-1 {
		return ""
	}
	return email[at+1:]
}

// CreateSession creates a new session for the given user ID and returns the session ID.
func (s *AuthService) CreateSession(userID uint) (string, error) {
	id, err := generateSessionID()
	if err != nil {
		return "", err
	}

	session := &db.Session{
		ID:        id,
		UserID:    userID,
		ExpiresAt: time.Now().Add(sessionDuration),
	}
	if err := s.db.Create(session).Error; err != nil {
		return "", err
	}

	return id, nil
}

// GetUserBySessionID returns the user for a valid, non-expired session.
func (s *AuthService) GetUserBySessionID(sessionID string) (*UserView, error) {
	var session db.Session
	err := s.db.Where("id = ? AND expires_at > ?", sessionID, time.Now()).First(&session).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return nil, ErrSessionNotFound
	}
	if err != nil {
		return nil, err
	}

	var user db.User
	if err := s.db.First(&user, session.UserID).Error; err != nil {
		return nil, err
	}

	return userToView(&user), nil
}

// Logout deletes the session record.
func (s *AuthService) Logout(sessionID string) error {
	return s.db.Delete(&db.Session{}, "id = ?", sessionID).Error
}

func generateSessionID() (string, error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}

// generateHexToken mints a hex-encoded random token of `bytes` raw bytes
// (resulting string is 2*bytes long). Used for both session IDs and
// pre-shared bearer tokens like the GPU worker token.
func generateHexToken(bytes int) (string, error) {
	b := make([]byte, bytes)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}

// subtleConstantTimeEq compares two strings in constant time. Used for
// pre-shared bearer tokens so an attacker can't time the comparison to
// recover a valid token byte-by-byte.
func subtleConstantTimeEq(a, b string) bool {
	if len(a) != len(b) {
		return false
	}
	return subtle.ConstantTimeCompare([]byte(a), []byte(b)) == 1
}

// GetGopherSettings returns the stored Gopher tunnel credentials. Creates a
// default empty row on first call.
func (s *AuthService) GetGopherSettings() (*db.GopherSettings, error) {
	var settings db.GopherSettings
	err := s.db.FirstOrCreate(&settings, db.GopherSettings{ID: 1}).Error
	return &settings, err
}

// SaveGopherSettings persists Gopher credentials. Empty fields are treated as
// "preserve existing" so the UI can rotate just the API key without
// re-entering the URL (and vice versa). Only the URL/key columns are
// touched; CloudTunnel* fields are owned by selftunnel.Service and managed
// separately via SaveCloudTunnelState.
func (s *AuthService) SaveGopherSettings(next db.GopherSettings) error {
	existing, err := s.GetGopherSettings()
	if err != nil {
		return err
	}
	if next.APIURL == "" {
		next.APIURL = existing.APIURL
	}
	if next.APIKey == "" {
		next.APIKey = existing.APIKey
	}
	return s.db.Model(&db.GopherSettings{}).Where("id = ?", 1).Updates(map[string]any{
		"api_url": next.APIURL,
		"api_key": next.APIKey,
	}).Error
}

// SaveCloudTunnelState updates only the self-bootstrap columns on the
// GopherSettings row. Used by selftunnel.Service to persist progress
// (machine_id, tunnel_id, URL, state, error) without racing
// SaveGopherSettings on the credentials columns.
func (s *AuthService) SaveCloudTunnelState(state db.GopherSettings) error {
	if _, err := s.GetGopherSettings(); err != nil {
		return err
	}
	return s.db.Model(&db.GopherSettings{}).Where("id = ?", 1).Updates(map[string]any{
		"cloud_machine_id":      state.CloudMachineID,
		"cloud_tunnel_id":       state.CloudTunnelID,
		"cloud_tunnel_url":      state.CloudTunnelURL,
		"cloud_bootstrap_state": state.CloudBootstrapState,
		"cloud_bootstrap_error": state.CloudBootstrapError,
	}).Error
}

// GetGPUSettings returns the GX10 / GPU plane settings, creating an empty
// row on first call. Empty BaseURL or Enabled=false means the GPU plane is
// effectively off — VMs receive no inference env vars and the jobs API
// rejects new submissions.
func (s *AuthService) GetGPUSettings() (*db.GPUSettings, error) {
	var settings db.GPUSettings
	err := s.db.FirstOrCreate(&settings, db.GPUSettings{ID: 1}).Error
	return &settings, err
}

// SaveGPUSettings persists GPU plane settings. Empty string fields are
// treated as "preserve existing" so the UI can rotate just one field at a
// time. The Enabled flag is always written through (no preserve semantics).
func (s *AuthService) SaveGPUSettings(next db.GPUSettings) error {
	existing, err := s.GetGPUSettings()
	if err != nil {
		return err
	}
	if next.BaseURL == "" {
		next.BaseURL = existing.BaseURL
	}
	if next.InferenceModel == "" {
		next.InferenceModel = existing.InferenceModel
	}
	if next.WorkerToken == "" {
		next.WorkerToken = existing.WorkerToken
	}
	next.ID = 1
	return s.db.Save(&next).Error
}

// GetNetworkSettings returns the stored IP pool / gateway configuration.
// Creates an empty row on first call. Empty strings on the row mean the
// caller should fall back to env defaults (handled by main.go's seed step).
func (s *AuthService) GetNetworkSettings() (*db.NetworkSettings, error) {
	var settings db.NetworkSettings
	err := s.db.FirstOrCreate(&settings, db.NetworkSettings{ID: 1}).Error
	return &settings, err
}

// SaveNetworkSettings persists the IP pool / gateway / prefix. Empty fields
// (and zero PrefixLen) are treated as "preserve existing" so the UI can
// rotate one knob without re-sending the others. No validation here — the
// handler checks that strings parse as valid IPv4 addresses before invoking
// this and that PrefixLen is in the legal /1..32 range.
func (s *AuthService) SaveNetworkSettings(next db.NetworkSettings) error {
	existing, err := s.GetNetworkSettings()
	if err != nil {
		return err
	}
	if next.IPPoolStart == "" {
		next.IPPoolStart = existing.IPPoolStart
	}
	if next.IPPoolEnd == "" {
		next.IPPoolEnd = existing.IPPoolEnd
	}
	if next.GatewayIP == "" {
		next.GatewayIP = existing.GatewayIP
	}
	if next.PrefixLen == 0 {
		next.PrefixLen = existing.PrefixLen
	}
	return s.db.Model(&db.NetworkSettings{}).Where("id = ?", 1).Updates(map[string]any{
		"ip_pool_start": next.IPPoolStart,
		"ip_pool_end":   next.IPPoolEnd,
		"gateway_ip":    next.GatewayIP,
		"prefix_len":    next.PrefixLen,
	}).Error
}

// RegenerateGPUWorkerToken mints a fresh 32-byte hex worker token and
// persists it. Returns the new token so the caller can surface it in the
// UI exactly once — admins must capture it for the GX10 install command.
func (s *AuthService) RegenerateGPUWorkerToken() (string, error) {
	tok, err := generateHexToken(32)
	if err != nil {
		return "", err
	}
	settings, err := s.GetGPUSettings()
	if err != nil {
		return "", err
	}
	settings.WorkerToken = tok
	if err := s.db.Save(settings).Error; err != nil {
		return "", err
	}
	return tok, nil
}

// VerifyGPUWorkerToken does a constant-time comparison against the stored
// worker token. Returns false when GPU is disabled, when no token has ever
// been generated, or when the presented token doesn't match.
func (s *AuthService) VerifyGPUWorkerToken(presented string) bool {
	settings, err := s.GetGPUSettings()
	if err != nil || settings.WorkerToken == "" || presented == "" {
		return false
	}
	return subtleConstantTimeEq(settings.WorkerToken, presented)
}

// UnpairGPU clears the GPU plane configuration — wipes the worker token,
// disables the plane, and clears the GX10 facts. Used by the admin
// "Unpair GX10" button. Idempotent: safe to call when no GX10 is paired.
//
// Operator is responsible for stopping the systemd units on the GX10
// itself. Until they do, the worker keeps polling Nimbus and gets 401s —
// harmless but noisy in journalctl on the GX10 side.
func (s *AuthService) UnpairGPU() error {
	settings, err := s.GetGPUSettings()
	if err != nil {
		return err
	}
	settings.Enabled = false
	settings.BaseURL = ""
	settings.InferenceModel = ""
	settings.WorkerToken = ""
	settings.GX10Hostname = ""
	settings.PairingToken = ""
	settings.PairingTokenExpiresAt = nil
	return s.db.Save(settings).Error
}

// gpuPairingTTL is how long a freshly-minted pairing token remains valid
// before MintGPUPairingToken would need to be called again. Five minutes is
// long enough for the operator to SSH from one window to the GX10 and paste
// the curl, short enough that a leaked URL is mostly inert.
const gpuPairingTTL = 5 * time.Minute

// MintGPUPairingToken stamps a fresh pairing token onto the singleton
// GPUSettings row with a TTL. Returns the token so the caller can embed it
// in the install URL. Replaces any existing pairing window — only one is
// active at a time, which matches the single-GX10 design.
func (s *AuthService) MintGPUPairingToken() (string, error) {
	tok, err := generateHexToken(24)
	if err != nil {
		return "", err
	}
	settings, err := s.GetGPUSettings()
	if err != nil {
		return "", err
	}
	exp := time.Now().UTC().Add(gpuPairingTTL)
	settings.PairingToken = tok
	settings.PairingTokenExpiresAt = &exp
	if err := s.db.Save(settings).Error; err != nil {
		return "", err
	}
	return tok, nil
}

// VerifyGPUPairingToken returns true when the presented token matches the
// active pairing token AND has not expired. Constant-time compare against
// the stored value. Does not consume — call ConsumeGPUPairingToken on
// successful registration to clear it.
func (s *AuthService) VerifyGPUPairingToken(presented string) bool {
	if presented == "" {
		return false
	}
	settings, err := s.GetGPUSettings()
	if err != nil || settings.PairingToken == "" || settings.PairingTokenExpiresAt == nil {
		return false
	}
	if time.Now().UTC().After(*settings.PairingTokenExpiresAt) {
		return false
	}
	return subtleConstantTimeEq(settings.PairingToken, presented)
}

// RegisterGPU finalizes the pairing handshake: clears the pairing token,
// records the GX10's self-reported facts, generates a fresh worker token,
// flips Enabled=true, and returns the worker token to the caller (the
// install script writes it into /etc/nimbus-gpu-worker.env).
//
// Idempotent on the worker token: if you call RegisterGPU twice with the
// same valid pairing token… you can't, because the first call clears it.
// A second register attempt fails verification.
//
// `port` is plumbed through so we can stamp the right inference URL —
// vLLM defaults to 8000 but operators can override at install time.
func (s *AuthService) RegisterGPU(hostname, ip, model string, port int) (workerToken string, err error) {
	tok, err := generateHexToken(32)
	if err != nil {
		return "", err
	}
	settings, err := s.GetGPUSettings()
	if err != nil {
		return "", err
	}
	if port == 0 {
		port = 8000
	}
	settings.Enabled = true
	settings.WorkerToken = tok
	settings.BaseURL = fmt.Sprintf("http://%s:%d", ip, port)
	if model != "" {
		settings.InferenceModel = model
	}
	settings.GX10Hostname = hostname
	settings.PairingToken = ""
	settings.PairingTokenExpiresAt = nil
	if err := s.db.Save(settings).Error; err != nil {
		return "", err
	}
	return tok, nil
}

// GetOAuthSettings returns the stored OAuth provider credentials.
// Creates a default empty row on first call. If the row exists but no access
// code has ever been generated, one is generated lazily so the admin always
// has a code to share.
func (s *AuthService) GetOAuthSettings() (*db.OAuthSettings, error) {
	var settings db.OAuthSettings
	if err := s.db.FirstOrCreate(&settings, db.OAuthSettings{ID: 1}).Error; err != nil {
		return nil, err
	}
	if settings.AccessCode == "" {
		code, err := generateAccessCode()
		if err != nil {
			return nil, err
		}
		settings.AccessCode = code
		settings.AccessCodeVersion = 1
		if err := s.db.Save(&settings).Error; err != nil {
			return nil, err
		}
	}
	return &settings, nil
}

// RegenerateAccessCode replaces the stored access code with a fresh one and
// bumps the version, invalidating every non-admin user's prior verification.
func (s *AuthService) RegenerateAccessCode() (*db.OAuthSettings, error) {
	settings, err := s.GetOAuthSettings()
	if err != nil {
		return nil, err
	}
	code, err := generateAccessCode()
	if err != nil {
		return nil, err
	}
	settings.AccessCode = code
	settings.AccessCodeVersion++
	if err := s.db.Save(settings).Error; err != nil {
		return nil, err
	}
	return settings, nil
}

// VerifyAccessCode checks the supplied code against the stored access code and,
// on success, marks the user as verified against the current version.
func (s *AuthService) VerifyAccessCode(userID uint, code string) error {
	settings, err := s.GetOAuthSettings()
	if err != nil {
		return err
	}
	if settings.AccessCode == "" {
		return ErrAccessCodeNotPresent
	}
	if strings.TrimSpace(code) != settings.AccessCode {
		return ErrInvalidAccessCode
	}
	return s.db.Model(&db.User{}).
		Where("id = ?", userID).
		Update("verified_code_version", settings.AccessCodeVersion).
		Error
}

// IsUserVerified returns true when the user has access to the console under
// the current authentication policy. Admins always pass. For everyone else,
// access is granted dynamically when the user's email domain is in the
// admin's current authorized-Google-domains list, OR when the user has
// explicitly entered the current access code (their stored
// verified_code_version matches).
//
// The domain check is intentionally dynamic and never persisted, so
// adding/removing a domain takes effect on the very next request.
func (s *AuthService) IsUserVerified(userID uint) (bool, error) {
	var user db.User
	if err := s.db.First(&user, userID).Error; err != nil {
		return false, err
	}
	if user.IsAdmin {
		return true, nil
	}
	settings, err := s.GetOAuthSettings()
	if err != nil {
		return false, err
	}
	if domain := emailDomain(user.Email); domain != "" {
		for _, d := range splitDomains(settings.AuthorizedGoogleDomains) {
			if d == domain {
				return true, nil
			}
		}
	}
	if hasOrgIntersection(user.GitHubOrgs, settings.AuthorizedGitHubOrgs) {
		return true, nil
	}
	return user.VerifiedCodeVersion == settings.AccessCodeVersion && settings.AccessCodeVersion > 0, nil
}

// hasOrgIntersection returns true when the user's stored org snapshot shares
// at least one entry with the admin's authorized list. Comparison is
// case-insensitive (lists are normalized at write time).
func hasOrgIntersection(userOrgsCSV, authorizedCSV string) bool {
	user := splitDomains(strings.ToLower(userOrgsCSV))
	if len(user) == 0 {
		return false
	}
	authorized := splitDomains(strings.ToLower(authorizedCSV))
	if len(authorized) == 0 {
		return false
	}
	set := make(map[string]struct{}, len(authorized))
	for _, a := range authorized {
		set[a] = struct{}{}
	}
	for _, u := range user {
		if _, ok := set[u]; ok {
			return true
		}
	}
	return false
}

// generateAccessCode produces a uniformly random 8-digit numeric code.
func generateAccessCode() (string, error) {
	var b [8]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	n := binary.BigEndian.Uint64(b[:]) % 100_000_000
	return fmt.Sprintf("%08d", n), nil
}

// SaveOAuthSettings persists OAuth provider credentials. Any field left empty
// keeps its existing value, so the UI can update one provider without touching
// the other.
func (s *AuthService) SaveOAuthSettings(next db.OAuthSettings) error {
	existing, err := s.GetOAuthSettings()
	if err != nil {
		return err
	}
	if next.GitHubClientID == "" {
		next.GitHubClientID = existing.GitHubClientID
	}
	if next.GitHubClientSecret == "" {
		next.GitHubClientSecret = existing.GitHubClientSecret
	}
	if next.GoogleClientID == "" {
		next.GoogleClientID = existing.GoogleClientID
	}
	if next.GoogleClientSecret == "" {
		next.GoogleClientSecret = existing.GoogleClientSecret
	}
	// Always preserve access code state and authorized lists — OAuth saves
	// never touch them.
	next.AccessCode = existing.AccessCode
	next.AccessCodeVersion = existing.AccessCodeVersion
	next.AuthorizedGoogleDomains = existing.AuthorizedGoogleDomains
	next.AuthorizedGitHubOrgs = existing.AuthorizedGitHubOrgs
	next.ID = 1
	return s.db.Save(&next).Error
}

// RegisterFirstAdmin creates the first admin account. Returns ErrUsersExist if
// any user already exists — only valid for the initial setup wizard.
func (s *AuthService) RegisterFirstAdmin(p RegisterParams) (*UserView, error) {
	has, err := s.HasAnyUsers()
	if err != nil {
		return nil, err
	}
	if has {
		return nil, ErrUsersExist
	}

	email := strings.ToLower(strings.TrimSpace(p.Email))
	hash, err := bcrypt.GenerateFromPassword([]byte(p.Password), bcrypt.DefaultCost)
	if err != nil {
		return nil, err
	}

	user := &db.User{
		Name:         strings.TrimSpace(p.Name),
		Email:        email,
		PasswordHash: string(hash),
		IsAdmin:      true,
	}
	if err := s.db.Create(user).Error; err != nil {
		return nil, err
	}
	// Eagerly seed the access code so the admin can view it immediately on
	// the Authentication settings page.
	if _, err := s.GetOAuthSettings(); err != nil {
		return nil, err
	}
	return userToView(user), nil
}

// HasAnyUsers returns true if at least one user account exists.
func (s *AuthService) HasAnyUsers() (bool, error) {
	var count int64
	err := s.db.Model(&db.User{}).Count(&count).Error
	return count > 0, err
}

// IsAdminClaimed returns true if at least one admin account exists.
func (s *AuthService) IsAdminClaimed() (bool, error) {
	var count int64
	err := s.db.Model(&db.User{}).Where("is_admin = ?", true).Count(&count).Error
	return count > 0, err
}

// PromoteFirstUserIfNoAdmin promotes the oldest user (lowest ID) to admin
// when no admin currently exists. Idempotent — returns (promoted, err).
//
// Why this exists: when `is_admin` was added to the User struct, AutoMigrate
// added the column with default false. Pre-existing rows therefore became
// non-admin members. HasAnyUsers() still returns true so the setup wizard
// doesn't prompt for admin creation either, leaving the system in a state
// where every user is locked out of admin features. This backfill runs at
// startup (so `nimbus install --upgrade` triggers it via main()) and
// recovers single-tenant homelab installs without operator intervention.
func (s *AuthService) PromoteFirstUserIfNoAdmin() (bool, error) {
	var adminCount int64
	if err := s.db.Model(&db.User{}).Where("is_admin = ?", true).Count(&adminCount).Error; err != nil {
		return false, err
	}
	if adminCount > 0 {
		return false, nil
	}
	var first db.User
	err := s.db.Order("id ASC").First(&first).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return false, nil // fresh install — setup wizard handles admin creation
	}
	if err != nil {
		return false, err
	}
	if err := s.db.Model(&db.User{}).Where("id = ?", first.ID).Update("is_admin", true).Error; err != nil {
		return false, err
	}
	return true, nil
}

// ClaimAdmin promotes the given user to admin. Returns ErrAdminAlreadyClaimed
// if any admin already exists.
func (s *AuthService) ClaimAdmin(userID uint) error {
	claimed, err := s.IsAdminClaimed()
	if err != nil {
		return err
	}
	if claimed {
		return ErrAdminAlreadyClaimed
	}
	return s.db.Model(&db.User{}).Where("id = ?", userID).Update("is_admin", true).Error
}

// ListAllUsers returns a view of every registered account.
func (s *AuthService) ListAllUsers() ([]*UserView, error) {
	var users []db.User
	if err := s.db.Find(&users).Error; err != nil {
		return nil, err
	}
	views := make([]*UserView, len(users))
	for i := range users {
		views[i] = userToView(&users[i])
	}
	return views, nil
}

// ListAllUsersForManagement returns the richer admin-facing shape:
// CreatedAt + Verified + provider hints, computed against a single
// OAuthSettings fetch so this stays O(1) DB reads regardless of user
// count.
func (s *AuthService) ListAllUsersForManagement() ([]*UserManagementView, error) {
	var users []db.User
	if err := s.db.Order("created_at DESC").Find(&users).Error; err != nil {
		return nil, err
	}
	settings, err := s.GetOAuthSettings()
	if err != nil {
		return nil, err
	}
	out := make([]*UserManagementView, len(users))
	for i := range users {
		out[i] = userToManagementView(&users[i], settings)
	}
	return out, nil
}
