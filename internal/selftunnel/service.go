// Package selftunnel orchestrates Nimbus's self-bootstrap through Gopher.
//
// When the admin saves Gopher credentials, this service:
//
//  1. Registers the Nimbus host with Gopher (POST /api/v1/machines).
//  2. Runs Gopher's bootstrap script locally (curl <url> | bash) so the
//     host installs the rathole client and connects back to the gateway.
//  3. Polls Gopher until the machine flips to "connected".
//  4. Creates a tunnel exposing Nimbus's HTTP port at cloud.<domain>
//     (POST /api/v1/tunnels {subdomain: "cloud", target_port: <port>}).
//  5. Persists the resulting URL on db.GopherSettings so the dashboard
//     can show a banner + redirect users from <ip>:<port> to the public
//     hostname.
//
// The flow runs in a goroutine fired from SaveGopher; the Settings page
// polls /api/settings/gopher/self-bootstrap to render the modal.
package selftunnel

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"sync"
	"time"

	"nimbus/internal/db"
	"nimbus/internal/tunnel"
)

// DefaultCloudSubdomain is the leftmost label the self-tunnel falls back to
// when db.GopherSettings.CloudSubdomain is empty. Two Nimbus instances
// pointed at the same Gopher (e.g. dev + prod) can override this via the
// Settings → Gopher panel so each claims a unique public hostname.
const DefaultCloudSubdomain = "cloud"

// EffectiveCloudSubdomain returns the configured subdomain, or
// DefaultCloudSubdomain when the configured value is empty. Centralised so
// every reader (selftunnel runBootstrap, the SaveGopher tear-down branch,
// the API view) collapses empty-vs-default the same way.
func EffectiveCloudSubdomain(configured string) string {
	if s := strings.TrimSpace(configured); s != "" {
		return s
	}
	return DefaultCloudSubdomain
}

// IsValidCloudSubdomain reports whether s is a single DNS label suitable for
// the leftmost component of a hostname. Lowercased a-z + digits + hyphens,
// 1-63 chars, no leading/trailing hyphen. Mirrors RFC 1035 §2.3.1.
func IsValidCloudSubdomain(s string) bool {
	if len(s) < 1 || len(s) > 63 {
		return false
	}
	if s[0] == '-' || s[len(s)-1] == '-' {
		return false
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		if (c < 'a' || c > 'z') && (c < '0' || c > '9') && c != '-' {
			return false
		}
	}
	return true
}

const (
	StateIdle           = ""
	StateRegistering    = "registering"
	StateInstalling     = "installing"
	StateWaitingConnect = "waiting_connect"
	StateCreatingTunnel = "creating_tunnel"
	StateActive         = "active"
	StateFailed         = "failed"
)

// machineConnectTimeout caps how long we wait for the rathole client to
// register with Gopher after the bootstrap script completes. The bootstrap
// itself takes 30-90s; the rathole-client systemd unit comes up within
// seconds of the script finishing, so 90s is generous.
const machineConnectTimeout = 90 * time.Second

// settingsStore is the small slice of service.AuthService we depend on.
// Defined here per the "accept interfaces" idiom so the package can be
// tested without a real GORM DB.
type settingsStore interface {
	GetGopherSettings() (*db.GopherSettings, error)
	SaveCloudTunnelState(state db.GopherSettings) error
	WipeGopherSettings() error
}

// gopherClient is the slice of tunnel.Client we need.
type gopherClient interface {
	CreateMachine(ctx context.Context, req tunnel.CreateMachineRequest) (*tunnel.Machine, error)
	GetMachine(ctx context.Context, id string) (*tunnel.Machine, error)
	DeleteMachine(ctx context.Context, id string) error
	CreateTunnel(ctx context.Context, req tunnel.CreateTunnelRequest) (*tunnel.Tunnel, error)
	DeleteTunnel(ctx context.Context, id string) error
	ListTunnels(ctx context.Context) ([]tunnel.Tunnel, error)
	ListTunnelsForMachine(ctx context.Context, machineID string) ([]tunnel.Tunnel, error)
}

// commandRunner runs a shell command. Real callers use exec.CommandContext;
// tests can stub. The bootstrap script needs root or passwordless sudo for
// the apt-install + systemd-unit steps.
type commandRunner func(ctx context.Context, name string, args ...string) ([]byte, error)

func realRun(ctx context.Context, name string, args ...string) ([]byte, error) {
	return exec.CommandContext(ctx, name, args...).CombinedOutput()
}

// Service orchestrates the self-bootstrap. Safe for concurrent use; only
// one bootstrap can be in flight at a time (guarded by inflight).
type Service struct {
	store      settingsStore
	gopher     gopherClient
	nimbusPort int
	run        commandRunner

	mu       sync.Mutex
	inflight bool
}

// New constructs a Service. nimbusPort is the HTTP port Nimbus listens on
// (target_port for the self-tunnel). gopher may be nil — the Service
// silently no-ops when tunnels aren't configured.
func New(store settingsStore, gopher gopherClient, nimbusPort int) *Service {
	return &Service{
		store:      store,
		gopher:     gopher,
		nimbusPort: nimbusPort,
		run:        realRun,
	}
}

// SetGopherClient replaces the client. Mirrors the live-reload pattern
// used elsewhere — when admin rotates Gopher creds, main.go rebuilds the
// tunnel.Client and pushes it here so the next Start uses fresh creds.
// Accepts a concrete *tunnel.Client (rather than the gopherClient
// interface) so handlers.SelfBootstrap can declare the method with a
// concrete parameter type and still match this implementation.
func (s *Service) SetGopherClient(c *tunnel.Client) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if c == nil {
		s.gopher = nil
		return
	}
	s.gopher = c
}

// Status returns the current self-bootstrap state from the DB. Used by
// the Settings page to poll the modal.
func (s *Service) Status() (db.GopherSettings, error) {
	settings, err := s.store.GetGopherSettings()
	if err != nil {
		return db.GopherSettings{}, err
	}
	return *settings, nil
}

// Start kicks off (or restarts) the self-bootstrap in the background.
// Returns immediately; callers poll Status to track progress.
//
// Idempotent on the steady state: if CloudTunnelURL is already populated
// and the tunnel is still active, this is a no-op. If a previous attempt
// failed mid-flight, Start tears down whatever was created and tries again.
func (s *Service) Start(ctx context.Context) error {
	s.mu.Lock()
	if s.inflight {
		s.mu.Unlock()
		return errors.New("self-bootstrap already in progress")
	}
	if s.gopher == nil {
		s.mu.Unlock()
		return errors.New("gopher tunnel integration is not configured")
	}
	s.inflight = true
	s.mu.Unlock()

	// Long-running: detach. The HTTP handler returns 202 Accepted and the
	// modal polls Status. We use a fresh context so the request lifetime
	// doesn't kill the bootstrap mid-flight.
	go func() {
		bgCtx, cancel := context.WithTimeout(context.Background(), 6*time.Minute)
		defer cancel()
		defer s.clearInflight()
		if err := s.runBootstrap(bgCtx); err != nil {
			log.Printf("self-bootstrap failed: %v", err)
			s.markFailed(err)
		}
	}()
	return nil
}

func (s *Service) clearInflight() {
	s.mu.Lock()
	s.inflight = false
	s.mu.Unlock()
}

// runBootstrap is the actual state machine. Each step writes its phase
// to the DB so the Settings modal sees progress.
func (s *Service) runBootstrap(ctx context.Context) error {
	// Read the configured cloud subdomain up front so the whole bootstrap
	// uses one consistent value. Empty falls back to DefaultCloudSubdomain.
	current, err := s.store.GetGopherSettings()
	if err != nil {
		return fmt.Errorf("load settings: %w", err)
	}
	subdomain := EffectiveCloudSubdomain(current.CloudSubdomain)

	// Step 1 — register the Nimbus host as a Gopher machine.
	if err := s.persist(db.GopherSettings{CloudBootstrapState: StateRegistering}); err != nil {
		return fmt.Errorf("persist state: %w", err)
	}
	machine, err := s.gopher.CreateMachine(ctx, tunnel.CreateMachineRequest{PublicSSH: false})
	if err != nil {
		return fmt.Errorf("register host: %w", err)
	}
	if err := s.persist(db.GopherSettings{
		CloudMachineID:      machine.ID,
		CloudBootstrapState: StateInstalling,
	}); err != nil {
		return fmt.Errorf("persist machine id: %w", err)
	}

	// Step 2 — delegate the install to the unhardened helper unit
	// (nimbus-gopher-bootstrap.service, fired by the .path watcher).
	// Sudo from the main service is blocked by NoNewPrivileges + the
	// hardened systemd ProtectSystem=strict, so we hand off via the
	// shared file-IPC: write pending → touch trigger → poll result.
	hostname := readHostname()
	if err := dispatchHelper(ctx, machine.BootstrapURL, hostname); err != nil {
		return fmt.Errorf("bootstrap script failed: %w", err)
	}

	// Step 3 — wait for Gopher to flip the machine to connected.
	if err := s.persist(db.GopherSettings{
		CloudMachineID:      machine.ID,
		CloudBootstrapState: StateWaitingConnect,
	}); err != nil {
		return fmt.Errorf("persist state: %w", err)
	}
	if err := s.waitConnected(ctx, machine.ID); err != nil {
		return fmt.Errorf("machine did not connect: %w", err)
	}

	// Step 4 — create the cloud tunnel (HTTP on Nimbus's port).
	if err := s.persist(db.GopherSettings{
		CloudMachineID:      machine.ID,
		CloudBootstrapState: StateCreatingTunnel,
	}); err != nil {
		return fmt.Errorf("persist state: %w", err)
	}
	t, err := s.gopher.CreateTunnel(ctx, tunnel.CreateTunnelRequest{
		MachineID:  machine.ID,
		TargetPort: s.nimbusPort,
		Subdomain:  subdomain,
	})
	if err != nil {
		// CreateTunnel can succeed server-side and still surface an error
		// to us — most commonly when Gopher takes longer than the http.Client
		// timeout to respond. If we tear everything down here, the next save
		// finds a broken half-state. Try to adopt a matching tunnel that
		// the server has by listing first; only fail if nothing matches.
		adopted, aerr := s.adoptCreatedTunnel(ctx, machine.ID, subdomain, s.nimbusPort)
		if aerr == nil && adopted != nil {
			log.Printf("self-bootstrap: adopted existing tunnel %s for machine %s after CreateTunnel error: %v", adopted.ID, machine.ID, err)
			t = adopted
		} else if isSubdomainConflict(err) {
			// 409 with subdomain-already-exists: a previous Nimbus run
			// (or another Nimbus pointed at this Gopher) registered a
			// machine + tunnel under the same subdomain, then died /
			// re-registered. The orphan tunnel sits attached to a stale
			// machine_id our adopt-by-machine logic can't see. Find it
			// across ALL tunnels, delete it, then retry CreateTunnel —
			// the operator chose this subdomain, they own it.
			retried, rerr := s.replaceConflictingTunnel(ctx, machine.ID, subdomain, s.nimbusPort)
			if rerr != nil {
				return fmt.Errorf("create tunnel (after subdomain conflict): %w", rerr)
			}
			t = retried
		} else {
			return fmt.Errorf("create tunnel: %w", err)
		}
	}

	// Step 5 — record success.
	return s.persist(db.GopherSettings{
		CloudMachineID:      machine.ID,
		CloudTunnelID:       t.ID,
		CloudTunnelURL:      t.TunnelURL,
		CloudBootstrapState: StateActive,
		CloudBootstrapError: "",
	})
}

// isSubdomainConflict matches Gopher's 409 + "subdomain already exists"
// shape. CreateTunnel surfaces this when the operator picked a
// subdomain that's already attached to a different machine — most
// often a manually-configured tunnel they want Nimbus to take over,
// or an orphan from a previous Nimbus run that re-registered as a
// new machine. Either way, the takeover path replaces it.
func isSubdomainConflict(err error) bool {
	var he *tunnel.HTTPError
	if !errors.As(err, &he) {
		return false
	}
	if he.Status != 409 {
		return false
	}
	return strings.Contains(strings.ToLower(he.Body), "subdomain")
}

// replaceConflictingTunnel handles the takeover-on-409 case. Lists
// every tunnel on Gopher, finds the one bound to `subdomain`, deletes
// it, then retries CreateTunnel under our `machineID`. The operator
// implicitly opted into this by re-saving with the conflicting
// subdomain — we treat their save as an "I own this subdomain"
// declaration. Returns the freshly-created tunnel; surfaces any
// retry/list failure verbatim so the operator sees what blocked it.
func (s *Service) replaceConflictingTunnel(ctx context.Context, machineID, subdomain string, port int) (*tunnel.Tunnel, error) {
	all, err := s.gopher.ListTunnels(ctx)
	if err != nil {
		return nil, fmt.Errorf("list tunnels: %w", err)
	}
	var conflicting *tunnel.Tunnel
	for i := range all {
		if all[i].Subdomain == subdomain {
			conflicting = &all[i]
			break
		}
	}
	if conflicting == nil {
		// 409 came back but the conflict isn't visible in our list —
		// could be a race or a Gopher account-scope mismatch. Bail
		// rather than blindly retry into the same 409.
		return nil, fmt.Errorf("subdomain %q reported as conflicting but not found in tunnels list", subdomain)
	}
	// Preserve the existing tunnel's privacy + transport flags. The
	// operator set them on the conflicting tunnel deliberately; taking
	// over the subdomain shouldn't silently downgrade a public tunnel
	// to private (or vice versa). Same logic applies to Transport and
	// NoTLS — the recreated tunnel mirrors what was there.
	log.Printf("self-bootstrap: replacing conflicting tunnel %s (subdomain=%s, machine=%s, private=%v) with one owned by machine %s",
		conflicting.ID, subdomain, conflicting.MachineID, conflicting.Private, machineID)
	if err := s.gopher.DeleteTunnel(ctx, conflicting.ID); err != nil {
		return nil, fmt.Errorf("delete conflicting tunnel %s: %w", conflicting.ID, err)
	}
	t, err := s.gopher.CreateTunnel(ctx, tunnel.CreateTunnelRequest{
		MachineID:  machineID,
		TargetPort: port,
		Subdomain:  subdomain,
		Private:    conflicting.Private,
		Transport:  conflicting.Transport,
		NoTLS:      conflicting.NoTLS,
	})
	if err != nil {
		return nil, fmt.Errorf("retry create tunnel after delete: %w", err)
	}
	return t, nil
}

// adoptCreatedTunnel checks Gopher for a tunnel matching the parameters
// we just tried to create. Used to recover from CreateTunnel client-side
// errors where the request actually reached and was processed by Gopher
// (typical with http.Client timeouts under load).
//
// Match criteria: same machine, same target_port, and either an empty
// configured subdomain (caller didn't pin one) or an exact subdomain
// match. Returns nil, nil when nothing matches — the caller falls
// through to the existing failure path.
func (s *Service) adoptCreatedTunnel(ctx context.Context, machineID, subdomain string, port int) (*tunnel.Tunnel, error) {
	list, err := s.gopher.ListTunnelsForMachine(ctx, machineID)
	if err != nil {
		return nil, err
	}
	for i := range list {
		t := list[i]
		if t.TargetPort != port {
			continue
		}
		if subdomain != "" && t.Subdomain != subdomain {
			continue
		}
		return &t, nil
	}
	return nil, nil
}

func (s *Service) waitConnected(ctx context.Context, machineID string) error {
	deadline := time.Now().Add(machineConnectTimeout)
	tick := time.NewTicker(3 * time.Second)
	defer tick.Stop()
	for {
		m, err := s.gopher.GetMachine(ctx, machineID)
		if err == nil {
			switch m.Status {
			case tunnel.StatusConnected, tunnel.StatusActive:
				return nil
			case tunnel.StatusFailed:
				return fmt.Errorf("gopher reported machine as failed")
			}
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("timeout after %s", machineConnectTimeout)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-tick.C:
		}
	}
}

// dispatchHelper hands the install off to the path-triggered helper
// unit (nimbus-gopher-bootstrap.path). Writes the pending payload,
// removes any stale result, touches the trigger file, then polls
// /var/lib/nimbus/bootstrap-result.json until populated or timeout.
//
// The helper service runs as root with no hardening — that's the
// whole point of the path-triggered split. The main nimbus.service
// stays locked down (NoNewPrivileges + ProtectSystem=strict);
// privilege boundary is the trigger file, not a sudoers entry.
//
// Timeout is 5 minutes — generous; the bootstrap typically completes
// in 30-90s but apt + slow networks can push it longer.
func dispatchHelper(ctx context.Context, bootstrapURL, machineName string) error {
	// Stale result from a previous run would be picked up by the
	// poll loop below before the helper even starts. Best-effort
	// remove; missing-file is fine.
	_ = os.Remove(resultPath)

	pending := helperPending{
		InstanceID:   "default",
		BootstrapURL: bootstrapURL,
		MachineName:  machineName,
	}
	buf, err := json.Marshal(pending)
	if err != nil {
		return fmt.Errorf("marshal pending: %w", err)
	}
	if err := os.WriteFile(pendingPath, buf, 0644); err != nil {
		return fmt.Errorf("write pending: %w", err)
	}
	// Touch the trigger file (create if absent, update mtime if not)
	// to fire the systemd path unit. The helper service deletes it
	// on completion (ExecStartPost) so the next run re-triggers
	// cleanly.
	now := time.Now()
	if err := os.WriteFile(triggerPath, []byte("trigger\n"), 0644); err != nil {
		return fmt.Errorf("write trigger: %w", err)
	}
	_ = os.Chtimes(triggerPath, now, now)

	deadline := time.Now().Add(5 * time.Minute)
	tick := time.NewTicker(2 * time.Second)
	defer tick.Stop()
	for {
		raw, err := os.ReadFile(resultPath)
		if err == nil {
			var res helperResult
			if jerr := json.Unmarshal(raw, &res); jerr != nil {
				return fmt.Errorf("parse helper result: %w", jerr)
			}
			if !res.Success {
				return fmt.Errorf("helper reported failure: %s (output: %s)",
					res.Error, truncate(res.Output, 4096))
			}
			return nil
		} else if !os.IsNotExist(err) {
			return fmt.Errorf("read result: %w", err)
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("helper timed out after 5 minutes — check `journalctl -u nimbus-gopher-bootstrap`")
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-tick.C:
		}
	}
}

// markFailed records the failure on the DB row, deletes any
// partially-registered Gopher machine, then decides whether to keep
// or wipe the credentials based on the failure shape.
//
//   - Auth-class failure (401/403): wipe credentials. The saved key
//     can't possibly work; force a re-paste with a fresh one.
//
//   - Everything else (conflicts, validation, timeouts, install
//     errors): keep credentials. The user's input is fine — it's the
//     operational step (subdomain conflict, helper install, etc.)
//     that broke. Wiping creds would force re-pasting for a problem
//     that re-pasting doesn't fix. The bootstrap_error field stays
//     populated so the modal can surface the real reason.
//
// In every case: delete the partial machine on Gopher, clear the
// cloud_machine_id / cloud_tunnel_id / cloud_tunnel_url fields, and
// drop the in-memory tunnel client when credentials get wiped.
func (s *Service) markFailed(failure error) {
	msg := failure.Error()
	existing, err := s.store.GetGopherSettings()
	if err != nil {
		log.Printf("self-bootstrap: failed to load settings while recording failure: %v", err)
		return
	}

	// Best-effort delete the partial machine on Gopher regardless of
	// failure class — an orphan machine is never useful and clutters
	// the operator's Gopher dashboard.
	if existing.CloudMachineID != "" && s.gopher != nil {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		if derr := s.gopher.DeleteMachine(ctx, existing.CloudMachineID); derr != nil {
			log.Printf("self-bootstrap cleanup: delete machine %s: %v", existing.CloudMachineID, derr)
		}
		cancel()
	}

	if isAuthClassFailure(failure) {
		// Creds are bad — wipe everything, drop the in-memory client.
		if werr := s.store.WipeGopherSettings(); werr != nil {
			log.Printf("self-bootstrap cleanup: wipe gopher settings: %v", werr)
		}
		s.mu.Lock()
		s.gopher = nil
		s.mu.Unlock()
		return
	}

	// Operational failure: keep API URL/key/subdomain, only clear the
	// cloud-tunnel state. Surfacing the error in CloudBootstrapError
	// means the modal (and the post-refresh form) shows WHY it failed
	// instead of going blank as if nothing was attempted.
	state := db.GopherSettings{
		CloudMachineID:      "",
		CloudTunnelID:       "",
		CloudTunnelURL:      "",
		CloudBootstrapState: StateFailed,
		CloudBootstrapError: msg,
	}
	if err := s.store.SaveCloudTunnelState(state); err != nil {
		log.Printf("self-bootstrap: failed to persist failure: %v", err)
	}
}

// isAuthClassFailure detects credential-invalid failures. Anything
// that wraps a tunnel.HTTPError with a 401/403 status (typically from
// CreateMachine — the first authenticated call in the bootstrap
// chain). Everything else (409 subdomain conflict, 500 install error,
// timeouts) is treated as operational.
func isAuthClassFailure(err error) bool {
	var he *tunnel.HTTPError
	if !errors.As(err, &he) {
		return false
	}
	return he.Status == 401 || he.Status == 403
}

// persist merges `update` onto the current row and writes it back. Only
// the cloud_* columns are touched (SaveCloudTunnelState's contract).
func (s *Service) persist(update db.GopherSettings) error {
	existing, err := s.store.GetGopherSettings()
	if err != nil {
		return err
	}
	state := *existing
	if update.CloudMachineID != "" {
		state.CloudMachineID = update.CloudMachineID
	}
	if update.CloudTunnelID != "" {
		state.CloudTunnelID = update.CloudTunnelID
	}
	if update.CloudTunnelURL != "" {
		state.CloudTunnelURL = update.CloudTunnelURL
	}
	if update.CloudBootstrapState != "" {
		state.CloudBootstrapState = update.CloudBootstrapState
	}
	// Error: clear when explicitly empty AND we're advancing to a
	// non-failed state, so transient failures don't sticky.
	if update.CloudBootstrapState != StateFailed {
		state.CloudBootstrapError = update.CloudBootstrapError
	}
	return s.store.SaveCloudTunnelState(state)
}

// readHostname returns the box's short hostname for GOPHER_MACHINE_NAME.
// On any failure falls back to "nimbus" — Gopher requires a non-empty
// machine name and we don't want the bootstrap to abort over hostname
// resolution.
func readHostname() string {
	out, err := exec.Command("hostname", "-s").Output()
	if err == nil {
		if name := strings.TrimSpace(string(out)); name != "" {
			return name
		}
	}
	return "nimbus"
}

// truncate keeps the head of s when it exceeds n bytes. Used to bound
// the bootstrap output we surface as the error message.
func truncate(s string, n int) string {
	s = strings.TrimSpace(s)
	if len(s) <= n {
		return s
	}
	return s[:n] + "\n…[" + strconv.Itoa(len(s)-n) + " bytes truncated]"
}
