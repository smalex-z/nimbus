package handlers

import (
	"context"
	"encoding/json"
	"log"
	"net"
	"net/http"
	"strings"
	"time"

	"nimbus/internal/api/response"
	"nimbus/internal/audit"
	"nimbus/internal/db"
	"nimbus/internal/provision"
	"nimbus/internal/selftunnel"
	"nimbus/internal/service"
	"nimbus/internal/tunnel"
	"nimbus/internal/vnetmgr"
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

// GPUConfigApplier is implemented by anything that holds the live
// provision.GPUBootstrapConfig and needs to be re-pushed when admins edit
// the GPU settings. provision.Service satisfies via SetGPUBootstrapConfig.
type GPUConfigApplier interface {
	SetGPUBootstrapConfig(provision.GPUBootstrapConfig)
}

// NetworkApplier is implemented by anything that needs to be told the live
// gateway IP and cloud-init prefix length after a Settings → Network save.
// provision.Service satisfies via SetGatewayIP / SetPrefixLen.
type NetworkApplier interface {
	SetGatewayIP(string)
	SetPrefixLen(int)
}

// NetworkOps performs the disruptive renumber + force-gateway batch ops.
// provision.Service satisfies this; the handler keeps it as an interface so
// tests can inject a fake without booting the whole orchestrator.
type NetworkOps interface {
	RenumberAllVMs(ctx context.Context, gateway, poolStart, poolEnd string) (provision.NetworkOpReport, error)
	ForceGatewayUpdate(ctx context.Context, gateway string) (provision.NetworkOpReport, error)
}

// PoolReseeder is what the network handler uses to converge the IP pool table
// to the new range after a save. *ippool.Pool satisfies it.
type PoolReseeder interface {
	Reseed(ctx context.Context, start, end string) (added, removedFree, stranded int, err error)
}

// SDNBootstrapper drives zone reconcile + status reads. *vnetmgr.Service
// satisfies it. SaveSDN calls Bootstrap on a false→true transition so the
// admin's first save lands the zone in Proxmox without a restart; GetSDN
// is the diagnostic read for the admin UI's status panel.
type SDNBootstrapper interface {
	Bootstrap(ctx context.Context) error
	Status(ctx context.Context) (*vnetmgr.StatusView, error)
}

// SelfBootstrap is implemented by selftunnel.Service. SaveGopher fires
// Start in a goroutine after a successful credential save, and the
// /self-bootstrap status/start endpoints proxy to it.
//
// SetGopherClient takes the concrete *tunnel.Client to avoid an interface
// re-declaration that selftunnel.Service wouldn't satisfy by name.
type SelfBootstrap interface {
	Start(ctx context.Context) error
	Status() (db.GopherSettings, error)
	SetGopherClient(c *tunnel.Client)
}

// AppURLResolver resolves the public origin Nimbus is reachable at, taking
// the live Gopher self-tunnel and inbound request into account. *Auth
// satisfies it via ResolveAppURL.
type AppURLResolver interface {
	ResolveAppURL(r *http.Request) string
}

// Settings handles admin-only configuration endpoints.
type Settings struct {
	auth            *service.AuthService
	appliers        []TunnelClientApplier
	tunnels         TunnelInfoSetter
	gpuAppliers     []GPUConfigApplier
	nimbusAppURL    string // captured at construction so SaveGPU can build NimbusGPUAPI URL
	appURLResolver  AppURLResolver
	selfBootstrap   SelfBootstrap
	networkAppliers []NetworkApplier
	networkOps      NetworkOps
	pool            PoolReseeder
	audit           *audit.Service
	sdn             SDNBootstrapper
}

func NewSettings(auth *service.AuthService) *Settings {
	return &Settings{auth: auth}
}

// WithAudit installs the audit-log sink. Nil disables emission; tests
// pass nil and production wires d.Audit.
func (s *Settings) WithAudit(a *audit.Service) *Settings { s.audit = a; return s }

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

// WithGPUAppliers registers components that should be notified when GPU
// settings change. Same shape as WithTunnelAppliers.
func (s *Settings) WithGPUAppliers(a ...GPUConfigApplier) *Settings {
	s.gpuAppliers = append(s.gpuAppliers, a...)
	return s
}

// WithNimbusAppURL captures the configured AppURL so SaveGPU can compose
// the per-VM NIMBUS_GPU_API var without threading config through every
// caller.
func (s *Settings) WithNimbusAppURL(u string) *Settings {
	s.nimbusAppURL = u
	return s
}

// WithAppURLResolver wires the live redirect-URI resolver so GetOAuth can
// surface what Nimbus is actually going to send to Google. *Auth satisfies
// this via ResolveAppURL.
func (s *Settings) WithAppURLResolver(r AppURLResolver) *Settings {
	s.appURLResolver = r
	return s
}

// WithSelfBootstrap wires the selftunnel.Service so SaveGopher can kick
// off the self-bootstrap automatically + the modal endpoints have
// something to talk to.
func (s *Settings) WithSelfBootstrap(b SelfBootstrap) *Settings {
	s.selfBootstrap = b
	return s
}

// WithNetworkAppliers registers components that need to be told the live
// gateway IP after a network settings save (provision.Service mainly).
func (s *Settings) WithNetworkAppliers(a ...NetworkApplier) *Settings {
	s.networkAppliers = append(s.networkAppliers, a...)
	return s
}

// WithNetworkOps wires the renumber / force-gateway batch operator.
// provision.Service satisfies it.
func (s *Settings) WithNetworkOps(o NetworkOps) *Settings {
	s.networkOps = o
	return s
}

// WithPoolReseeder wires the IP pool so SaveNetwork can converge the
// allocation table to the new range. *ippool.Pool satisfies it.
func (s *Settings) WithPoolReseeder(p PoolReseeder) *Settings {
	s.pool = p
	return s
}

// WithSDNBootstrapper wires the per-user VNet manager so SaveSDN can
// kick a fresh zone bootstrap on enable, and GetSDN can render live
// zone status to the admin UI. *vnetmgr.Service satisfies it.
func (s *Settings) WithSDNBootstrapper(b SDNBootstrapper) *Settings {
	s.sdn = b
	return s
}

type oauthSettingsView struct {
	GitHubClientID   string `json:"github_client_id"`
	GoogleClientID   string `json:"google_client_id"`
	GitHubConfigured bool   `json:"github_configured"`
	GoogleConfigured bool   `json:"google_configured"`

	// GoogleRedirectURI is the exact URI Nimbus will send to Google on the
	// next OAuth start — must be registered byte-for-byte in Google Cloud
	// Console. Empty if the resolver isn't wired (older callers).
	GoogleRedirectURI string `json:"google_redirect_uri"`
	// GitHubCallbackURL is the matching value for GitHub's "Authorization
	// callback URL" field. GitHub doesn't accept a redirect_uri query param
	// the way Google does — it uses whatever is registered on the OAuth app
	// — so this is purely informational for the admin.
	GitHubCallbackURL string `json:"github_callback_url"`
	// RedirectURISource explains where the host portion came from so the UI
	// can show the right hint. One of: "cloud_tunnel" | "app_url" |
	// "request_host" | "" (resolver missing).
	RedirectURISource string `json:"redirect_uri_source"`
	// RedirectURIWarning is set when the resolved host is something Google
	// will reject (raw IP other than 127.0.0.1, loopback). Empty when the
	// host looks acceptable.
	RedirectURIWarning string `json:"redirect_uri_warning,omitempty"`
}

// GetOAuth handles GET /api/settings/oauth. Secrets are never returned.
//
// @Summary     Read OAuth provider settings (admin)
// @Description Secrets are never sent — only the configured/not flag and the
// @Description resolved redirect URIs the SPA shows admins so they can
// @Description register them with Google/GitHub.
// @Tags        settings
// @Security    cookieAuth
// @Produce     json
// @Success     200 {object} EnvelopeOK{data=oauthSettingsView}
// @Failure     401 {object} EnvelopeError
// @Failure     403 {object} EnvelopeError
// @Failure     500 {object} EnvelopeError
// @Router      /settings/oauth [get]
func (s *Settings) GetOAuth(w http.ResponseWriter, r *http.Request) {
	settings, err := s.auth.GetOAuthSettings()
	if err != nil {
		response.InternalError(w, "failed to load OAuth settings")
		return
	}
	view := oauthSettingsView{
		GitHubClientID:   settings.GitHubClientID,
		GoogleClientID:   settings.GoogleClientID,
		GitHubConfigured: settings.GitHubClientID != "" && settings.GitHubClientSecret != "",
		GoogleConfigured: settings.GoogleClientID != "" && settings.GoogleClientSecret != "",
	}
	if s.appURLResolver != nil {
		base := s.appURLResolver.ResolveAppURL(r)
		view.GoogleRedirectURI = base + "/api/auth/google/callback"
		view.GitHubCallbackURL = base + "/api/auth/github/callback"
		view.RedirectURISource = redirectURISource(base, s.auth, s.nimbusAppURL)
		view.RedirectURIWarning = redirectURIWarning(base)
	}
	response.Success(w, view)
}

// redirectURISource categorises which input the resolver picked, mirroring
// the precedence in Auth.ResolveAppURL. The settings handler uses this to
// drive a one-line UI hint ("registered Cloud Tunnel" / "from APP_URL" /
// "guessed from this browser session").
func redirectURISource(base string, auth *service.AuthService, envAppURL string) string {
	trim := func(s string) string { return strings.TrimRight(strings.TrimSpace(s), "/") }
	if gopher, err := auth.GetGopherSettings(); err == nil {
		if u := trim(gopher.CloudTunnelURL); u != "" && u == base {
			return "cloud_tunnel"
		}
	}
	if u := trim(envAppURL); u != "" && !looksLikeLocalhost(stripScheme(u)) && u == base {
		return "app_url"
	}
	return "request_host"
}

// redirectURIWarning returns operator-readable copy when the resolved host
// is something Google will refuse on a Web App credential type. Returns
// empty string when the host looks acceptable.
func redirectURIWarning(base string) string {
	host := stripScheme(base)
	if host == "" || looksLikeLocalhost(host) {
		return "Resolved host is a loopback address. Google won't accept it as a redirect URI; either set APP_URL or finish the Gopher self-bootstrap so cloud.<domain> is live."
	}
	if isRawIPHost(host) {
		return "Resolved host is a raw IP address. Google rejects raw IPs for redirect URIs (other than 127.0.0.1) — register a hostname (a custom domain or the Gopher cloud.<domain>) before saving credentials."
	}
	return ""
}

// stripScheme drops the leading scheme + path from a URL string, leaving
// only host[:port]. Empty input yields empty output.
func stripScheme(u string) string {
	s := strings.TrimSpace(u)
	s = strings.TrimPrefix(strings.TrimPrefix(s, "https://"), "http://")
	if i := strings.IndexAny(s, "/?#"); i >= 0 {
		s = s[:i]
	}
	return s
}

// isRawIPHost reports whether host (no scheme, may include :port) is a
// dotted-decimal IPv4 address. Used to flag the "Google rejects raw IPs"
// case without firing on hostnames that just happen to start with digits.
func isRawIPHost(host string) bool {
	h := host
	if i := strings.LastIndex(h, ":"); i >= 0 && !strings.Contains(h[:i], ":") {
		h = h[:i]
	}
	if h == "" {
		return false
	}
	return net.ParseIP(h) != nil && net.ParseIP(h).To4() != nil
}

type saveOAuthRequest struct {
	GitHubClientID     string `json:"github_client_id"`
	GitHubClientSecret string `json:"github_client_secret"`
	GoogleClientID     string `json:"google_client_id"`
	GoogleClientSecret string `json:"google_client_secret"`
}

// SaveOAuth handles PUT /api/settings/oauth.
//
// @Summary     Update OAuth provider credentials (admin)
// @Tags        settings
// @Security    cookieAuth
// @Accept      json
// @Param       body body     saveOAuthRequest true "OAuth client IDs + secrets"
// @Success     200  {object} EnvelopeOK
// @Failure     400  {object} EnvelopeError
// @Failure     401  {object} EnvelopeError
// @Failure     403  {object} EnvelopeError
// @Failure     500  {object} EnvelopeError
// @Router      /settings/oauth [put]
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
		s.audit.Record(r.Context(), audit.Event{
			Action:   "settings.oauth.update",
			Success:  false,
			ErrorMsg: err.Error(),
		})
		response.InternalError(w, "failed to save OAuth settings")
		return
	}
	// Don't log secrets — just which providers were configured.
	s.audit.Record(r.Context(), audit.Event{
		Action: "settings.oauth.update",
		Details: map[string]any{
			"github_configured": req.GitHubClientID != "",
			"google_configured": req.GoogleClientID != "",
		},
		Success: true,
	})
	response.Success(w, map[string]string{"message": "OAuth settings saved"})
}

type accessCodeView struct {
	AccessCode string `json:"access_code"`
	Version    int    `json:"version"`
}

// GetAccessCode handles GET /api/settings/access-code (admin only).
// Returns the raw 8-digit code so the admin UI can reveal it on demand.
//
// @Summary     Reveal the current access code (admin)
// @Tags        settings
// @Security    cookieAuth
// @Produce     json
// @Success     200 {object} EnvelopeOK{data=accessCodeView}
// @Failure     401 {object} EnvelopeError
// @Failure     403 {object} EnvelopeError
// @Failure     500 {object} EnvelopeError
// @Router      /settings/access-code [get]
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
//
// @Summary     List authorized Google Workspace domains (admin)
// @Description Empty list means any verified Google account may sign in.
// @Tags        settings
// @Security    cookieAuth
// @Produce     json
// @Success     200 {object} EnvelopeOK{data=authorizedDomainsView}
// @Failure     401 {object} EnvelopeError
// @Failure     403 {object} EnvelopeError
// @Failure     500 {object} EnvelopeError
// @Router      /settings/google-domains [get]
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
//
// @Summary     Replace the authorized Google Workspace domains (admin)
// @Tags        settings
// @Security    cookieAuth
// @Accept      json
// @Produce     json
// @Param       body body     saveAuthorizedDomainsRequest true "Domain list"
// @Success     200  {object} EnvelopeOK{data=authorizedDomainsView}
// @Failure     400  {object} EnvelopeError
// @Failure     401  {object} EnvelopeError
// @Failure     403  {object} EnvelopeError
// @Failure     500  {object} EnvelopeError
// @Router      /settings/google-domains [put]
func (s *Settings) SaveAuthorizedGoogleDomains(w http.ResponseWriter, r *http.Request) {
	var req saveAuthorizedDomainsRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		response.BadRequest(w, "invalid JSON")
		return
	}
	if err := s.auth.SaveAuthorizedGoogleDomains(req.Domains); err != nil {
		s.audit.Record(r.Context(), audit.Event{
			Action:   "settings.oauth.google_domains.update",
			Details:  map[string]any{"domains": req.Domains},
			Success:  false,
			ErrorMsg: err.Error(),
		})
		response.InternalError(w, "failed to save authorized domains")
		return
	}
	s.audit.Record(r.Context(), audit.Event{
		Action:  "settings.oauth.google_domains.update",
		Details: map[string]any{"domains": req.Domains},
		Success: true,
	})
	s.GetAuthorizedGoogleDomains(w, r)
}

type authorizedOrgsView struct {
	Orgs []string `json:"orgs"`
}

type saveAuthorizedOrgsRequest struct {
	Orgs []string `json:"orgs"`
}

// GetAuthorizedGitHubOrgs handles GET /api/settings/github-orgs (admin only).
//
// @Summary     List authorized GitHub orgs (admin)
// @Description Empty list means any GitHub account may sign in.
// @Tags        settings
// @Security    cookieAuth
// @Produce     json
// @Success     200 {object} EnvelopeOK{data=authorizedOrgsView}
// @Failure     401 {object} EnvelopeError
// @Failure     403 {object} EnvelopeError
// @Failure     500 {object} EnvelopeError
// @Router      /settings/github-orgs [get]
func (s *Settings) GetAuthorizedGitHubOrgs(w http.ResponseWriter, r *http.Request) {
	settings, err := s.auth.GetOAuthSettings()
	if err != nil {
		response.InternalError(w, "failed to load authorized orgs")
		return
	}
	orgs := []string{}
	for _, o := range strings.Split(settings.AuthorizedGitHubOrgs, ",") {
		o = strings.TrimSpace(o)
		if o != "" {
			orgs = append(orgs, o)
		}
	}
	response.Success(w, authorizedOrgsView{Orgs: orgs})
}

// SaveAuthorizedGitHubOrgs handles PUT /api/settings/github-orgs (admin only).
//
// @Summary     Replace the authorized GitHub orgs (admin)
// @Tags        settings
// @Security    cookieAuth
// @Accept      json
// @Produce     json
// @Param       body body     saveAuthorizedOrgsRequest true "Org list"
// @Success     200  {object} EnvelopeOK{data=authorizedOrgsView}
// @Failure     400  {object} EnvelopeError
// @Failure     401  {object} EnvelopeError
// @Failure     403  {object} EnvelopeError
// @Failure     500  {object} EnvelopeError
// @Router      /settings/github-orgs [put]
func (s *Settings) SaveAuthorizedGitHubOrgs(w http.ResponseWriter, r *http.Request) {
	var req saveAuthorizedOrgsRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		response.BadRequest(w, "invalid JSON")
		return
	}
	if err := s.auth.SaveAuthorizedGitHubOrgs(req.Orgs); err != nil {
		s.audit.Record(r.Context(), audit.Event{
			Action:   "settings.oauth.github_orgs.update",
			Details:  map[string]any{"orgs": req.Orgs},
			Success:  false,
			ErrorMsg: err.Error(),
		})
		response.InternalError(w, "failed to save authorized orgs")
		return
	}
	s.audit.Record(r.Context(), audit.Event{
		Action:  "settings.oauth.github_orgs.update",
		Details: map[string]any{"orgs": req.Orgs},
		Success: true,
	})
	s.GetAuthorizedGitHubOrgs(w, r)
}

type gopherSettingsView struct {
	APIURL string `json:"api_url"`
	// CloudSubdomain is the *effective* leftmost label of the public URL —
	// empty in the DB collapses to selftunnel.DefaultCloudSubdomain ("cloud")
	// here so the UI never has to guess the fallback.
	CloudSubdomain string `json:"cloud_subdomain"`
	Configured     bool   `json:"configured"`
}

// GetGopher handles GET /api/settings/gopher (admin only). The API key is
// never returned; the SPA only needs to know whether it's set.
//
// @Summary     Read Gopher tunnel-gateway settings (admin)
// @Tags        settings
// @Security    cookieAuth
// @Produce     json
// @Success     200 {object} EnvelopeOK{data=gopherSettingsView}
// @Failure     401 {object} EnvelopeError
// @Failure     403 {object} EnvelopeError
// @Failure     500 {object} EnvelopeError
// @Router      /settings/gopher [get]
func (s *Settings) GetGopher(w http.ResponseWriter, _ *http.Request) {
	settings, err := s.auth.GetGopherSettings()
	if err != nil {
		response.InternalError(w, "failed to load Gopher settings")
		return
	}
	response.Success(w, gopherSettingsView{
		APIURL:         settings.APIURL,
		CloudSubdomain: selftunnel.EffectiveCloudSubdomain(settings.CloudSubdomain),
		Configured:     settings.APIURL != "" && settings.APIKey != "",
	})
}

type saveGopherRequest struct {
	APIURL string `json:"api_url"`
	APIKey string `json:"api_key"`
	// CloudSubdomain is the leftmost label of the public hostname Nimbus's
	// self-tunnel exposes the dashboard at. Empty preserves the existing
	// stored value (same rule as APIURL/APIKey). Validated as a DNS label
	// before persisting so a typo never reaches Gopher.
	CloudSubdomain string `json:"cloud_subdomain"`
}

// SaveGopher handles PUT /api/settings/gopher (admin only). Persists the
// values, then rebuilds the live tunnel.Client and pushes it to every
// registered applier (so provision flow + admin /tunnels endpoint pick up
// the new credentials with no restart). On clear (both fields blank),
// passes nil to disable.
//
// When cloud_subdomain changes from the previously-active value, the existing
// cloud tunnel is deleted on Gopher and the saved CloudTunnelID/URL are
// cleared so the next self-bootstrap recreates the tunnel under the new
// subdomain. The admin still has to re-register the OAuth redirect URI on
// any IdP that pinned the old hostname — the UI surfaces a confirm dialog
// before this fires.
//
// @Summary     Update Gopher credentials + cloud subdomain (admin)
// @Description Live-reloads the tunnel client across the running process —
// @Description no restart needed. Empty api_url + api_key disables tunnels
// @Description entirely. Changing cloud_subdomain tears down the existing
// @Description cloud tunnel.
// @Tags        settings
// @Security    cookieAuth
// @Accept      json
// @Produce     json
// @Param       body body     saveGopherRequest true "Gopher creds + subdomain"
// @Success     200  {object} EnvelopeOK{data=gopherSettingsView}
// @Failure     400  {object} EnvelopeError
// @Failure     401  {object} EnvelopeError
// @Failure     403  {object} EnvelopeError
// @Failure     500  {object} EnvelopeError
// @Router      /settings/gopher [put]
func (s *Settings) SaveGopher(w http.ResponseWriter, r *http.Request) {
	var req saveGopherRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		response.BadRequest(w, "invalid JSON")
		return
	}
	url := strings.TrimSpace(req.APIURL)
	key := strings.TrimSpace(req.APIKey)
	subdomain := strings.ToLower(strings.TrimSpace(req.CloudSubdomain))

	// Validate subdomain only when the caller supplied a non-empty value;
	// empty means "preserve existing" per the SaveGopherSettings contract.
	if subdomain != "" && !selftunnel.IsValidCloudSubdomain(subdomain) {
		response.BadRequest(w, "cloud_subdomain must be a DNS label: 1-63 chars, a-z/0-9/hyphen, no leading or trailing hyphen")
		return
	}

	// Snapshot the *effective* subdomain BEFORE the save so we can detect a
	// change and tear down the obsolete tunnel.
	prev, err := s.auth.GetGopherSettings()
	if err != nil {
		response.InternalError(w, "failed to load existing Gopher settings")
		return
	}
	prevEffective := selftunnel.EffectiveCloudSubdomain(prev.CloudSubdomain)
	nextEffective := prevEffective
	if subdomain != "" {
		nextEffective = subdomain
	}
	subdomainChanged := nextEffective != prevEffective

	if err := s.auth.SaveGopherSettings(db.GopherSettings{
		APIURL:         url,
		APIKey:         key,
		CloudSubdomain: subdomain,
	}); err != nil {
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
	if s.selfBootstrap != nil {
		s.selfBootstrap.SetGopherClient(client)
	}

	// Subdomain changed AND we still have a Gopher client: delete the old
	// tunnel + clear the saved tunnel state so the next bootstrap creates a
	// fresh one under the new hostname. Non-fatal — if Gopher rejects the
	// DELETE, the operator can clean it up manually; we still want the new
	// subdomain saved locally.
	if subdomainChanged && client != nil && prev.CloudTunnelID != "" {
		if err := client.DeleteTunnel(r.Context(), prev.CloudTunnelID); err != nil {
			log.Printf("save-gopher: failed to delete obsolete tunnel %s: %v", prev.CloudTunnelID, err)
		}
		// Clear the local pointer regardless — the old tunnel is no longer
		// the source of truth, even if Gopher still has the row.
		if err := s.auth.SaveCloudTunnelState(db.GopherSettings{
			CloudMachineID:      prev.CloudMachineID, // keep the machine; we just rebuild the tunnel
			CloudTunnelID:       "",
			CloudTunnelURL:      "",
			CloudBootstrapState: "",
			CloudBootstrapError: "",
		}); err != nil {
			log.Printf("save-gopher: failed to clear cloud tunnel state: %v", err)
		}
	}

	// Kick off the self-bootstrap when we now have a usable Gopher client.
	// Errors here are non-fatal for the save itself — the modal will poll
	// /self-bootstrap and surface whatever happens.
	if client != nil && s.selfBootstrap != nil {
		if err := s.selfBootstrap.Start(r.Context()); err != nil {
			// Log via the response — no separate logger threaded in here.
			// Non-fatal: the save itself still succeeded.
			_ = err
		}
	}

	s.audit.Record(r.Context(), audit.Event{
		Action: "settings.gopher.update",
		Details: map[string]any{
			"api_url":         settings.APIURL,
			"cloud_subdomain": selftunnel.EffectiveCloudSubdomain(settings.CloudSubdomain),
			"configured":      client != nil,
		},
		Success: true,
	})
	response.Success(w, gopherSettingsView{
		APIURL:         settings.APIURL,
		CloudSubdomain: selftunnel.EffectiveCloudSubdomain(settings.CloudSubdomain),
		Configured:     client != nil,
	})
}

// gpuSettingsView is what GET /api/settings/gpu returns. The worker token
// is never sent back to clients — operators don't see it; pairing produces
// a fresh one as part of /api/gpu/register.
type gpuSettingsView struct {
	Enabled        bool   `json:"enabled"`
	BaseURL        string `json:"base_url"`
	InferenceModel string `json:"inference_model"`
	Configured     bool   `json:"configured"`
	GX10Hostname   string `json:"gx10_hostname,omitempty"` // self-reported at pairing time
}

// GetGPU handles GET /api/settings/gpu (admin only).
//
// @Summary     Read GX10 GPU plane settings (admin)
// @Description The worker token is never sent — pairing produces a fresh one.
// @Tags        settings
// @Security    cookieAuth
// @Produce     json
// @Success     200 {object} EnvelopeOK{data=gpuSettingsView}
// @Failure     401 {object} EnvelopeError
// @Failure     403 {object} EnvelopeError
// @Failure     500 {object} EnvelopeError
// @Router      /settings/gpu [get]
func (s *Settings) GetGPU(w http.ResponseWriter, _ *http.Request) {
	settings, err := s.auth.GetGPUSettings()
	if err != nil {
		response.InternalError(w, "failed to load GPU settings")
		return
	}
	response.Success(w, gpuSettingsView{
		Enabled:        settings.Enabled,
		BaseURL:        settings.BaseURL,
		InferenceModel: settings.InferenceModel,
		Configured:     settings.WorkerToken != "" && settings.BaseURL != "",
		GX10Hostname:   settings.GX10Hostname,
	})
}

type saveGPURequest struct {
	Enabled        bool   `json:"enabled"`
	BaseURL        string `json:"base_url"`
	InferenceModel string `json:"inference_model"`
}

// SaveGPU handles PUT /api/settings/gpu (admin only).
//
// @Summary     Update GX10 GPU plane settings (admin)
// @Description Live-pushes the bootstrap config to provision.Service so
// @Description subsequent VM provisions inject the new env without restart.
// @Tags        settings
// @Security    cookieAuth
// @Accept      json
// @Produce     json
// @Param       body body     saveGPURequest true "GPU plane settings"
// @Success     200  {object} EnvelopeOK{data=gpuSettingsView}
// @Failure     400  {object} EnvelopeError
// @Failure     401  {object} EnvelopeError
// @Failure     403  {object} EnvelopeError
// @Failure     500  {object} EnvelopeError
// @Router      /settings/gpu [put]
func (s *Settings) SaveGPU(w http.ResponseWriter, r *http.Request) {
	var req saveGPURequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		response.BadRequest(w, "invalid JSON")
		return
	}
	if err := s.auth.SaveGPUSettings(db.GPUSettings{
		Enabled:        req.Enabled,
		BaseURL:        strings.TrimSpace(req.BaseURL),
		InferenceModel: strings.TrimSpace(req.InferenceModel),
	}); err != nil {
		response.InternalError(w, "failed to save GPU settings")
		return
	}
	settings, err := s.auth.GetGPUSettings()
	if err != nil {
		response.InternalError(w, "saved, but failed to reload: "+err.Error())
		return
	}

	// Push the fresh config to anything that holds a live copy
	// (provision.Service for cloud-init env injection). Disabled or empty
	// BaseURL means push a zero config — provision will then skip the
	// GPU bootstrap step on subsequent VMs.
	//
	// nimbusGPUAPI: prefer the request host (the admin's browser just
	// proved it's reachable) over s.nimbusAppURL — if APP_URL is misconfigured
	// (defaults to localhost:5173), this prevents poisoning the bootstrap
	// config with a URL guests can't reach.
	var bootstrapCfg provision.GPUBootstrapConfig
	if settings.Enabled && settings.BaseURL != "" {
		nimbusGPUAPI := strings.TrimRight(s.nimbusAppURL, "/") + "/api/gpu"
		if r.Host != "" && !looksLikeLocalhost(r.Host) {
			scheme := "http"
			if r.TLS != nil || r.Header.Get("X-Forwarded-Proto") == "https" {
				scheme = "https"
			}
			nimbusGPUAPI = scheme + "://" + r.Host + "/api/gpu"
		}
		bootstrapCfg = provision.GPUBootstrapConfig{
			BaseURL:        settings.BaseURL,
			NimbusGPUAPI:   nimbusGPUAPI,
			InferenceModel: settings.InferenceModel,
		}
	}
	for _, a := range s.gpuAppliers {
		a.SetGPUBootstrapConfig(bootstrapCfg)
	}

	s.audit.Record(r.Context(), audit.Event{
		Action: "settings.gpu.update",
		Details: map[string]any{
			"enabled":         settings.Enabled,
			"base_url":        settings.BaseURL,
			"inference_model": settings.InferenceModel,
		},
		Success: true,
	})
	response.Success(w, gpuSettingsView{
		Enabled:        settings.Enabled,
		BaseURL:        settings.BaseURL,
		InferenceModel: settings.InferenceModel,
		Configured:     settings.WorkerToken != "" && settings.BaseURL != "",
		GX10Hostname:   settings.GX10Hostname,
	})
}

// looksLikeLocalhost reports whether the bare host (no scheme, may include
// port) refers to a loopback address — those are unreachable from VMs and
// from the GX10, so callers should fall back to AppURL or refuse to bake
// the host into per-VM bootstraps. Mirrors the URL-shaped helper in
// provision/gpu_bootstrap.go but takes a "host:port" instead of a URL.
func looksLikeLocalhost(host string) bool {
	if host == "" {
		return true
	}
	h := strings.ToLower(host)
	if i := strings.LastIndex(h, ":"); i >= 0 && !strings.Contains(h[:i], ":") {
		h = h[:i]
	}
	return h == "localhost" ||
		strings.HasPrefix(h, "127.") ||
		h == "0.0.0.0" ||
		h == "::1" ||
		h == "[::1]"
}

// (Worker-token regeneration is gone — operators don't see the token.
// Re-pairing via Settings → Add GX10 produces a fresh worker token as
// part of the handshake.)

type selfBootstrapStatusView struct {
	State     string `json:"state"`
	Error     string `json:"error,omitempty"`
	TunnelURL string `json:"tunnel_url,omitempty"`
}

// SelfBootstrapStatus handles GET /api/settings/gopher/self-bootstrap —
// the Settings modal polls this to render the phase indicator.
//
// @Summary     Read the Gopher self-bootstrap state (admin)
// @Tags        settings
// @Security    cookieAuth
// @Produce     json
// @Success     200 {object} EnvelopeOK{data=selfBootstrapStatusView}
// @Failure     401 {object} EnvelopeError
// @Failure     403 {object} EnvelopeError
// @Failure     500 {object} EnvelopeError
// @Router      /settings/gopher/self-bootstrap [get]
func (s *Settings) SelfBootstrapStatus(w http.ResponseWriter, _ *http.Request) {
	if s.selfBootstrap == nil {
		response.Success(w, selfBootstrapStatusView{State: ""})
		return
	}
	state, err := s.selfBootstrap.Status()
	if err != nil {
		response.InternalError(w, "failed to load bootstrap status")
		return
	}
	response.Success(w, selfBootstrapStatusView{
		State:     state.CloudBootstrapState,
		Error:     state.CloudBootstrapError,
		TunnelURL: state.CloudTunnelURL,
	})
}

// SelfBootstrapStart handles POST /api/settings/gopher/self-bootstrap —
// retry hook for the modal's "Try again" button after a failure. SaveGopher
// invokes this path automatically; this endpoint exists for explicit retry.
//
// @Summary     Retry the Gopher self-bootstrap (admin)
// @Tags        settings
// @Security    cookieAuth
// @Produce     json
// @Success     200 {object} EnvelopeOK
// @Failure     400 {object} EnvelopeError
// @Failure     401 {object} EnvelopeError
// @Failure     403 {object} EnvelopeError
// @Router      /settings/gopher/self-bootstrap [post]
func (s *Settings) SelfBootstrapStart(w http.ResponseWriter, r *http.Request) {
	if s.selfBootstrap == nil {
		response.BadRequest(w, "self-bootstrap requires Gopher to be configured")
		return
	}
	if err := s.selfBootstrap.Start(r.Context()); err != nil {
		response.BadRequest(w, err.Error())
		return
	}
	response.Success(w, map[string]string{"state": "started"})
}

// networkSettingsView is what GET / PUT /api/settings/network return.
type networkSettingsView struct {
	IPPoolStart string `json:"ip_pool_start"`
	IPPoolEnd   string `json:"ip_pool_end"`
	GatewayIP   string `json:"gateway_ip"`
	PrefixLen   int    `json:"prefix_len"`
}

// GetNetwork handles GET /api/settings/network (admin only). Returns the live
// IP pool range, gateway, and cloud-init prefix length. Used by the Settings
// → Network panel and as the canonical source for the renumber /
// force-gateway confirmation modals.
//
// @Summary     Read live network settings (admin)
// @Tags        settings
// @Security    cookieAuth
// @Produce     json
// @Success     200 {object} EnvelopeOK{data=networkSettingsView}
// @Failure     401 {object} EnvelopeError
// @Failure     403 {object} EnvelopeError
// @Failure     500 {object} EnvelopeError
// @Router      /settings/network [get]
func (s *Settings) GetNetwork(w http.ResponseWriter, _ *http.Request) {
	settings, err := s.auth.GetNetworkSettings()
	if err != nil {
		response.InternalError(w, "failed to load network settings")
		return
	}
	response.Success(w, networkSettingsView{
		IPPoolStart: settings.IPPoolStart,
		IPPoolEnd:   settings.IPPoolEnd,
		GatewayIP:   settings.GatewayIP,
		PrefixLen:   settings.PrefixLen,
	})
}

type saveNetworkRequest struct {
	IPPoolStart string `json:"ip_pool_start"`
	IPPoolEnd   string `json:"ip_pool_end"`
	GatewayIP   string `json:"gateway_ip"`
	PrefixLen   int    `json:"prefix_len"`
}

// SaveNetwork handles PUT /api/settings/network (admin only). Persists the
// supplied values, then reseeds the IP pool to converge to the new range and
// pushes the live gateway + prefix to every registered NetworkApplier.
// Existing VMs are NOT touched — that requires the explicit RenumberVMs /
// ForceGatewayUpdate endpoints.
//
// @Summary     Update network settings (admin)
// @Description Reseeds the IP pool and pushes the new gateway/prefix to live
// @Description provision config. Existing VMs are not touched — use the
// @Description renumber / force-gateway endpoints for that.
// @Tags        settings
// @Security    cookieAuth
// @Accept      json
// @Produce     json
// @Param       body body     saveNetworkRequest true "Network settings"
// @Success     200  {object} EnvelopeOK{data=networkSettingsView}
// @Failure     400  {object} EnvelopeError
// @Failure     401  {object} EnvelopeError
// @Failure     403  {object} EnvelopeError
// @Failure     500  {object} EnvelopeError
// @Router      /settings/network [put]
func (s *Settings) SaveNetwork(w http.ResponseWriter, r *http.Request) {
	var req saveNetworkRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		response.BadRequest(w, "invalid JSON")
		return
	}
	start := strings.TrimSpace(req.IPPoolStart)
	end := strings.TrimSpace(req.IPPoolEnd)
	gw := strings.TrimSpace(req.GatewayIP)

	for label, v := range map[string]string{"ip_pool_start": start, "ip_pool_end": end, "gateway_ip": gw} {
		if v == "" {
			continue // empty = preserve existing
		}
		if ip := net.ParseIP(v); ip == nil || ip.To4() == nil {
			response.BadRequest(w, label+" is not a valid IPv4 address")
			return
		}
	}
	// Prefix: 0 = preserve existing (caller didn't include the field).
	// Otherwise must be a sane IPv4 mask length. /1..32 covers everything;
	// callers that want /0 should be loudly told no.
	if req.PrefixLen != 0 && (req.PrefixLen < 1 || req.PrefixLen > 32) {
		response.BadRequest(w, "prefix_len must be between 1 and 32")
		return
	}

	if err := s.auth.SaveNetworkSettings(db.NetworkSettings{
		IPPoolStart: start,
		IPPoolEnd:   end,
		GatewayIP:   gw,
		PrefixLen:   req.PrefixLen,
	}); err != nil {
		response.InternalError(w, "failed to save network settings")
		return
	}

	settings, err := s.auth.GetNetworkSettings()
	if err != nil {
		response.InternalError(w, "saved, but failed to reload: "+err.Error())
		return
	}

	if s.pool != nil && settings.IPPoolStart != "" && settings.IPPoolEnd != "" {
		if _, _, _, err := s.pool.Reseed(r.Context(), settings.IPPoolStart, settings.IPPoolEnd); err != nil {
			response.BadRequest(w, "saved, but pool reseed failed: "+err.Error())
			return
		}
	}
	for _, a := range s.networkAppliers {
		a.SetGatewayIP(settings.GatewayIP)
		a.SetPrefixLen(settings.PrefixLen)
	}

	s.audit.Record(r.Context(), audit.Event{
		Action: "settings.network.update",
		Details: map[string]any{
			"ip_pool_start": settings.IPPoolStart,
			"ip_pool_end":   settings.IPPoolEnd,
			"gateway_ip":    settings.GatewayIP,
			"prefix_len":    settings.PrefixLen,
		},
		Success: true,
	})
	response.Success(w, networkSettingsView{
		IPPoolStart: settings.IPPoolStart,
		IPPoolEnd:   settings.IPPoolEnd,
		GatewayIP:   settings.GatewayIP,
		PrefixLen:   settings.PrefixLen,
	})
}

// networkOpResponse is the JSON shape returned by the renumber / force-gateway
// endpoints — Updated count + per-VM failures the UI can surface.
type networkOpResponse struct {
	Updated  int                    `json:"updated"`
	Failures []networkOpFailureView `json:"failures"`
}

type networkOpFailureView struct {
	VMRowID  uint   `json:"vm_row_id"`
	VMID     int    `json:"vmid"`
	Hostname string `json:"hostname"`
	Error    string `json:"error"`
}

func toNetworkOpResponse(rep provision.NetworkOpReport) networkOpResponse {
	out := networkOpResponse{
		Updated:  rep.Updated,
		Failures: make([]networkOpFailureView, 0, len(rep.Failures)),
	}
	for _, f := range rep.Failures {
		out.Failures = append(out.Failures, networkOpFailureView{
			VMRowID:  f.VMRowID,
			VMID:     f.VMID,
			Hostname: f.Hostname,
			Error:    f.Err,
		})
	}
	return out
}

// ForceGatewayUpdate handles POST /api/settings/network/force-gateway-update
// (admin only). Pushes the currently-saved gateway to every managed VM via
// `qm set --ipconfig0` and reboots running VMs so the change takes effect.
// Disruptive — every running VM bounces.
//
// @Summary     Force-push the saved gateway to every managed VM (admin)
// @Description Disruptive — every running VM bounces. Each per-VM failure
// @Description is reported in the response; the loop doesn't abort.
// @Tags        settings
// @Security    cookieAuth
// @Produce     json
// @Success     200 {object} EnvelopeOK{data=networkOpResponse}
// @Failure     400 {object} EnvelopeError
// @Failure     401 {object} EnvelopeError
// @Failure     403 {object} EnvelopeError
// @Failure     500 {object} EnvelopeError
// @Router      /settings/network/force-gateway-update [post]
func (s *Settings) ForceGatewayUpdate(w http.ResponseWriter, r *http.Request) {
	if s.networkOps == nil {
		response.InternalError(w, "network ops not wired")
		return
	}
	settings, err := s.auth.GetNetworkSettings()
	if err != nil {
		response.InternalError(w, "failed to load network settings")
		return
	}
	if settings.GatewayIP == "" {
		response.BadRequest(w, "gateway_ip is not set; save network settings first")
		return
	}
	rep, err := s.networkOps.ForceGatewayUpdate(r.Context(), settings.GatewayIP)
	if err != nil {
		s.audit.Record(r.Context(), audit.Event{
			Action:   "settings.network.force_gateway",
			Details:  map[string]any{"gateway_ip": settings.GatewayIP},
			Success:  false,
			ErrorMsg: err.Error(),
		})
		response.InternalError(w, err.Error())
		return
	}
	s.audit.Record(r.Context(), audit.Event{
		Action: "settings.network.force_gateway",
		Details: map[string]any{
			"gateway_ip": settings.GatewayIP,
			"updated":    rep.Updated,
			"failures":   len(rep.Failures),
		},
		Success: true,
	})
	response.Success(w, toNetworkOpResponse(rep))
}

// RenumberVMs handles POST /api/settings/network/renumber-vms (admin only).
// Reserves a fresh IP from the saved pool for every managed VM, updates each
// VM's cloud-init, reboots it, and releases the old IP. Disruptive.
//
// Refuses with 400 if the pool has fewer free addresses than the number of
// VMs to renumber — operator must widen the pool first.
//
// @Summary     Renumber every managed VM into the saved pool (admin)
// @Description Disruptive — every VM gets a fresh IP and bounces. 400 when
// @Description the pool has fewer free addresses than VMs to renumber.
// @Tags        settings
// @Security    cookieAuth
// @Produce     json
// @Success     200 {object} EnvelopeOK{data=networkOpResponse}
// @Failure     400 {object} EnvelopeError
// @Failure     401 {object} EnvelopeError
// @Failure     403 {object} EnvelopeError
// @Failure     500 {object} EnvelopeError
// @Router      /settings/network/renumber-vms [post]
func (s *Settings) RenumberVMs(w http.ResponseWriter, r *http.Request) {
	if s.networkOps == nil {
		response.InternalError(w, "network ops not wired")
		return
	}
	settings, err := s.auth.GetNetworkSettings()
	if err != nil {
		response.InternalError(w, "failed to load network settings")
		return
	}
	if settings.GatewayIP == "" || settings.IPPoolStart == "" || settings.IPPoolEnd == "" {
		response.BadRequest(w, "network settings (gateway_ip + ip_pool_start + ip_pool_end) must all be saved before renumbering")
		return
	}
	rep, err := s.networkOps.RenumberAllVMs(r.Context(), settings.GatewayIP, settings.IPPoolStart, settings.IPPoolEnd)
	if err != nil {
		s.audit.Record(r.Context(), audit.Event{
			Action:   "settings.network.renumber_vms",
			Success:  false,
			ErrorMsg: err.Error(),
		})
		response.BadRequest(w, err.Error())
		return
	}
	s.audit.Record(r.Context(), audit.Event{
		Action: "settings.network.renumber_vms",
		Details: map[string]any{
			"updated":  rep.Updated,
			"failures": len(rep.Failures),
		},
		Success: true,
	})
	response.Success(w, toNetworkOpResponse(rep))
}

// RegenerateAccessCode handles POST /api/settings/access-code/regenerate (admin only).
// Issues a fresh code and bumps the version, invalidating every non-admin
// user's prior verification.
//
// @Summary     Rotate the access code (admin)
// @Description Bumps the version, invalidating every non-admin's prior
// @Description verification. They'll be prompted to re-enter the new code
// @Description on next access.
// @Tags        settings
// @Security    cookieAuth
// @Produce     json
// @Success     200 {object} EnvelopeOK{data=accessCodeView}
// @Failure     401 {object} EnvelopeError
// @Failure     403 {object} EnvelopeError
// @Failure     500 {object} EnvelopeError
// @Router      /settings/access-code/regenerate [post]
func (s *Settings) RegenerateAccessCode(w http.ResponseWriter, r *http.Request) {
	settings, err := s.auth.RegenerateAccessCode()
	if err != nil {
		s.audit.Record(r.Context(), audit.Event{
			Action:   "settings.access_code.regenerate",
			Success:  false,
			ErrorMsg: err.Error(),
		})
		response.InternalError(w, "failed to regenerate access code")
		return
	}
	// Don't log the code itself — just the version bump. The code is
	// shown to the admin in the response; that's the only place it
	// should appear.
	s.audit.Record(r.Context(), audit.Event{
		Action:  "settings.access_code.regenerate",
		Details: map[string]any{"version": settings.AccessCodeVersion},
		Success: true,
	})
	response.Success(w, accessCodeView{
		AccessCode: settings.AccessCode,
		Version:    settings.AccessCodeVersion,
	})
}

// sdnSettingsView is the admin-facing read shape — combines the
// stored config with live zone status from Proxmox so the admin UI can
// render the toggle + diagnostic panel in one round trip. The status
// is best-effort: a Proxmox blip surfaces as zone_status="error" with
// proxmox_error populated, but the rest of the view still rendered.
type sdnSettingsView struct {
	Enabled      bool   `json:"enabled"`
	ZoneName     string `json:"zone_name"`
	ZoneType     string `json:"zone_type"`
	Supernet     string `json:"supernet"`
	SubnetSize   int    `json:"subnet_size"`
	DNSServer    string `json:"dns_server,omitempty"`
	ZoneStatus   string `json:"zone_status"`
	VNetCount    int    `json:"vnet_count"`
	ProxmoxError string `json:"proxmox_error,omitempty"`
}

// saveSDNRequest is the body of PUT /api/settings/sdn. SubnetSize is
// optional — empty/0 preserves existing. DNSServer empty = clear.
type saveSDNRequest struct {
	Enabled    bool   `json:"enabled"`
	ZoneName   string `json:"zone_name,omitempty"`
	ZoneType   string `json:"zone_type,omitempty"`
	Supernet   string `json:"supernet,omitempty"`
	SubnetSize int    `json:"subnet_size,omitempty"`
	DNSServer  string `json:"dns_server"`
}

// publicSDNStatusView is the slim shape returned by GET
// /api/sdn/status — accessible to every verified user (not just
// admin). Drives the Provision form's subnet picker: when Enabled is
// false the picker collapses to a single greyed "Cluster LAN" tile;
// when true, members see only the subnet picker while admins keep a
// "Cluster LAN (admin)" escape hatch.
type publicSDNStatusView struct {
	Enabled bool `json:"enabled"`
	// DefaultBridge names the cluster bridge non-SDN VMs land on
	// (today: always "vmbr0"). Surfaced so the UI can label the
	// admin escape hatch with the operator-visible name rather than
	// hard-coding "vmbr0".
	DefaultBridge string `json:"default_bridge"`
}

// PublicSDNStatus handles GET /api/sdn/status — verified-user-readable.
// Returns the bare-minimum the Provision form needs: is SDN on, and
// what's the bridge name when it isn't. Admin-specific details
// (zone status, vnet count, proxmox errors) live on /settings/sdn.
//
// @Summary     Public SDN enablement status
// @Description Tells the Provision form whether per-user isolation is
// @Description on cluster-wide. Members see only their own subnets in
// @Description the picker; admins keep a `vmbr0` escape hatch.
// @Tags        sdn
// @Security    cookieAuth
// @Produce     json
// @Success     200 {object} EnvelopeOK{data=publicSDNStatusView}
// @Failure     401 {object} EnvelopeError
// @Failure     500 {object} EnvelopeError
// @Router      /sdn/status [get]
func (s *Settings) PublicSDNStatus(w http.ResponseWriter, r *http.Request) {
	settings, err := s.auth.GetNetworkSettings()
	if err != nil {
		response.InternalError(w, "failed to load settings: "+err.Error())
		return
	}
	response.Success(w, publicSDNStatusView{
		Enabled:       settings.SDNEnabled,
		DefaultBridge: "vmbr0",
	})
}

// GetSDN handles GET /api/settings/sdn (admin only). Returns the
// stored SDN config plus live zone status from Proxmox.
//
// @Summary     Read SDN settings + live zone status (admin)
// @Description Combines the stored config with a Proxmox round-trip
// @Description that reports whether the zone exists and is applied.
// @Description ProxmoxError is populated (non-fatal) when the SDN
// @Description package isn't installed cluster-wide.
// @Tags        settings
// @Security    cookieAuth
// @Produce     json
// @Success     200 {object} EnvelopeOK{data=sdnSettingsView}
// @Failure     401 {object} EnvelopeError
// @Failure     403 {object} EnvelopeError
// @Failure     500 {object} EnvelopeError
// @Router      /settings/sdn [get]
func (s *Settings) GetSDN(w http.ResponseWriter, r *http.Request) {
	if s.sdn == nil {
		response.InternalError(w, "SDN bootstrapper not wired")
		return
	}
	st, err := s.sdn.Status(r.Context())
	if err != nil {
		response.InternalError(w, "failed to load sdn status: "+err.Error())
		return
	}
	response.Success(w, sdnSettingsView{
		Enabled:      st.Enabled,
		ZoneName:     st.ZoneName,
		ZoneType:     st.ZoneType,
		Supernet:     st.Supernet,
		SubnetSize:   st.SubnetSize,
		DNSServer:    st.DNSServer,
		ZoneStatus:   st.ZoneStatus,
		VNetCount:    st.VNetCount,
		ProxmoxError: st.ProxmoxError,
	})
}

// SaveSDN handles PUT /api/settings/sdn (admin only). Persists the
// SDN config, then on a false→true transition (or while enabled and
// zone-config changed) kicks a fresh Bootstrap so the zone lands in
// Proxmox without a restart. Bootstrap failures surface as 502 so the
// admin sees them — DB save already happened, so a retry just needs
// to fix Proxmox-side state (install the SDN package, etc.).
//
// @Summary     Update SDN settings (admin)
// @Description Persists config and runs zone bootstrap on enable.
// @Description Returns the live status view with the post-save state
// @Description so the admin UI can confirm "zone is active" in one
// @Description round trip.
// @Tags        settings
// @Security    cookieAuth
// @Accept      json
// @Produce     json
// @Param       body body     saveSDNRequest true "SDN settings"
// @Success     200  {object} EnvelopeOK{data=sdnSettingsView}
// @Failure     400  {object} EnvelopeError
// @Failure     401  {object} EnvelopeError
// @Failure     403  {object} EnvelopeError
// @Failure     500  {object} EnvelopeError
// @Failure     502  {object} EnvelopeError
// @Router      /settings/sdn [put]
func (s *Settings) SaveSDN(w http.ResponseWriter, r *http.Request) {
	if s.sdn == nil {
		response.InternalError(w, "SDN bootstrapper not wired")
		return
	}
	var req saveSDNRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		response.BadRequest(w, "invalid JSON")
		return
	}
	zone := strings.TrimSpace(req.ZoneName)
	zoneType := strings.TrimSpace(req.ZoneType)
	supernet := strings.TrimSpace(req.Supernet)

	// Light validation. Heavy validation (does Proxmox accept this
	// supernet?) lands in Bootstrap — it'll surface upstream errors
	// verbatim so admins see the actual cause.
	if req.Enabled && zone == "" {
		// Read existing to allow a save that just flips the toggle.
		existing, err := s.auth.GetNetworkSettings()
		if err != nil {
			response.InternalError(w, "load existing settings: "+err.Error())
			return
		}
		if existing.SDNZoneName == "" {
			response.BadRequest(w, "zone_name is required to enable SDN")
			return
		}
	}
	if zoneType != "" && zoneType != "simple" {
		// VXLAN lands in P4. Hard-reject anything else for now so an
		// admin doesn't silently misconfigure.
		response.BadRequest(w, "zone_type must be 'simple' (VXLAN support is planned)")
		return
	}
	if supernet != "" {
		if _, _, err := net.ParseCIDR(supernet); err != nil {
			response.BadRequest(w, "supernet must be a valid CIDR (e.g. 10.42.0.0/16): "+err.Error())
			return
		}
	}
	if req.SubnetSize != 0 && (req.SubnetSize < 16 || req.SubnetSize > 30) {
		response.BadRequest(w, "subnet_size must be between /16 and /30")
		return
	}

	if err := s.auth.SaveSDNSettings(db.NetworkSettings{
		SDNEnabled:        req.Enabled,
		SDNZoneName:       zone,
		SDNZoneType:       zoneType,
		SDNSubnetSupernet: supernet,
		SDNSubnetSize:     req.SubnetSize,
		SDNDNSServer:      req.DNSServer,
	}); err != nil {
		response.InternalError(w, "save sdn settings: "+err.Error())
		return
	}

	// Always run Bootstrap when enabled — idempotent, and catches the
	// case where a previous save persisted but the Bootstrap call
	// failed (e.g. Proxmox was momentarily down).
	if req.Enabled {
		ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
		defer cancel()
		if err := s.sdn.Bootstrap(ctx); err != nil {
			// 502 — config was saved, but the Proxmox-side apply
			// didn't land. Admin can retry without re-saving.
			log.Printf("save sdn: bootstrap failed: %v", err)
			response.Error(w, http.StatusBadGateway, "settings saved, but proxmox bootstrap failed: "+err.Error())
			return
		}
	}

	// Re-read the live status so the response reflects what just landed.
	st, err := s.sdn.Status(r.Context())
	if err != nil {
		response.InternalError(w, "saved, but failed to reload status: "+err.Error())
		return
	}
	response.Success(w, sdnSettingsView{
		Enabled:      st.Enabled,
		ZoneName:     st.ZoneName,
		ZoneType:     st.ZoneType,
		Supernet:     st.Supernet,
		SubnetSize:   st.SubnetSize,
		DNSServer:    st.DNSServer,
		ZoneStatus:   st.ZoneStatus,
		VNetCount:    st.VNetCount,
		ProxmoxError: st.ProxmoxError,
	})
}
