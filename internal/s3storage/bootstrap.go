package s3storage

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"net"
	"strings"
	"time"

	"golang.org/x/crypto/ssh"
)

// SSH bootstrap timing — matches the Gopher tunnel bootstrap pattern in
// provision/tunnel.go. The shape (dial-retry on failure, single-shot
// exec, longer exec timeout to absorb cloud-init's dpkg locks and a
// minio image pull) is identical; constants live here to avoid coupling
// the two subsystems.
const (
	bootstrapDialTimeout    = 30 * time.Second
	bootstrapExecTimeout    = 8 * time.Minute
	bootstrapMaxAttempts    = 3
	bootstrapRetryDelay     = 5 * time.Second
	maxBootstrapOutputBytes = 8 * 1024
)

// installScript is the shell command we exec on the VM to bring MinIO
// up. Idempotent — safe to re-run if a previous attempt failed
// mid-stream. The placeholders are substituted before sending; we
// single-quote them so the shell doesn't interpret randomness as
// metacharacters.
//
// Notes:
//   - cloud-init status --wait blocks until cloud-init has finished its
//     own apt-get phase. Without this, the docker installer races
//     cloud-init for /var/lib/dpkg/lock-frontend and fails. The agent
//     comes up before cloud-init is done (we wait on the agent in the
//     provision step), so we MUST gate package work on cloud-init.
//   - get.docker.com is the upstream-blessed installer; preferred over a
//     curated apt source because it works on whichever Ubuntu/Debian
//     point release the operator's templates carry.
//   - --restart unless-stopped so MinIO survives reboots without a
//     systemd unit.
//   - /srv/minio is the canonical "service data" mount point.
//
// Privileged commands are explicitly sudo-prefixed: cloud images SSH us
// in as a non-root cloud user (`ubuntu`/`debian`) with passwordless sudo
// but no docker-group membership. Even after Docker is installed,
// `docker` calls need sudo until the user logs out and back in.
const installScript = `set -euo pipefail
if command -v cloud-init >/dev/null; then
  cloud-init status --wait || true
fi
if ! command -v docker >/dev/null; then
  curl -fsSL https://get.docker.com | sh
fi
sudo mkdir -p /srv/minio
sudo docker rm -f nimbus-minio 2>/dev/null || true
sudo docker run -d --name nimbus-minio --restart unless-stopped \
  -p 9000:9000 -p 9001:9001 \
  -e MINIO_ROOT_USER=%s \
  -e MINIO_ROOT_PASSWORD=%s \
  -v /srv/minio:/data \
  minio/minio:latest server /data --console-address ":9001"
`

// GenerateRootCredentials returns a fresh (user, password) pair suitable
// for MinIO's MINIO_ROOT_USER / MINIO_ROOT_PASSWORD. 32-byte hex →
// 64-char strings, well above MinIO's 8-char minimum and short enough
// to fit a docker env var without quoting tricks.
func GenerateRootCredentials() (user, password string, err error) {
	u, err := randomHex(16) // 32 hex chars
	if err != nil {
		return "", "", fmt.Errorf("random user: %w", err)
	}
	p, err := randomHex(32) // 64 hex chars
	if err != nil {
		return "", "", fmt.Errorf("random password: %w", err)
	}
	return "nimbus_" + u, p, nil
}

func randomHex(n int) (string, error) {
	buf := make([]byte, n)
	if _, err := rand.Read(buf); err != nil {
		return "", err
	}
	return hex.EncodeToString(buf), nil
}

// runBootstrap dials the VM, runs the MinIO install script with the
// provided root credentials, and returns nil on success. Mirrors the
// retry/timeout shape of provision.runTunnelBootstrap.
func runBootstrap(ctx context.Context, ip, user, privatePEM, rootUser, rootPass string) error {
	if ip == "" {
		return errors.New("empty ip")
	}
	signer, err := ssh.ParsePrivateKey([]byte(privatePEM))
	if err != nil {
		return fmt.Errorf("parse private key: %w", err)
	}
	cfg := &ssh.ClientConfig{
		User:            user,
		Auth:            []ssh.AuthMethod{ssh.PublicKeys(signer)},
		HostKeyCallback: ssh.InsecureIgnoreHostKey(), //nolint:gosec
		Timeout:         bootstrapDialTimeout,
	}

	cmd := fmt.Sprintf(installScript, shellSingleQuote(rootUser), shellSingleQuote(rootPass))

	var lastErr error
	for attempt := 1; attempt <= bootstrapMaxAttempts; attempt++ {
		client, err := dialSSH(ctx, ip, cfg)
		if err != nil {
			lastErr = err
			if attempt == bootstrapMaxAttempts {
				break
			}
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(bootstrapRetryDelay):
			}
			continue
		}
		defer client.Close() //nolint:errcheck

		session, err := client.NewSession()
		if err != nil {
			return fmt.Errorf("new session: %w", err)
		}
		defer session.Close() //nolint:errcheck

		execTimer := time.AfterFunc(bootstrapExecTimeout, func() {
			_ = session.Close()
		})
		out, runErr := session.CombinedOutput(cmd)
		execTimer.Stop()
		if runErr != nil {
			return fmt.Errorf("install script failed: %w (output: %s)", runErr, truncateOutput(string(out), maxBootstrapOutputBytes))
		}
		return nil
	}
	return fmt.Errorf("ssh connect failed after %d attempts: %w", bootstrapMaxAttempts, lastErr)
}

// shellSingleQuote wraps s for safe inclusion in a single-quoted shell
// string, escaping any embedded single quotes.
func shellSingleQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", "'\\''") + "'"
}

// truncateOutput keeps the head and tail of a long command output so the
// failure record stays useful without bloating the DB.
func truncateOutput(s string, max int) string {
	if len(s) <= max {
		return s
	}
	half := max / 2
	return s[:half] + "\n... [truncated] ...\n" + s[len(s)-half:]
}

func dialSSH(ctx context.Context, ip string, cfg *ssh.ClientConfig) (*ssh.Client, error) {
	dialCtx, cancel := context.WithTimeout(ctx, bootstrapDialTimeout)
	defer cancel()

	dialer := &net.Dialer{Timeout: bootstrapDialTimeout}
	conn, err := dialer.DialContext(dialCtx, "tcp", net.JoinHostPort(ip, "22"))
	if err != nil {
		return nil, fmt.Errorf("dial %s:22: %w", ip, err)
	}
	_ = conn.SetDeadline(time.Now().Add(bootstrapDialTimeout))

	clientConn, chans, reqs, err := ssh.NewClientConn(conn, net.JoinHostPort(ip, "22"), cfg)
	if err != nil {
		_ = conn.Close()
		return nil, fmt.Errorf("ssh handshake: %w", err)
	}
	_ = conn.SetDeadline(time.Time{})
	return ssh.NewClient(clientConn, chans, reqs), nil
}
