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
