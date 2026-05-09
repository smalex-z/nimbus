package gateway

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"errors"
	"fmt"
	"net"
	"strings"
	"time"

	"golang.org/x/crypto/ssh"
)

// EphemeralKeypair is the one-shot ed25519 SSH keypair we mint for
// bootstrapping a single gateway LXC. The private half lives only
// in memory for the duration of Provision; once setup completes
// the keypair is discarded. PVE injects the public half into
// /root/.ssh/authorized_keys at LXC create time via the
// `ssh-public-keys` parameter, giving us a one-shot login channel
// without any operator-managed key infrastructure.
//
// Exported so the BootstrapFn hook signature can reference it from
// outside the package (tests live in gateway_test).
type EphemeralKeypair struct {
	authorizedKey string // OpenSSH "ssh-ed25519 AAAA... nimbus-gateway-bootstrap" format
	signer        ssh.Signer
}

// AuthorizedKey returns the OpenSSH-formatted public key suitable
// for /root/.ssh/authorized_keys (and PVE's `ssh-public-keys` LXC
// create parameter, which writes that file for us).
func (k *EphemeralKeypair) AuthorizedKey() string { return k.authorizedKey }

// newEphemeralKeypair generates a fresh ed25519 keypair. The private
// half is wrapped in an ssh.Signer ready to plug into ssh.ClientConfig.
func newEphemeralKeypair() (*EphemeralKeypair, error) {
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return nil, fmt.Errorf("generate ed25519 key: %w", err)
	}
	signer, err := ssh.NewSignerFromKey(priv)
	if err != nil {
		return nil, fmt.Errorf("wrap ed25519 signer: %w", err)
	}
	sshPub, err := ssh.NewPublicKey(pub)
	if err != nil {
		return nil, fmt.Errorf("wrap ed25519 pubkey: %w", err)
	}
	authorized := strings.TrimSuffix(string(ssh.MarshalAuthorizedKey(sshPub)), "\n")
	authorized += " nimbus-gateway-bootstrap"
	return &EphemeralKeypair{authorizedKey: authorized, signer: signer}, nil
}

// runSSHBootstrap dials the LXC's host-network IP, authenticates with
// the ephemeral key, and runs the supplied script as root. Blocks
// until the script exits (or the context fires).
//
// Caller is responsible for sleeping/retrying SSH dial — fresh LXCs
// take a few seconds to boot sshd. We retry the dial internally so
// the caller doesn't need to.
func runSSHBootstrap(ctx context.Context, host string, key *EphemeralKeypair, script string) error {
	cfg := &ssh.ClientConfig{
		User:            "root",
		Auth:            []ssh.AuthMethod{ssh.PublicKeys(key.signer)},
		HostKeyCallback: ssh.InsecureIgnoreHostKey(), // freshly-created LXC, no TOFU base
		Timeout:         10 * time.Second,
	}

	addr := net.JoinHostPort(host, "22")
	client, err := dialSSHWithRetries(ctx, addr, cfg)
	if err != nil {
		return fmt.Errorf("dial gateway-lxc ssh %s: %w", addr, err)
	}
	defer client.Close() //nolint:errcheck

	session, err := client.NewSession()
	if err != nil {
		return fmt.Errorf("open ssh session: %w", err)
	}
	defer session.Close() //nolint:errcheck

	var stdout, stderr bytes.Buffer
	session.Stdout = &stdout
	session.Stderr = &stderr
	if err := session.Run(script); err != nil {
		return fmt.Errorf("run bootstrap (stderr=%q): %w", strings.TrimSpace(stderr.String()), err)
	}
	return nil
}

// dialSSHWithRetries polls TCP/22 then attempts handshake until it
// works or the context expires. Standard fresh-host pattern: sshd
// takes ~5-15 s to start after the LXC begins booting.
func dialSSHWithRetries(ctx context.Context, addr string, cfg *ssh.ClientConfig) (*ssh.Client, error) {
	const interval = 2 * time.Second
	deadline, hasDeadline := ctx.Deadline()
	if !hasDeadline {
		deadline = time.Now().Add(90 * time.Second)
	}

	var lastErr error
	for {
		if ctx.Err() != nil {
			if lastErr != nil {
				return nil, fmt.Errorf("%w (last attempt: %v)", ctx.Err(), lastErr)
			}
			return nil, ctx.Err()
		}
		if time.Now().After(deadline) {
			if lastErr != nil {
				return nil, fmt.Errorf("ssh did not come up within budget (last attempt: %w)", lastErr)
			}
			return nil, errors.New("ssh did not come up within budget")
		}
		client, err := ssh.Dial("tcp", addr, cfg)
		if err == nil {
			return client, nil
		}
		lastErr = err
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(interval):
		}
	}
}
