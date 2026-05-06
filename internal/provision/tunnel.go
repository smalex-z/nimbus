package provision

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"nimbus/internal/proxmox"
	"nimbus/internal/tunnel"

	internalerrors "nimbus/internal/errors"
)

// tunnelBootstrapExecTimeout bounds the bootstrap command itself. The script
// downloads the rathole binary, may wait on dpkg locks (cloud-init holds
// /var/lib/dpkg/lock-frontend during early boot), retries apt installs, and
// installs/starts a systemd unit. Five minutes covers the realistic upper
// bound including a couple of dpkg-lock retry cycles.
const tunnelBootstrapExecTimeout = 5 * time.Minute

// tunnelBootstrapPollInterval paces agent/exec-status polling. Tighter than
// WaitForIP's interval — the bootstrap finishes on the order of seconds when
// dpkg isn't blocked.
const tunnelBootstrapPollInterval = 2 * time.Second

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

// runTunnelBootstrap runs the Gopher one-line bootstrap inside the VM via
// the qemu-guest-agent — no SSH, no L3 reach to the VM required. The data
// path is virtio-serial through the host hypervisor, so this works
// identically on vmbr0 and on isolated SDN subnets.
//
// machineName is exported as GOPHER_MACHINE_NAME so the bootstrap script
// skips its interactive `/dev/tty` prompt — there's no PTY here.
//
// Returns nil on success; any non-zero exit + the captured stdout/stderr
// is wrapped into the error so the caller can record it on tunnel_error.
func runTunnelBootstrap(ctx context.Context, px AgentRunner, node string, vmid int, bootstrapURL, machineName string) error {
	if bootstrapURL == "" {
		return errors.New("empty bootstrap URL")
	}

	// Single-quote URL + machine name so the shell doesn't interpret
	// special characters from Gopher's path (tokens) or the hostname.
	// Set GOPHER_MACHINE_NAME on the receiving shell — the script reads
	// it before it would otherwise prompt over /dev/tty.
	//
	// Download then exec, instead of `curl ... | sh`. The pipe form was
	// silently masking curl failures: if curl exits non-zero (4xx, DNS
	// blip, expired URL) the `| sh` still runs on empty input, exits
	// 0, and the SSH session reports success — leaving the machine
	// permanently `pending` on Gopher with no log on either side. POSIX
	// pipefail isn't available on dash (which is /bin/sh on Ubuntu),
	// so we split the pipeline. After the install, sanity-check the
	// systemd unit actually came up and surface its journal if not.
	quote := func(s string) string { return "'" + strings.ReplaceAll(s, "'", "'\\''") + "'" }
	script := strings.Join([]string{
		"set -e",
		"_T=$(mktemp)",
		fmt.Sprintf("curl -fsSL %s -o \"$_T\"", quote(bootstrapURL)),
		fmt.Sprintf("GOPHER_MACHINE_NAME=%s sh \"$_T\"", quote(machineName)),
		"_RC=$?",
		"rm -f \"$_T\"",
		// Verify the rathole client unit came up. Without this the
		// caller has no signal — the install script is one-shot, and
		// a failure to fetch / start the binary leaves no trace on
		// Gopher's side until the 60s poll timeout fires.
		"if ! systemctl is-active --quiet rathole-client; then",
		"  echo 'gopher bootstrap completed but rathole-client is not active:' >&2",
		"  journalctl -u rathole-client --no-pager -n 30 >&2 || true",
		"  exit 1",
		"fi",
		"exit $_RC",
	}, "\n")

	execCtx, cancel := context.WithTimeout(ctx, tunnelBootstrapExecTimeout)
	defer cancel()
	status, err := px.AgentRun(execCtx, node, vmid, []string{"/bin/sh"}, script, tunnelBootstrapPollInterval)
	if err != nil {
		return fmt.Errorf("agent exec: %w", err)
	}
	if status.ExitCode != 0 {
		out := truncateOutput(strings.TrimRight(status.OutData+status.ErrData, "\n"), maxBootstrapOutputBytes)
		return fmt.Errorf("bootstrap exit %d (output: %s)", status.ExitCode, out)
	}
	return nil
}

// ── Per-port tunnel CRUD (post-provision "Networks" surface) ────────────────

// ListVMTunnels returns every Gopher per-port tunnel attached to this VM.
// Returns an empty slice when the VM has no Gopher machine record (i.e.
// public_tunnel was not requested at provision time, or registration failed).
//
// requesterID is forwarded to Get for ownership gating; non-nil values must
// match vm.OwnerID or NotFound is returned.
func (s *Service) ListVMTunnels(ctx context.Context, vmID uint, requesterID *uint) ([]tunnel.Tunnel, error) {
	vm, err := s.Get(ctx, vmID, requesterID)
	if err != nil {
		return nil, err
	}
	if vm.TunnelID == "" || s.tunnels == nil {
		return []tunnel.Tunnel{}, nil
	}
	return s.tunnels.ListTunnelsForMachine(ctx, vm.TunnelID)
}

// VMTunnelRequest is the input to CreateVMTunnel — a thin pass-through to
// the Gopher CreateTunnelRequest with the machine_id derived from the VM.
// Bundled into a struct (vs. positional args) so adding new fields like
// transport/bot-protection doesn't ripple through call sites.
type VMTunnelRequest struct {
	TargetPort           int
	Subdomain            string
	Transport            string // "tcp" or "udp"; blank → "tcp"
	Private              bool
	NoTLS                bool
	BotProtectionEnabled bool
	BotProtectionTTL     int
	BotProtectionAllowIP string
	TLSSkipVerify        bool
}

// CreateVMTunnel registers a per-port tunnel on this VM's Gopher machine.
// TargetPort must be 1-65535; Subdomain is optional (Gopher derives one
// from the machine name when blank for TCP, ignores it for UDP). Returns
// the created tunnel record — note Gopher coerces some fields server-side
// (e.g. clears bot_protection on UDP tunnels), so the response is the
// source of truth, not the request.
//
// requesterID is forwarded to Get for ownership gating; non-nil values must
// match vm.OwnerID or NotFound is returned. Without this gate a member could
// create internet-facing tunnels on another member's VM.
func (s *Service) CreateVMTunnel(ctx context.Context, vmID uint, req VMTunnelRequest, requesterID *uint) (*tunnel.Tunnel, error) {
	// Cheap input validation comes first — same answer for any caller.
	if req.TargetPort < 1 || req.TargetPort > 65535 {
		return nil, &internalerrors.ValidationError{Field: "target_port", Message: "must be 1-65535"}
	}
	if req.Transport != "" && req.Transport != "tcp" && req.Transport != "udp" {
		return nil, &internalerrors.ValidationError{Field: "transport", Message: "must be \"tcp\" or \"udp\""}
	}
	// Ownership before deployment-config so a non-owner gets NotFound rather
	// than learning whether tunnels are wired up at all.
	vm, err := s.Get(ctx, vmID, requesterID)
	if err != nil {
		return nil, err
	}
	if s.tunnels == nil {
		return nil, errors.New("gopher tunnel integration is not configured")
	}
	if vm.TunnelID == "" {
		return nil, &internalerrors.ValidationError{
			Field:   "vm",
			Message: "this VM is not connected to Gopher — re-provision with the public-SSH toggle to enable tunnels",
		}
	}
	return s.tunnels.CreateTunnel(ctx, tunnel.CreateTunnelRequest{
		MachineID:            vm.TunnelID,
		TargetPort:           req.TargetPort,
		Subdomain:            strings.TrimSpace(req.Subdomain),
		Transport:            req.Transport,
		Private:              req.Private,
		NoTLS:                req.NoTLS,
		BotProtectionEnabled: req.BotProtectionEnabled,
		BotProtectionTTL:     req.BotProtectionTTL,
		BotProtectionAllowIP: req.BotProtectionAllowIP,
		TLSSkipVerify:        req.TLSSkipVerify,
	})
}

// DeleteVMTunnel removes a per-port tunnel. The tunnelID must belong to the
// named VM's Gopher machine — we filter by machine_id before deleting so a
// caller can't tear down another VM's tunnel by guessing IDs.
//
// requesterID is forwarded to Get for ownership gating; non-nil values must
// match vm.OwnerID or NotFound is returned. The machine_id filter on its own
// only prevents cross-VM mismatch, not cross-user access.
func (s *Service) DeleteVMTunnel(ctx context.Context, vmID uint, tunnelID string, requesterID *uint) error {
	// Ownership before deployment-config so a non-owner gets NotFound rather
	// than learning whether tunnels are wired up at all.
	vm, err := s.Get(ctx, vmID, requesterID)
	if err != nil {
		return err
	}
	if s.tunnels == nil {
		return errors.New("gopher tunnel integration is not configured")
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

// AgentRunner is the slice of *proxmox.Client the bootstrap paths need —
// "accept interfaces" so tests can inject a fake without standing up the
// whole Proxmox client surface.
type AgentRunner interface {
	AgentRun(ctx context.Context, node string, vmid int, command []string, inputData string, pollInterval time.Duration) (*proxmox.AgentExecStatus, error)
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
