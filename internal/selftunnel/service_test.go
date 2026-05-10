package selftunnel

import (
	"context"
	"errors"
	"sync"
	"testing"

	"nimbus/internal/db"
	"nimbus/internal/tunnel"
)

// fakeStore is a settingsStore that holds a single GopherSettings row
// in memory. Tracks Wipe calls so tests can assert.
type fakeStore struct {
	mu        sync.Mutex
	row       db.GopherSettings
	wipes     int
	saveState int
}

func (f *fakeStore) GetGopherSettings() (*db.GopherSettings, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := f.row
	return &out, nil
}

func (f *fakeStore) SaveCloudTunnelState(state db.GopherSettings) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.saveState++
	f.row.CloudMachineID = state.CloudMachineID
	f.row.CloudTunnelID = state.CloudTunnelID
	f.row.CloudTunnelURL = state.CloudTunnelURL
	f.row.CloudBootstrapState = state.CloudBootstrapState
	f.row.CloudBootstrapError = state.CloudBootstrapError
	return nil
}

func (f *fakeStore) WipeGopherSettings() error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.wipes++
	f.row = db.GopherSettings{}
	return nil
}

// fakeGopher records DeleteMachine calls and lets tests script errors.
type fakeGopher struct {
	mu        sync.Mutex
	deleted   []string
	deleteErr error
	// listTunnels scripts the response for ListTunnelsForMachine.
	listTunnels    []tunnel.Tunnel
	listTunnelsErr error
}

func (f *fakeGopher) CreateMachine(_ context.Context, _ tunnel.CreateMachineRequest) (*tunnel.Machine, error) {
	return nil, errors.New("not used in cleanup tests")
}
func (f *fakeGopher) GetMachine(_ context.Context, _ string) (*tunnel.Machine, error) {
	return nil, errors.New("not used in cleanup tests")
}
func (f *fakeGopher) DeleteMachine(_ context.Context, id string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.deleted = append(f.deleted, id)
	return f.deleteErr
}
func (f *fakeGopher) CreateTunnel(_ context.Context, _ tunnel.CreateTunnelRequest) (*tunnel.Tunnel, error) {
	return nil, errors.New("not used in cleanup tests")
}
func (f *fakeGopher) ListTunnelsForMachine(_ context.Context, _ string) ([]tunnel.Tunnel, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.listTunnelsErr != nil {
		return nil, f.listTunnelsErr
	}
	out := make([]tunnel.Tunnel, len(f.listTunnels))
	copy(out, f.listTunnels)
	return out, nil
}
func (f *fakeGopher) ListTunnels(_ context.Context) ([]tunnel.Tunnel, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.listTunnelsErr != nil {
		return nil, f.listTunnelsErr
	}
	out := make([]tunnel.Tunnel, len(f.listTunnels))
	copy(out, f.listTunnels)
	return out, nil
}
func (f *fakeGopher) DeleteTunnel(_ context.Context, _ string) error {
	return nil
}

// TestMarkFailed_DeletesMachineAndWipesRow covers the happy cleanup
// path: a failed bootstrap with a registered Gopher machine deletes
// the machine, wipes every Gopher column, and drops the in-memory
// gopher client so subsequent calls can't 401 against stale creds.
func TestMarkFailed_DeletesMachineAndWipesRow(t *testing.T) {
	t.Parallel()
	store := &fakeStore{row: db.GopherSettings{
		APIURL:         "https://g.example",
		APIKey:         "secret",
		CloudSubdomain: "cloud",
		CloudMachineID: "m-42",
		CloudTunnelID:  "t-7",
	}}
	gopher := &fakeGopher{}
	svc := &Service{store: store, gopher: gopher}

	svc.markFailed("bootstrap script blew up")

	if got := len(gopher.deleted); got != 1 || gopher.deleted[0] != "m-42" {
		t.Errorf("DeleteMachine called %v, want [m-42]", gopher.deleted)
	}
	if store.wipes != 1 {
		t.Errorf("WipeGopherSettings called %d times, want 1", store.wipes)
	}
	if store.row != (db.GopherSettings{}) {
		t.Errorf("row not wiped: %+v", store.row)
	}
	if svc.gopher != nil {
		t.Errorf("gopher client not cleared after wipe")
	}
}

// TestMarkFailed_NoMachineRegistered_StillWipes covers an early
// failure (e.g. checkSudo fails before CreateMachine). No machine to
// delete, but credentials should still be wiped.
func TestMarkFailed_NoMachineRegistered_StillWipes(t *testing.T) {
	t.Parallel()
	store := &fakeStore{row: db.GopherSettings{
		APIURL: "https://g.example",
		APIKey: "secret",
	}}
	gopher := &fakeGopher{}
	svc := &Service{store: store, gopher: gopher}

	svc.markFailed("sudo blocked")

	if got := len(gopher.deleted); got != 0 {
		t.Errorf("DeleteMachine should not be called when CloudMachineID is empty, got %v", gopher.deleted)
	}
	if store.wipes != 1 {
		t.Errorf("WipeGopherSettings called %d times, want 1", store.wipes)
	}
}

// TestMarkFailed_DeleteMachineError_StillWipes covers a Gopher-side
// error during cleanup. The DB wipe must still happen — leaving the
// row with stale state is worse than orphaning a machine on Gopher.
func TestMarkFailed_DeleteMachineError_StillWipes(t *testing.T) {
	t.Parallel()
	store := &fakeStore{row: db.GopherSettings{
		APIURL:         "https://g.example",
		APIKey:         "secret",
		CloudMachineID: "m-99",
	}}
	gopher := &fakeGopher{deleteErr: errors.New("gopher 500")}
	svc := &Service{store: store, gopher: gopher}

	svc.markFailed("creating tunnel timed out")

	if store.wipes != 1 {
		t.Errorf("WipeGopherSettings should run even when DeleteMachine errors; got %d", store.wipes)
	}
	if svc.gopher != nil {
		t.Errorf("gopher client should be cleared even when DeleteMachine errors")
	}
}

// TestAdoptCreatedTunnel covers the timeout-recovery path. CreateTunnel
// can succeed server-side and surface a client-side error (most often
// http.Client timeout under load); without adoption we'd tear down the
// tunnel that was actually created. The match contract:
//   - same machine, same target_port required
//   - subdomain "" in the request matches anything
//   - non-empty subdomain must match exactly
//
// Returning (nil, nil) means "no match — caller should fall through to
// the existing failure path."
func TestAdoptCreatedTunnel(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name      string
		list      []tunnel.Tunnel
		listErr   error
		machineID string
		subdomain string
		port      int
		wantID    string // "" expects nil tunnel
		wantErr   bool
	}{
		{
			name:      "empty list → no adoption",
			machineID: "m-1",
			port:      8080,
			wantID:    "",
		},
		{
			name: "match on machine+port (no subdomain pinned)",
			list: []tunnel.Tunnel{
				{ID: "t-1", MachineID: "m-1", TargetPort: 8080, Subdomain: "cloud"},
			},
			machineID: "m-1",
			port:      8080,
			wantID:    "t-1",
		},
		{
			name: "match on machine+port+subdomain",
			list: []tunnel.Tunnel{
				{ID: "t-2", MachineID: "m-1", TargetPort: 8080, Subdomain: "cloud"},
			},
			machineID: "m-1",
			subdomain: "cloud",
			port:      8080,
			wantID:    "t-2",
		},
		{
			name: "wrong port → no adoption",
			list: []tunnel.Tunnel{
				{ID: "t-3", MachineID: "m-1", TargetPort: 9090, Subdomain: "cloud"},
			},
			machineID: "m-1",
			port:      8080,
			wantID:    "",
		},
		{
			name: "subdomain pinned but tunnel uses a different one → no adoption",
			list: []tunnel.Tunnel{
				{ID: "t-4", MachineID: "m-1", TargetPort: 8080, Subdomain: "other"},
			},
			machineID: "m-1",
			subdomain: "cloud",
			port:      8080,
			wantID:    "",
		},
		{
			name: "first matching wins when multiple match",
			list: []tunnel.Tunnel{
				{ID: "t-5", MachineID: "m-1", TargetPort: 8080, Subdomain: "cloud"},
				{ID: "t-6", MachineID: "m-1", TargetPort: 8080, Subdomain: "cloud"},
			},
			machineID: "m-1",
			subdomain: "cloud",
			port:      8080,
			wantID:    "t-5",
		},
		{
			name:      "list error propagates",
			listErr:   errors.New("gopher 503"),
			machineID: "m-1",
			port:      8080,
			wantErr:   true,
		},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			gopher := &fakeGopher{listTunnels: tc.list, listTunnelsErr: tc.listErr}
			svc := &Service{gopher: gopher}
			got, err := svc.adoptCreatedTunnel(context.Background(), tc.machineID, tc.subdomain, tc.port)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("expected error, got nil")
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if tc.wantID == "" {
				if got != nil {
					t.Errorf("expected nil adoption, got %+v", got)
				}
				return
			}
			if got == nil {
				t.Fatalf("expected tunnel %s, got nil", tc.wantID)
			}
			if got.ID != tc.wantID {
				t.Errorf("got tunnel %s, want %s", got.ID, tc.wantID)
			}
		})
	}
}

// recordingGopher extends fakeGopher with capture+script support for
// the takeover-on-409 path. CreateTunnel returns a scripted error the
// first time and a scripted result on the retry; DeleteTunnel records
// the id deleted so the test can assert the conflicting tunnel was
// removed before the retry.
type recordingGopher struct {
	fakeGopher
	createCalls    int
	createErrFirst error
	createResult   *tunnel.Tunnel
	deletedTunnels []string
}

func (g *recordingGopher) CreateTunnel(_ context.Context, _ tunnel.CreateTunnelRequest) (*tunnel.Tunnel, error) {
	g.mu.Lock()
	g.createCalls++
	call := g.createCalls
	g.mu.Unlock()
	if call == 1 && g.createErrFirst != nil {
		return nil, g.createErrFirst
	}
	return g.createResult, nil
}
func (g *recordingGopher) DeleteTunnel(_ context.Context, id string) error {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.deletedTunnels = append(g.deletedTunnels, id)
	return nil
}

// TestReplaceConflictingTunnel covers the takeover path: when
// CreateTunnel returns 409 "subdomain already exists", we list every
// tunnel cluster-wide, delete the one bound to that subdomain, and
// retry under our machine. Operator workflow: they manually set up a
// Gopher tunnel earlier, now want Nimbus to take it over by saving
// the same subdomain.
func TestReplaceConflictingTunnel_DeletesOrphanAndRetries(t *testing.T) {
	t.Parallel()
	conflict := tunnel.Tunnel{
		ID:         "t-orphan",
		MachineID:  "m-old",
		Subdomain:  "nimbus",
		TargetPort: 8080,
	}
	created := &tunnel.Tunnel{
		ID:         "t-new",
		MachineID:  "m-new",
		Subdomain:  "nimbus",
		TargetPort: 8080,
		TunnelURL:  "https://nimbus.altsuite.co",
	}
	g := &recordingGopher{
		fakeGopher: fakeGopher{
			listTunnels: []tunnel.Tunnel{conflict},
		},
		createResult: created,
	}
	svc := &Service{gopher: g, nimbusPort: 8080}

	got, err := svc.replaceConflictingTunnel(context.Background(), "m-new", "nimbus", 8080)
	if err != nil {
		t.Fatalf("replaceConflictingTunnel: %v", err)
	}
	if got == nil || got.ID != "t-new" {
		t.Fatalf("expected new tunnel t-new, got %+v", got)
	}
	if len(g.deletedTunnels) != 1 || g.deletedTunnels[0] != "t-orphan" {
		t.Errorf("expected DeleteTunnel(t-orphan), got %v", g.deletedTunnels)
	}
	if g.createCalls != 1 {
		t.Errorf("createCalls = %d, want 1 (the retry)", g.createCalls)
	}
}

// TestReplaceConflictingTunnel_NoMatch surfaces the bail path: 409
// happened but our list doesn't show the conflicting subdomain (race
// or scope mismatch). Surface the situation rather than blindly retry
// into another 409.
func TestReplaceConflictingTunnel_NoMatch_Errors(t *testing.T) {
	t.Parallel()
	g := &recordingGopher{
		fakeGopher: fakeGopher{listTunnels: []tunnel.Tunnel{}},
	}
	svc := &Service{gopher: g, nimbusPort: 8080}

	_, err := svc.replaceConflictingTunnel(context.Background(), "m-new", "nimbus", 8080)
	if err == nil {
		t.Fatal("expected error when conflict not found in tunnel list")
	}
	if len(g.deletedTunnels) != 0 {
		t.Errorf("should not have deleted anything when no conflict found; deleted=%v", g.deletedTunnels)
	}
}

// TestIsSubdomainConflict covers the predicate that decides whether
// to enter the takeover path.
func TestIsSubdomainConflict(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		err  error
		want bool
	}{
		{"409 subdomain", &tunnel.HTTPError{Status: 409, Body: "subdomain already exists"}, true},
		{"409 case-insensitive", &tunnel.HTTPError{Status: 409, Body: "Subdomain conflict"}, true},
		{"409 not subdomain", &tunnel.HTTPError{Status: 409, Body: "machine not found"}, false},
		{"500 subdomain", &tunnel.HTTPError{Status: 500, Body: "subdomain database error"}, false},
		{"plain error", errors.New("something else"), false},
		{"nil", nil, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := isSubdomainConflict(tc.err); got != tc.want {
				t.Errorf("isSubdomainConflict(%v) = %v, want %v", tc.err, got, tc.want)
			}
		})
	}
}
