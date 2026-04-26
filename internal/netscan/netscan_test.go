package netscan_test

import (
	"context"
	"net"
	"testing"
	"time"

	"nimbus/internal/netscan"
)

func TestParseMode(t *testing.T) {
	t.Parallel()
	tests := map[string]netscan.Mode{
		"":            netscan.ModeBoth,
		"both":        netscan.ModeBoth,
		"tcp":         netscan.ModeTCP,
		"off":         netscan.ModeOff,
		"weird-value": netscan.ModeBoth,
	}
	for in, want := range tests {
		if got := netscan.ParseMode(in); got != want {
			t.Errorf("ParseMode(%q) = %s, want %s", in, got, want)
		}
	}
}

// TestScanner_TCPProbe_FindsListeningHost spins up a real TCP listener on a
// localhost port and confirms the scanner reports the IP as in-use.
func TestScanner_TCPProbe_FindsListeningHost(t *testing.T) {
	t.Parallel()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer listener.Close() //nolint:errcheck

	port := listener.Addr().(*net.TCPAddr).Port

	s := netscan.New(netscan.Config{
		Mode:        netscan.ModeTCP,
		TCPPorts:    []int{port},
		TCPTimeout:  500 * time.Millisecond,
		Concurrency: 4,
	})

	candidates := []net.IP{net.ParseIP("127.0.0.1")}
	got, err := s.Scan(context.Background(), candidates)
	if err != nil {
		t.Fatalf("Scan: %v", err)
	}
	if len(got) != 1 || !got[0].Equal(net.ParseIP("127.0.0.1")) {
		t.Errorf("Scan() = %v, want [127.0.0.1]", got)
	}
}

// TestScanner_TCPProbe_RefusedAlsoCounts confirms a "connection refused"
// (host present, port closed) is treated as proof of presence.
func TestScanner_TCPProbe_RefusedAlsoCounts(t *testing.T) {
	t.Parallel()
	// Port that's almost certainly closed on localhost — kernel will refuse.
	// Using a high random port reduces the chance of accidentally hitting
	// something a co-tenant test set up.
	const refusedPort = 1
	s := netscan.New(netscan.Config{
		Mode:        netscan.ModeTCP,
		TCPPorts:    []int{refusedPort},
		TCPTimeout:  500 * time.Millisecond,
		Concurrency: 4,
	})

	got, err := s.Scan(context.Background(), []net.IP{net.ParseIP("127.0.0.1")})
	if err != nil {
		t.Fatalf("Scan: %v", err)
	}
	if len(got) != 1 {
		t.Errorf("Scan() = %v, want [127.0.0.1] (refused counts)", got)
	}
}

// TestScanner_OffMode is the tightest no-op contract — no probes regardless
// of input, no error.
func TestScanner_OffMode(t *testing.T) {
	t.Parallel()
	s := netscan.New(netscan.Config{Mode: netscan.ModeOff})
	got, err := s.Scan(context.Background(), []net.IP{net.ParseIP("127.0.0.1")})
	if err != nil {
		t.Fatalf("Scan: %v", err)
	}
	if got != nil {
		t.Errorf("ModeOff returned %v, want nil", got)
	}
}

// TestScanner_EmptyCandidates short-circuits without doing any work.
func TestScanner_EmptyCandidates(t *testing.T) {
	t.Parallel()
	s := netscan.New(netscan.Config{Mode: netscan.ModeTCP})
	got, err := s.Scan(context.Background(), nil)
	if err != nil {
		t.Fatalf("Scan: %v", err)
	}
	if got != nil {
		t.Errorf("empty candidates returned %v, want nil", got)
	}
}

// TestVerifier_FreeWhenScannerSilent — Scanner returns no hits → VerifyFree
// reports the IP as free, holder=nil.
func TestVerifier_FreeWhenScannerSilent(t *testing.T) {
	t.Parallel()
	v := netscan.NewVerifier(silentScanner{})
	free, holder, err := v.VerifyFree(context.Background(), "10.0.0.99")
	if err != nil {
		t.Fatalf("VerifyFree: %v", err)
	}
	if !free {
		t.Errorf("free = false, want true")
	}
	if holder != nil {
		t.Errorf("holder = %v, want nil", holder)
	}
}

// TestVerifier_NotFreeWhenScannerHits — Scanner finds the IP → VerifyFree
// reports not-free with holder=nil (we don't know which device).
func TestVerifier_NotFreeWhenScannerHits(t *testing.T) {
	t.Parallel()
	v := netscan.NewVerifier(stubScanner{hits: []net.IP{net.ParseIP("10.0.0.99")}})
	free, holder, err := v.VerifyFree(context.Background(), "10.0.0.99")
	if err != nil {
		t.Fatalf("VerifyFree: %v", err)
	}
	if free {
		t.Errorf("free = true, want false")
	}
	if holder != nil {
		t.Errorf("holder = %v, want nil", holder)
	}
}

// silentScanner / stubScanner are minimal Scanner stubs for verifier tests.
type silentScanner struct{}

func (silentScanner) Scan(_ context.Context, _ []net.IP) ([]net.IP, error) { return nil, nil }

type stubScanner struct{ hits []net.IP }

func (s stubScanner) Scan(_ context.Context, _ []net.IP) ([]net.IP, error) { return s.hits, nil }
