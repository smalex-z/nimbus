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
			s.markFailed(err.Error())
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
		if aerr != nil || adopted == nil {
			return fmt.Errorf("create tunnel: %w", err)
		}
		log.Printf("self-bootstrap: adopted existing tunnel %s for machine %s after CreateTunnel error: %v", adopted.ID, machine.ID, err)
		t = adopted
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

// markFailed records the failure on the DB row, then runs the
// clean-slate cleanup so the operator isn't left with a half-registered
// Gopher machine and stale API credentials they have to manually undo.
//
// Sequencing matters:
//  1. Write StateFailed + error message — so the modal renders the
//     reason even if cleanup races a still-polling SPA refresh.
//  2. cleanupAfterFailure deletes any registered Gopher machine
//     (cascades to tunnels) and wipes every Gopher column on the row.
//  3. Drop the in-memory tunnel.Client — credentials are gone, the
//     client can no longer authenticate. Re-saving from the SPA
//     rebuilds it via the TunnelClientApplier path.
//
// All steps are best-effort: a stale row is preferable to a panic, and
// the operator still has the SPA + Gopher web UI to clean up by hand.
func (s *Service) markFailed(msg string) {
	existing, err := s.store.GetGopherSettings()
	if err != nil {
		log.Printf("self-bootstrap: failed to load settings while recording failure: %v", err)
		return
	}
	state := *existing
	state.CloudBootstrapState = StateFailed
	state.CloudBootstrapError = msg
	if err := s.store.SaveCloudTunnelState(state); err != nil {
		log.Printf("self-bootstrap: failed to persist failure: %v", err)
	}
	s.cleanupAfterFailure(existing)
}

// cleanupAfterFailure tears down Gopher-side artifacts and wipes the
// local DB row. Best-effort throughout — the goal is "no partial state
// the operator can stumble into," not transactional certainty. Errors
// are logged.
//
// Called from markFailed only. Reads the pre-failure settings (passed
// in) since we'll wipe them mid-flow.
func (s *Service) cleanupAfterFailure(pre *db.GopherSettings) {
	if pre == nil {
		return
	}
	// 1. Best-effort delete the machine on Gopher. DeleteMachine
	//    cascades to all child tunnels so we don't need a separate
	//    DeleteTunnel call. Fresh ctx — markFailed may be called from
	//    a cancelled bootstrap ctx, but the cleanup HTTP call should
	//    still get a fair shot.
	if pre.CloudMachineID != "" && s.gopher != nil {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		if err := s.gopher.DeleteMachine(ctx, pre.CloudMachineID); err != nil {
			log.Printf("self-bootstrap cleanup: delete machine %s: %v", pre.CloudMachineID, err)
		}
		cancel()
	}
	// 2. Wipe every Gopher column. The user's choice — partial state is
	//    confusing, force re-paste on retry.
	if err := s.store.WipeGopherSettings(); err != nil {
		log.Printf("self-bootstrap cleanup: wipe gopher settings: %v", err)
	}
	// 3. Drop the in-memory tunnel.Client. Credentials are gone; any
	//    further calls would 401 against Gopher. Other appliers
	//    (provision.Service, admin tunnels handler) hold their own
	//    clients — they self-heal when the operator next saves creds.
	s.mu.Lock()
	s.gopher = nil
	s.mu.Unlock()
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
