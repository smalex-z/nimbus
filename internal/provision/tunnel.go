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
	"nimbus/internal/tunnel"
)

// tunnelBootstrapSSHTimeout caps the entire SSH-and-run-script step. The
// `curl … | sh` is fast (rathole binary download + start), but we don't want
// to sit forever on a hung connection.
const tunnelBootstrapSSHTimeout = 30 * time.Second

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
// returns nil on success. host:port defaults to ip:22. The remote command
// runs synchronously — return only after the bootstrap script finishes (or
// errors).
func runTunnelBootstrap(ctx context.Context, ip, user, privatePEM, bootstrapURL string) error {
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
		Timeout:         tunnelBootstrapSSHTimeout,
	}

	dialCtx, cancel := context.WithTimeout(ctx, tunnelBootstrapSSHTimeout)
	defer cancel()

	dialer := &net.Dialer{Timeout: tunnelBootstrapSSHTimeout}
	conn, err := dialer.DialContext(dialCtx, "tcp", net.JoinHostPort(ip, "22"))
	if err != nil {
		return fmt.Errorf("dial %s:22: %w", ip, err)
	}
	defer conn.Close() //nolint:errcheck
	_ = conn.SetDeadline(time.Now().Add(tunnelBootstrapSSHTimeout))

	clientConn, chans, reqs, err := ssh.NewClientConn(conn, net.JoinHostPort(ip, "22"), cfg)
	if err != nil {
		return fmt.Errorf("ssh handshake: %w", err)
	}
	client := ssh.NewClient(clientConn, chans, reqs)
	defer client.Close() //nolint:errcheck

	session, err := client.NewSession()
	if err != nil {
		return fmt.Errorf("new session: %w", err)
	}
	defer session.Close() //nolint:errcheck

	// Single-quote the URL so the shell doesn't interpret special characters
	// from Gopher's bootstrap path (tokens, query strings).
	cmd := fmt.Sprintf("curl -fsSL '%s' | sh", strings.ReplaceAll(bootstrapURL, "'", "'\\''"))
	out, err := session.CombinedOutput(cmd)
	if err != nil {
		return fmt.Errorf("bootstrap command failed: %w (output: %s)", err, strings.TrimSpace(string(out)))
	}
	return nil
}

// waitTunnelActive polls Gopher until the tunnel is active or the budget is
// exhausted. Returns the active tunnel on success.
func (s *Service) waitTunnelActive(ctx context.Context, id string) (*tunnel.Tunnel, error) {
	deadline := time.Now().Add(tunnelActiveTimeout)
	t := time.NewTicker(tunnelPollInterval)
	defer t.Stop()

	// First check is immediate — no point waiting an interval if Gopher
	// reports active right away (race when bootstrap is fast). Transient
	// errors fall through to the next tick.
	for {
		if got, err := s.tunnels.Get(ctx, id); err == nil {
			switch got.Status {
			case tunnel.StatusActive:
				return got, nil
			case tunnel.StatusFailed:
				return nil, fmt.Errorf("gopher reported tunnel %s as failed", id)
			}
		}
		if time.Now().After(deadline) {
			return nil, fmt.Errorf("tunnel %s did not reach active within %s", id, tunnelActiveTimeout)
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-t.C:
		}
	}
}
