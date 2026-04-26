// Package netscan probes a list of candidate IPs and reports which ones are
// in use somewhere on the local network. It exists to close a hole in the IP
// reconciler: the Proxmox walk only sees IPs claimed by VMs (and now LXCs),
// so anything else on the LAN — physical hosts, gateways, NAS boxes,
// statically-assigned workstations, IoT — would be invisible and at risk of
// double-allocation if the Nimbus IP pool overlaps their addresses.
//
// Detection strategy — pure stdlib, no privileges:
//
//  1. TCP probe: dial one common port per candidate IP with a short timeout.
//     A connect or "connection refused" both prove a host is at that IP.
//     This works across L3 boundaries (router-traversed) so it covers the
//     case where Nimbus runs in a different VLAN than the pool range.
//
//  2. ARP cache read (Linux only): the kernel had to ARP-resolve every
//     L2-local destination during step 1, so /proc/net/arp now contains an
//     entry for every reachable host on the same subnet — including ones
//     that silently dropped our SYNs. Reading the cache costs nothing and
//     catches firewalled hosts.
//
// Together these two signals match the coverage of an active raw-socket ARP
// scan (which would need CAP_NET_RAW) without elevating privileges.
package netscan

import (
	"context"
	"net"
	"sort"
	"strconv"
	"sync"
	"time"
)

// Mode chooses which sub-scanners participate in a Scan.
type Mode string

const (
	ModeOff  Mode = "off"  // no scan; loop is disabled
	ModeTCP  Mode = "tcp"  // TCP probe only — skip ARP cache read
	ModeBoth Mode = "both" // TCP probe + ARP cache read; default
)

// ParseMode normalizes a config string. Unknown / empty values default to "both".
func ParseMode(s string) Mode {
	switch s {
	case "off":
		return ModeOff
	case "tcp":
		return ModeTCP
	case "both", "":
		return ModeBoth
	default:
		return ModeBoth
	}
}

// Config tunes the scanner.
type Config struct {
	Mode        Mode
	TCPPorts    []int         // ports probed per candidate (default: 22, 80, 443, 445)
	TCPTimeout  time.Duration // per-port dial timeout (default: 200ms)
	Concurrency int           // simultaneous in-flight probes (default: 50)
}

// Scanner returns the subset of candidates that appear to be in use.
type Scanner interface {
	// Scan probes every IP in candidates and returns those that responded.
	// Order is unspecified. A nil result with nil error means "no responses".
	// An error means the scan didn't run usefully — callers should treat
	// the result as untrusted (not "all free").
	Scan(ctx context.Context, candidates []net.IP) ([]net.IP, error)
}

// New constructs a Scanner for the given mode. Returns a no-op scanner for
// ModeOff so callers can keep their wiring uniform.
func New(cfg Config) Scanner {
	cfg = cfg.withDefaults()
	if cfg.Mode == ModeOff {
		return offScanner{}
	}
	return &scanner{cfg: cfg}
}

func (c Config) withDefaults() Config {
	if len(c.TCPPorts) == 0 {
		c.TCPPorts = []int{22, 80, 443, 445}
	}
	if c.TCPTimeout <= 0 {
		c.TCPTimeout = 200 * time.Millisecond
	}
	if c.Concurrency <= 0 {
		c.Concurrency = 50
	}
	return c
}

type offScanner struct{}

func (offScanner) Scan(_ context.Context, _ []net.IP) ([]net.IP, error) { return nil, nil }

type scanner struct {
	cfg Config
}

func (s *scanner) Scan(ctx context.Context, candidates []net.IP) ([]net.IP, error) {
	if len(candidates) == 0 {
		return nil, nil
	}
	inUse := s.tcpProbe(ctx, candidates)
	if s.cfg.Mode == ModeBoth {
		// readARPCache is platform-specific; non-Linux returns nil.
		for ip := range readARPCache() {
			inUse[ip] = true
		}
	}
	out := make([]net.IP, 0, len(inUse))
	for s := range inUse {
		out = append(out, net.ParseIP(s))
	}
	sort.Slice(out, func(i, j int) bool {
		return compareIP(out[i], out[j]) < 0
	})
	return out, nil
}

// tcpProbe dials each candidate's TCP ports concurrently. An IP is marked
// in-use the moment any port either accepts the connection or refuses it
// (refused == "host is up, port is closed" — both confirm presence).
//
// Returns a set keyed by the canonical 4-byte-or-16-byte form's String().
func (s *scanner) tcpProbe(ctx context.Context, candidates []net.IP) map[string]bool {
	type result struct {
		ip    string
		inUse bool
	}
	results := make(chan result, len(candidates))
	sem := make(chan struct{}, s.cfg.Concurrency)
	var wg sync.WaitGroup

	for _, ip := range candidates {
		ip := ip
		wg.Add(1)
		sem <- struct{}{}
		go func() {
			defer wg.Done()
			defer func() { <-sem }()
			results <- result{ip: ip.String(), inUse: s.probeOne(ctx, ip)}
		}()
	}

	go func() {
		wg.Wait()
		close(results)
	}()

	out := make(map[string]bool, len(candidates))
	for r := range results {
		if r.inUse {
			out[r.ip] = true
		}
	}
	return out
}

// probeOne dials each configured port in turn. Stops at the first signal of
// life (connect or refused). Other errors (timeout, host-unreachable) move
// on to the next port.
func (s *scanner) probeOne(ctx context.Context, ip net.IP) bool {
	dialer := net.Dialer{Timeout: s.cfg.TCPTimeout}
	for _, port := range s.cfg.TCPPorts {
		select {
		case <-ctx.Done():
			return false
		default:
		}
		addr := net.JoinHostPort(ip.String(), strconv.Itoa(port))
		conn, err := dialer.DialContext(ctx, "tcp", addr)
		if err == nil {
			_ = conn.Close()
			return true
		}
		if isConnRefused(err) {
			return true
		}
	}
	return false
}

// compareIP orders IPs by big-endian bytes — same convention the pool uses.
func compareIP(a, b net.IP) int {
	a, b = a.To16(), b.To16()
	for i := range a {
		if a[i] != b[i] {
			return int(a[i]) - int(b[i])
		}
	}
	return 0
}
