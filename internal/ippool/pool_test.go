package ippool_test

import (
	"context"
	"errors"
	"path/filepath"
	"sync"
	"testing"

	"nimbus/internal/db"
	"nimbus/internal/ippool"
)

// newTestPool returns a Pool backed by a fresh on-disk SQLite file scoped to
// the test's temporary directory. We use a file (not :memory:) so the
// single-connection pool from db.New behaves identically to production and
// each test gets full isolation.
func newTestPool(t *testing.T) *ippool.Pool {
	t.Helper()
	path := filepath.Join(t.TempDir(), "test.db")
	database, err := db.New(path, ippool.Model())
	if err != nil {
		t.Fatalf("db.New: %v", err)
	}
	return ippool.New(database.DB)
}

func TestSeed(t *testing.T) {
	t.Parallel()

	t.Run("creates exactly the requested addresses", func(t *testing.T) {
		p := newTestPool(t)
		if err := p.Seed(context.Background(), "10.0.0.10", "10.0.0.12"); err != nil {
			t.Fatalf("Seed: %v", err)
		}
		got, err := p.List(context.Background())
		if err != nil {
			t.Fatalf("List: %v", err)
		}
		want := []string{"10.0.0.10", "10.0.0.11", "10.0.0.12"}
		if len(got) != len(want) {
			t.Fatalf("got %d rows, want %d", len(got), len(want))
		}
		for i, ip := range want {
			if got[i].IP != ip || got[i].Status != ippool.StatusFree {
				t.Errorf("row %d: got %+v, want IP=%s status=free", i, got[i], ip)
			}
		}
	})

	t.Run("idempotent across multiple seeds (same range)", func(t *testing.T) {
		p := newTestPool(t)
		ctx := context.Background()
		for i := 0; i < 3; i++ {
			if err := p.Seed(ctx, "10.0.0.1", "10.0.0.5"); err != nil {
				t.Fatalf("Seed iteration %d: %v", i, err)
			}
		}
		got, _ := p.List(ctx)
		if len(got) != 5 {
			t.Errorf("got %d rows after 3 seeds, want 5", len(got))
		}
	})

	t.Run("idempotent when expanding range", func(t *testing.T) {
		p := newTestPool(t)
		ctx := context.Background()
		_ = p.Seed(ctx, "10.0.0.1", "10.0.0.5")

		// Reserve one to prove existing rows are preserved.
		if _, err := p.Reserve(ctx, "host1"); err != nil {
			t.Fatalf("Reserve: %v", err)
		}

		// Expand the pool — existing rows must keep their state.
		if err := p.Seed(ctx, "10.0.0.1", "10.0.0.10"); err != nil {
			t.Fatalf("Seed expand: %v", err)
		}
		got, _ := p.List(ctx)
		if len(got) != 10 {
			t.Errorf("got %d rows after expand, want 10", len(got))
		}
		var reservedCount int
		for _, r := range got {
			if r.Status == ippool.StatusReserved {
				reservedCount++
			}
		}
		if reservedCount != 1 {
			t.Errorf("expected 1 reserved row preserved, got %d", reservedCount)
		}
	})

	t.Run("rejects inverted range", func(t *testing.T) {
		p := newTestPool(t)
		err := p.Seed(context.Background(), "10.0.0.10", "10.0.0.5")
		if !errors.Is(err, ippool.ErrInvalidRange) {
			t.Errorf("got %v, want ErrInvalidRange", err)
		}
	})

	t.Run("rejects malformed input", func(t *testing.T) {
		p := newTestPool(t)
		err := p.Seed(context.Background(), "not-an-ip", "10.0.0.5")
		if !errors.Is(err, ippool.ErrInvalidRange) {
			t.Errorf("got %v, want ErrInvalidRange", err)
		}
	})
}

func TestReserve(t *testing.T) {
	t.Parallel()

	t.Run("returns lowest free IP", func(t *testing.T) {
		p := newTestPool(t)
		ctx := context.Background()
		_ = p.Seed(ctx, "10.0.0.1", "10.0.0.3")
		ip, err := p.Reserve(ctx, "first-host")
		if err != nil {
			t.Fatalf("Reserve: %v", err)
		}
		if ip != "10.0.0.1" {
			t.Errorf("got %s, want 10.0.0.1", ip)
		}
		ip2, _ := p.Reserve(ctx, "second-host")
		if ip2 != "10.0.0.2" {
			t.Errorf("second reserve got %s, want 10.0.0.2", ip2)
		}
	})

	t.Run("ErrPoolExhausted when nothing free", func(t *testing.T) {
		p := newTestPool(t)
		ctx := context.Background()
		_ = p.Seed(ctx, "10.0.0.1", "10.0.0.2")
		_, _ = p.Reserve(ctx, "a")
		_, _ = p.Reserve(ctx, "b")
		_, err := p.Reserve(ctx, "c")
		if !errors.Is(err, ippool.ErrPoolExhausted) {
			t.Errorf("got %v, want ErrPoolExhausted", err)
		}
	})

	t.Run("concurrent reservations get distinct IPs", func(t *testing.T) {
		p := newTestPool(t)
		ctx := context.Background()
		_ = p.Seed(ctx, "10.0.0.1", "10.0.0.20")

		const N = 20
		var wg sync.WaitGroup
		ips := make(chan string, N)
		errs := make(chan error, N)
		for i := 0; i < N; i++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				ip, err := p.Reserve(ctx, "concurrent")
				if err != nil {
					errs <- err
					return
				}
				ips <- ip
			}()
		}
		wg.Wait()
		close(ips)
		close(errs)

		seen := make(map[string]bool)
		for ip := range ips {
			if seen[ip] {
				t.Errorf("duplicate IP allocated: %s", ip)
			}
			seen[ip] = true
		}
		if len(seen) != N {
			t.Errorf("got %d distinct IPs, want %d", len(seen), N)
		}
		for err := range errs {
			t.Errorf("unexpected concurrent error: %v", err)
		}
	})
}

func TestMarkAllocatedAndRelease(t *testing.T) {
	t.Parallel()
	p := newTestPool(t)
	ctx := context.Background()
	_ = p.Seed(ctx, "10.0.0.1", "10.0.0.3")

	ip, _ := p.Reserve(ctx, "host-x")

	if err := p.MarkAllocated(ctx, ip, 100); err != nil {
		t.Fatalf("MarkAllocated: %v", err)
	}
	rows, _ := p.List(ctx)
	var allocRow ippool.IPAllocation
	for _, r := range rows {
		if r.IP == ip {
			allocRow = r
		}
	}
	if allocRow.Status != ippool.StatusAllocated {
		t.Errorf("status=%s, want allocated", allocRow.Status)
	}
	if allocRow.VMID == nil || *allocRow.VMID != 100 {
		t.Errorf("vmid=%v, want 100", allocRow.VMID)
	}

	if err := p.Release(ctx, ip); err != nil {
		t.Fatalf("Release: %v", err)
	}
	rows, _ = p.List(ctx)
	for _, r := range rows {
		if r.IP == ip {
			if r.Status != ippool.StatusFree {
				t.Errorf("after Release status=%s, want free", r.Status)
			}
			if r.VMID != nil || r.Hostname != nil {
				t.Errorf("after Release expected nil metadata, got vmid=%v hostname=%v", r.VMID, r.Hostname)
			}
		}
	}

	t.Run("MarkAllocated on unreserved IP errors", func(t *testing.T) {
		err := p.MarkAllocated(ctx, "10.0.0.99", 999)
		if err == nil {
			t.Errorf("expected error for unknown IP")
		}
	})

	t.Run("Release on already-free IP is a no-op", func(t *testing.T) {
		if err := p.Release(ctx, "10.0.0.2"); err != nil {
			t.Errorf("expected no-op, got %v", err)
		}
	})
}
