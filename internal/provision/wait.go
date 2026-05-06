package provision

import (
	"context"
	"errors"
	"fmt"
	"net"
	"strings"
	"time"

	"nimbus/internal/proxmox"
)

// agentFailureBudget is how long we wait for the qemu-guest-agent to come up
// before falling back to TCP-pinging port 22 directly. Templates that ship
// without the agent will hit this fallback every time.
const agentFailureBudget = 30 * time.Second

// WaitDiagnostics is what the last polling attempt observed when WaitForIP
// returns. It's used to enrich the soft-success warning so users (and the
// next agent debugging this) can see *why* the wait failed instead of
// guessing. Populated even on the success path — caller can ignore it then.
//
// Possible terminal states:
//   - AgentSeen=true, AgentIPs lists what the agent reported and the
//     expected IP wasn't among them → cloud-init didn't apply the IP we
//     configured, OR a DHCP race overrode it.
//   - AgentSeen=false, LastAgentErr non-empty → the agent was never
//     reachable. Usually "QEMU guest agent is not running" (template
//     missing the package, or VM still booting cloud-init).
//   - TCPReachable=true → fallback succeeded; only meaningful when
//     AgentSeen=false (we'd have returned earlier on a real success).
type WaitDiagnostics struct {
	AgentSeen     bool
	AgentIPs      []string // every non-loopback IP the agent reported, last call
	FirstAgentErr string   // FIRST real error from GetAgentInterfaces (not ctx-truncated)
	TCPReachable  bool     // did the TCP:22 fallback ever succeed?
	Elapsed       time.Duration
	ExpectedIP    string

	// AgentConfig is the value of the `agent` field in the VM's Proxmox
	// config at the moment of timeout. Empty means the field wasn't set
	// (clone didn't inherit it, or template never had it). When set, it
	// reads like "enabled=1". Captured post-mortem only, not on every
	// poll, because the config rarely changes during readiness wait.
	AgentConfig string
}

// WaitForIP polls the qemu-guest-agent until it reports the expected IPv4
// address on any interface. After agentFailureBudget elapses without the
// agent responding, we additionally fall back to attempting a TCP connection
// to the IP on port 22 — that confirms cloud-init finished, the network came
// up, and sshd is listening, even on templates without the guest agent.
//
// Returns nil when the IP is reachable, ctx.Err() on deadline, or a wrapped
// error if Proxmox itself rejects our requests. Diagnostics is always
// populated — it captures the last observed agent/TCP state so the caller
// can surface a useful error message instead of "timed out, no idea why".
func WaitForIP(
	ctx context.Context,
	px ProxmoxClient,
	node string,
	vmid int,
	expectedIP string,
	pollInterval time.Duration,
) (WaitDiagnostics, error) {
	if pollInterval <= 0 {
		pollInterval = 3 * time.Second
	}

	diag := WaitDiagnostics{ExpectedIP: expectedIP}
	start := time.Now()
	for {
		// Always try the agent first — it's the fast path.
		ifaces, err := px.GetAgentInterfaces(ctx, node, vmid)
		if err == nil {
			diag.AgentSeen = true
			diag.AgentIPs = listAgentIPs(ifaces)
			if hasIP(ifaces, expectedIP) {
				diag.Elapsed = time.Since(start)
				return diag, nil
			}
		} else if diag.FirstAgentErr == "" && !errors.Is(err, context.DeadlineExceeded) {
			// Capture the FIRST real Proxmox error — once ctx is close
			// to its deadline, every subsequent HTTP request fails with
			// context.DeadlineExceeded which masks the real cause. The
			// shape we want is `"QEMU guest agent is not running"` or
			// `"QEMU guest agent timeout"` — the actual Proxmox message.
			diag.FirstAgentErr = trimAgentError(err.Error())
		}

		// Once the agent has been quiet long enough, also try the TCP fallback.
		if time.Since(start) > agentFailureBudget && tcpReachable(ctx, expectedIP, 22) {
			diag.TCPReachable = true
			diag.Elapsed = time.Since(start)
			return diag, nil
		}

		select {
		case <-ctx.Done():
			diag.Elapsed = time.Since(start)
			return diag, fmt.Errorf("wait for IP %s on vmid %d: %w", expectedIP, vmid, ctx.Err())
		case <-time.After(pollInterval):
		}
	}
}

// Summary returns a one-line, human-readable explanation of the terminal
// state — used in the soft-success warning so the user sees what we
// actually observed instead of "could not confirm".
func (d WaitDiagnostics) Summary() string {
	cfgNote := ""
	if d.AgentConfig == "" {
		cfgNote = " — VM config has no `agent` field set (clone may not be inheriting `agent: enabled=1` from the template)"
	} else if !strings.Contains(d.AgentConfig, "enabled=1") {
		cfgNote = fmt.Sprintf(" — VM config has `agent: %s` (not enabled)", d.AgentConfig)
	}

	switch {
	case !d.AgentSeen && d.FirstAgentErr != "":
		return "qemu-guest-agent never responded (Proxmox said: " + d.FirstAgentErr + ")" + cfgNote
	case !d.AgentSeen:
		// No real error captured AND no successful response — likely
		// every request hit the ctx-deadline or the agent is configured
		// off. Surface the config note prominently.
		return "qemu-guest-agent never responded" + cfgNote
	case len(d.AgentIPs) == 0:
		return "qemu-guest-agent ran but reported no non-loopback IPs (cloud-init network setup likely failed)"
	default:
		return fmt.Sprintf("qemu-guest-agent reported %s on the VM; expected %s — cloud-init may not have applied the configured ipconfig0",
			strings.Join(d.AgentIPs, ", "), d.ExpectedIP)
	}
}

// trimAgentError strips Proxmox's noisy prefix so the surfaced error is
// the actual message rather than `proxmox: GET /nodes/.../agent/... returned 500: ...`.
func trimAgentError(s string) string {
	// Look for the marker Proxmox uses to delimit response body.
	if i := strings.LastIndex(s, ": "); i >= 0 && i < len(s)-2 {
		s = s[i+2:]
	}
	s = strings.TrimSpace(s)
	if len(s) > 200 {
		s = s[:197] + "…"
	}
	return s
}

// listAgentIPs flattens every non-loopback IPv4/IPv6 address the agent
// reported into a sortable []string. Used in diagnostics; loopback is
// filtered the same way hasIP filters it.
func listAgentIPs(ifaces []proxmox.NetworkInterface) []string {
	var out []string
	for _, iface := range ifaces {
		if iface.Name == "lo" || strings.HasPrefix(iface.Name, "lo") {
			continue
		}
		for _, addr := range iface.IPAddresses {
			if addr.IPAddress == "" {
				continue
			}
			out = append(out, addr.IPAddress)
		}
	}
	return out
}

func hasIP(ifaces []proxmox.NetworkInterface, want string) bool {
	for _, iface := range ifaces {
		// Skip loopback to avoid false positives.
		if iface.Name == "lo" || strings.HasPrefix(iface.Name, "lo") {
			continue
		}
		for _, addr := range iface.IPAddresses {
			if addr.IPAddress == want {
				return true
			}
		}
	}
	return false
}

// tcpReachable returns true if a TCP connection to ip:port succeeds within 2
// seconds. Used as the fallback for templates without the guest agent.
func tcpReachable(ctx context.Context, ip string, port int) bool {
	dialer := net.Dialer{Timeout: 2 * time.Second}
	conn, err := dialer.DialContext(ctx, "tcp", net.JoinHostPort(ip, fmt.Sprintf("%d", port)))
	if err != nil {
		// Refused / timeout / unreachable all mean "not yet" — not a logger-worthy event.
		var opErr *net.OpError
		_ = errors.As(err, &opErr)
		return false
	}
	_ = conn.Close()
	return true
}
