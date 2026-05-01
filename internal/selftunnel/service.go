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
	"errors"
	"fmt"
	"log"
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
}

// gopherClient is the slice of tunnel.Client we need.
type gopherClient interface {
	CreateMachine(ctx context.Context, req tunnel.CreateMachineRequest) (*tunnel.Machine, error)
	GetMachine(ctx context.Context, id string) (*tunnel.Machine, error)
	DeleteMachine(ctx context.Context, id string) error
	CreateTunnel(ctx context.Context, req tunnel.CreateTunnelRequest) (*tunnel.Tunnel, error)
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

	// Pre-flight: bootstrap script needs sudo for apt-install + systemd.
	// Surfacing this as a state-machine failure rather than letting the
	// bootstrap exit with a confusing apt error mid-run.
	if err := s.checkSudo(ctx); err != nil {
		s.markFailed(fmt.Sprintf("passwordless sudo required to install rathole on the Nimbus host: %v", err))
		s.clearInflight()
		return err
	}

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

	// Step 2 — run the bootstrap script locally. Hostname goes via
	// GOPHER_MACHINE_NAME so the script's interactive prompt is skipped.
	hostname := readHostname()
	cmd := fmt.Sprintf(
		"curl -fsSL %s | GOPHER_MACHINE_NAME=%s sudo -E bash",
		shellQuote(machine.BootstrapURL), shellQuote(hostname),
	)
	out, runErr := s.run(ctx, "bash", "-c", cmd)
	if runErr != nil {
		return fmt.Errorf("bootstrap script failed: %w (output: %s)", runErr, truncate(string(out), 4096))
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
		return fmt.Errorf("create tunnel: %w", err)
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

// checkSudo pre-flights `sudo -n true` so we fail fast with a clear error
// rather than partway through the bootstrap. Cancellable via ctx.
func (s *Service) checkSudo(ctx context.Context) error {
	out, err := s.run(ctx, "sudo", "-n", "true")
	if err != nil {
		return fmt.Errorf("%w (output: %s)", err, strings.TrimSpace(string(out)))
	}
	return nil
}

// markFailed writes the failed state + error message to the DB.
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

// shellQuote wraps s in single quotes for safe interpolation into a
// `bash -c "..."` command line. Mirrors the pattern in provision/tunnel.go.
func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", "'\\''") + "'"
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
