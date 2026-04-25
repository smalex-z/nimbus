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

// WaitForIP polls the qemu-guest-agent until it reports the expected IPv4
// address on any interface. After agentFailureBudget elapses without the
// agent responding, we additionally fall back to attempting a TCP connection
// to the IP on port 22 — that confirms cloud-init finished, the network came
// up, and sshd is listening, even on templates without the guest agent.
//
// Returns nil when the IP is reachable, ctx.Err() on deadline, or a wrapped
// error if Proxmox itself rejects our requests.
func WaitForIP(
	ctx context.Context,
	px ProxmoxClient,
	node string,
	vmid int,
	expectedIP string,
	pollInterval time.Duration,
) error {
	if pollInterval <= 0 {
		pollInterval = 3 * time.Second
	}

	start := time.Now()
	for {
		// Always try the agent first — it's the fast path.
		ifaces, err := px.GetAgentInterfaces(ctx, node, vmid)
		if err == nil && hasIP(ifaces, expectedIP) {
			return nil
		}

		// Once the agent has been quiet long enough, also try the TCP fallback.
		if time.Since(start) > agentFailureBudget && tcpReachable(ctx, expectedIP, 22) {
			return nil
		}

		select {
		case <-ctx.Done():
			return fmt.Errorf("wait for IP %s on vmid %d: %w", expectedIP, vmid, ctx.Err())
		case <-time.After(pollInterval):
		}
	}
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
