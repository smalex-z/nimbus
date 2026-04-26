package provision

import (
	"context"
	"errors"
	"fmt"
	"net"
	"strings"
	"time"

	"golang.org/x/crypto/ssh"

	"nimbus/internal/db"
	internalerrors "nimbus/internal/errors"
	"nimbus/internal/tunnel"
)

// tunnelBootstrapDialTimeout bounds the SSH dial + handshake. The VM's sshd
// is up within seconds of WaitForIP returning; if it takes longer than this,
// it's a real connectivity problem, not a slow boot.
const tunnelBootstrapDialTimeout = 30 * time.Second

// tunnelBootstrapExecTimeout bounds the bootstrap command itself. The script
// downloads the rathole binary, may wait on dpkg locks (cloud-init holds
// /var/lib/dpkg/lock-frontend during early boot), retries apt installs, and
// installs/starts a systemd unit. Five minutes covers the realistic upper
// bound including a couple of dpkg-lock retry cycles. Tighter than this and
// we kill the SSH session mid-bootstrap, leaving the machine in a stuck
// pending state on Gopher.
const tunnelBootstrapExecTimeout = 5 * time.Minute

// tunnelBootstrapMaxAttempts caps how many times we'll retry the connect.
// WaitForIP confirms the agent reports an IP; sshd may still be a couple of
// seconds behind, so a small retry budget catches the common race without
// stretching provisioning.
const (
	tunnelBootstrapMaxAttempts = 3
	tunnelBootstrapRetryDelay  = 5 * time.Second
)

// maxBootstrapOutputBytes caps the bootstrap stdout/stderr we keep in the
// VM's tunnel_error column. Real failures fit comfortably; runaway loops
// (e.g. a bootstrap script stuck reprinting the same error) won't bloat the
// DB. We keep the head + tail so the user sees the start of the run AND the
// final state where the script gave up.
const maxBootstrapOutputBytes = 8 * 1024

// tunnelActiveTimeout / tunnelPollInterval pace the post-bootstrap status poll.
// Design §10.1 specifies 60 s / 3 s.
const (
	tunnelActiveTimeout = 60 * time.Second
	tunnelPollInterval  = 3 * time.Second
)

// privateKeyForBootstrap returns the plaintext SSH private key Nimbus should
// use to log into the freshly provisioned VM. Three sources, in priority:
//
//  1. The plaintext we already have in memory (when GenerateKey was true,
//     resolveSSHKey returned the freshly minted private half).
//  2. The vault — when the user attached or imported a private half on the
//     SSHKey row.
//  3. Otherwise: error. The user gave us only a public key, so we can't SSH.
func (s *Service) privateKeyForBootstrap(ctx context.Context, key *db.SSHKey, justGenerated string) (string, error) {
	if justGenerated != "" {
		return justGenerated, nil
	}
	if key == nil {
		return "", errors.New("no ssh key resolved")
	}
	if !key.HasPrivateKey() {
		return "", errors.New("private half not available — vault has only the public key")
	}
	_, plain, err := s.keys.GetPrivateKey(ctx, key.ID)
	if err != nil {
		return "", fmt.Errorf("decrypt key %d: %w", key.ID, err)
	}
	return plain, nil
}

// runTunnelBootstrap dials the VM, executes `curl <bootstrap_url> | sh`, and
// returns nil on success. The dial+handshake is retried — sshd may not be
// listening immediately after WaitForIP returns. The remote command itself
// is NOT retried; a failure there is a real script error, not a race.
//
// machineName is exported as GOPHER_MACHINE_NAME so the bootstrap script
// skips its interactive prompt — without a PTY, the script's `/dev/tty`
// fallback isn't usable.
func runTunnelBootstrap(ctx context.Context, ip, user, privatePEM, bootstrapURL, machineName string) error {
	if bootstrapURL == "" {
		return errors.New("empty bootstrap URL")
	}
	signer, err := ssh.ParsePrivateKey([]byte(privatePEM))
	if err != nil {
		return fmt.Errorf("parse private key: %w", err)
	}
	cfg := &ssh.ClientConfig{
		User: user,
		Auth: []ssh.AuthMethod{ssh.PublicKeys(signer)},
		// The VM was just created — we have no host-key history to compare
		// against. This is one-time provisioning over the cluster LAN, not a
		// long-lived SSH client; trust-on-first-use is acceptable here.
		HostKeyCallback: ssh.InsecureIgnoreHostKey(), //nolint:gosec
		Timeout:         tunnelBootstrapDialTimeout,
	}

	var lastErr error
	for attempt := 1; attempt <= tunnelBootstrapMaxAttempts; attempt++ {
		client, err := dialSSH(ctx, ip, cfg)
		if err != nil {
			lastErr = err
			if attempt == tunnelBootstrapMaxAttempts {
				break
			}
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(tunnelBootstrapRetryDelay):
			}
			continue
		}
		defer client.Close() //nolint:errcheck

		session, err := client.NewSession()
		if err != nil {
			return fmt.Errorf("new session: %w", err)
		}
		defer session.Close() //nolint:errcheck

		// Single-quote URL + machine name so the shell doesn't interpret
		// special characters from Gopher's path (tokens) or the hostname.
		// Set GOPHER_MACHINE_NAME on the receiving shell — the script reads
		// it before it would otherwise prompt over /dev/tty (which doesn't
		// exist on a non-PTY SSH session).
		quote := func(s string) string { return "'" + strings.ReplaceAll(s, "'", "'\\''") + "'" }
		cmd := fmt.Sprintf("curl -fsSL %s | GOPHER_MACHINE_NAME=%s sh", quote(bootstrapURL), quote(machineName))

		// Bound the exec separately from the dial. If the bootstrap genuinely
		// runs longer than tunnelBootstrapExecTimeout, close the session to
		// unblock CombinedOutput; the subsequent runErr will surface as
		// "session closed" with whatever output we captured up to that point.
		execTimer := time.AfterFunc(tunnelBootstrapExecTimeout, func() {
			_ = session.Close()
		})
		out, runErr := session.CombinedOutput(cmd)
		execTimer.Stop()
		if runErr != nil {
			return fmt.Errorf("bootstrap command failed: %w (output: %s)", runErr, truncateOutput(string(out), maxBootstrapOutputBytes))
		}
		return nil
	}
	return fmt.Errorf("ssh connect failed after %d attempts: %w", tunnelBootstrapMaxAttempts, lastErr)
}

// ── Per-port tunnel CRUD (post-provision "Networks" surface) ────────────────

// ListVMTunnels returns every Gopher per-port tunnel attached to this VM.
// Returns an empty slice when the VM has no Gopher machine record (i.e.
// public_tunnel was not requested at provision time, or registration failed).
func (s *Service) ListVMTunnels(ctx context.Context, vmID uint) ([]tunnel.Tunnel, error) {
	vm, err := s.Get(ctx, vmID)
	if err != nil {
		return nil, err
	}
	if vm.TunnelID == "" || s.tunnels == nil {
		return []tunnel.Tunnel{}, nil
	}
	return s.tunnels.ListTunnelsForMachine(ctx, vm.TunnelID)
}

// CreateVMTunnel registers a per-port tunnel on this VM's Gopher machine.
// targetPort must be 1-65535; subdomain is optional (Gopher derives one from
// the machine name when blank); private flips the gateway-side bind to
// 127.0.0.1 (VPS-local only). Returns the created tunnel record.
func (s *Service) CreateVMTunnel(ctx context.Context, vmID uint, targetPort int, subdomain string, private bool) (*tunnel.Tunnel, error) {
	if s.tunnels == nil {
		return nil, errors.New("gopher tunnel integration is not configured")
	}
	if targetPort < 1 || targetPort > 65535 {
		return nil, &internalerrors.ValidationError{Field: "target_port", Message: "must be 1-65535"}
	}
	vm, err := s.Get(ctx, vmID)
	if err != nil {
		return nil, err
	}
	if vm.TunnelID == "" {
		return nil, &internalerrors.ValidationError{
			Field:   "vm",
			Message: "this VM is not connected to Gopher — re-provision with the public-SSH toggle to enable tunnels",
		}
	}
	return s.tunnels.CreateTunnel(ctx, tunnel.CreateTunnelRequest{
		MachineID:  vm.TunnelID,
		TargetPort: targetPort,
		Subdomain:  strings.TrimSpace(subdomain),
		Private:    private,
	})
}

// DeleteVMTunnel removes a per-port tunnel. The tunnelID must belong to the
// named VM's Gopher machine — we filter by machine_id before deleting so a
// caller can't tear down another VM's tunnel by guessing IDs.
func (s *Service) DeleteVMTunnel(ctx context.Context, vmID uint, tunnelID string) error {
	if s.tunnels == nil {
		return errors.New("gopher tunnel integration is not configured")
	}
	vm, err := s.Get(ctx, vmID)
	if err != nil {
		return err
	}
	if vm.TunnelID == "" {
		return &internalerrors.NotFoundError{Resource: "tunnel", ID: tunnelID}
	}
	tunnels, err := s.tunnels.ListTunnelsForMachine(ctx, vm.TunnelID)
	if err != nil {
		return err
	}
	for _, t := range tunnels {
		if t.ID == tunnelID {
			return s.tunnels.DeleteTunnel(ctx, tunnelID)
		}
	}
	return &internalerrors.NotFoundError{Resource: "tunnel", ID: tunnelID}
}

// truncateOutput keeps the first half + last half of s when it exceeds max,
// joined by an elision marker. Trims surrounding whitespace first so the
// return is tidy when the input fits. Used to keep bootstrap stderr from
// blowing up tunnel_error storage on stuck loops.
func truncateOutput(s string, max int) string {
	s = strings.TrimSpace(s)
	if len(s) <= max {
		return s
	}
	half := max / 2
	return s[:half] + "\n…[truncated " + fmt.Sprintf("%d", len(s)-max) + " bytes]…\n" + s[len(s)-half:]
}

// dialSSH opens a single SSH session to ip:22 with the supplied config,
// honoring ctx cancellation. Returns a usable *ssh.Client; caller must Close.
//
// The connection deadline is only set across dial+handshake, not the full
// session lifetime — a hard deadline on the underlying conn would kill long
// command runs (the bootstrap can take minutes once dpkg-lock retries kick
// in). Once the handshake is done, the deadline is cleared and command-level
// bounding is the caller's responsibility (see runTunnelBootstrap).
func dialSSH(ctx context.Context, ip string, cfg *ssh.ClientConfig) (*ssh.Client, error) {
	dialCtx, cancel := context.WithTimeout(ctx, tunnelBootstrapDialTimeout)
	defer cancel()

	dialer := &net.Dialer{Timeout: tunnelBootstrapDialTimeout}
	conn, err := dialer.DialContext(dialCtx, "tcp", net.JoinHostPort(ip, "22"))
	if err != nil {
		return nil, fmt.Errorf("dial %s:22: %w", ip, err)
	}
	_ = conn.SetDeadline(time.Now().Add(tunnelBootstrapDialTimeout))

	clientConn, chans, reqs, err := ssh.NewClientConn(conn, net.JoinHostPort(ip, "22"), cfg)
	if err != nil {
		_ = conn.Close()
		return nil, fmt.Errorf("ssh handshake: %w", err)
	}
	// Clear the dial-time deadline so the subsequent command exec isn't
	// killed mid-run.
	_ = conn.SetDeadline(time.Time{})
	return ssh.NewClient(clientConn, chans, reqs), nil
}

// waitMachineActive polls Gopher until the machine reports connected or the
// budget is exhausted. (Gopher's external API exposes "connected" as the
// success state; "active" was an earlier name we still tolerate for safety.)
// Returns the machine on success — its PublicSSHHost/Port carry the routable
// connection details when Gopher includes them.
func (s *Service) waitMachineActive(ctx context.Context, id string) (*tunnel.Machine, error) {
	deadline := time.Now().Add(tunnelActiveTimeout)
	t := time.NewTicker(tunnelPollInterval)
	defer t.Stop()

	// First check is immediate — bootstrap can finish before the first tick
	// fires. Transient lookup errors fall through to the next tick.
	for {
		if got, err := s.tunnels.GetMachine(ctx, id); err == nil {
			switch got.Status {
			case tunnel.StatusConnected, tunnel.StatusActive:
				return got, nil
			case tunnel.StatusFailed:
				return nil, fmt.Errorf("gopher reported machine %s as failed", id)
			}
		}
		if time.Now().After(deadline) {
			return nil, fmt.Errorf("machine %s did not become connected within %s", id, tunnelActiveTimeout)
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-t.C:
		}
	}
}
