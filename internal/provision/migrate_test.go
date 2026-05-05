package provision_test

import (
	"context"
	"errors"
	"strings"
	"sync/atomic"
	"testing"

	"nimbus/internal/db"
	internalerrors "nimbus/internal/errors"
	"nimbus/internal/proxmox"
)

// seedVM inserts a single VM row in the test DB and returns its row id.
// All tests use the same shape — running on "alpha" by default — so the
// migrate target tests can flip vm.Status to exercise the offline path.
func seedVM(t *testing.T, database *db.DB, status string) uint {
	t.Helper()
	row := db.VM{
		VMID:     200,
		Hostname: "test-host",
		IP:       "10.0.0.1",
		Node:     "alpha",
		Tier:     "small",
		Status:   status,
	}
	if err := database.Create(&row).Error; err != nil {
		t.Fatalf("seed vm: %v", err)
	}
	return row.ID
}

// migrateFakePVE returns a fakePVE with two online nodes ("alpha" and
// "beta") and migrate/lifecycle stubs that the per-test setup can
// override. Every other ProxmoxClient method is wired to its no-op
// default — these tests don't need them.
func migrateFakePVE(t *testing.T) *fakePVE {
	t.Helper()
	f := happyFakePVE(t)
	f.getNodes = func(_ context.Context) ([]proxmox.Node, error) {
		return []proxmox.Node{
			{Name: "alpha", Status: "online", MaxMem: 16 << 30, MaxCPU: 8},
			{Name: "beta", Status: "online", MaxMem: 16 << 30, MaxCPU: 8},
		}, nil
	}
	return f
}

func TestMigrateAdmin_OnlineHappyPath(t *testing.T) {
	t.Parallel()
	fake := migrateFakePVE(t)

	var migrateOnline atomic.Int32
	fake.migrateVM = func(_ context.Context, src string, vmid int, target string, online bool) (string, error) {
		if online {
			migrateOnline.Add(1)
		}
		if src != "alpha" || target != "beta" || vmid != 200 {
			t.Errorf("unexpected migrate args: src=%s target=%s vmid=%d", src, target, vmid)
		}
		return "task:migrate", nil
	}

	svc, _, database := newTestService(t, fake)
	id := seedVM(t, database, "running")

	res, err := svc.MigrateAdmin(context.Background(), id, "beta", false)
	if err != nil {
		t.Fatalf("MigrateAdmin: %v", err)
	}
	if res.Mode != "online" || res.TargetNode != "beta" || res.WasStopped {
		t.Errorf("unexpected result: %+v", res)
	}
	if migrateOnline.Load() != 1 {
		t.Errorf("expected one online migrate call, got %d", migrateOnline.Load())
	}

	var got db.VM
	if err := database.First(&got, id).Error; err != nil {
		t.Fatalf("reload vm: %v", err)
	}
	if got.Node != "beta" {
		t.Errorf("DB node not updated: %s", got.Node)
	}
}

func TestMigrateAdmin_SameNodeRejected(t *testing.T) {
	t.Parallel()
	fake := migrateFakePVE(t)
	svc, _, database := newTestService(t, fake)
	id := seedVM(t, database, "running")

	_, err := svc.MigrateAdmin(context.Background(), id, "alpha", false)
	var conflict *internalerrors.ConflictError
	if !errors.As(err, &conflict) {
		t.Fatalf("expected ConflictError, got %T %v", err, err)
	}
}

func TestMigrateAdmin_TargetOfflineRejected(t *testing.T) {
	t.Parallel()
	fake := migrateFakePVE(t)
	fake.getNodes = func(_ context.Context) ([]proxmox.Node, error) {
		return []proxmox.Node{
			{Name: "alpha", Status: "online", MaxMem: 16 << 30, MaxCPU: 8},
			{Name: "beta", Status: "offline", MaxMem: 16 << 30, MaxCPU: 8},
		}, nil
	}
	svc, _, database := newTestService(t, fake)
	id := seedVM(t, database, "running")

	_, err := svc.MigrateAdmin(context.Background(), id, "beta", false)
	var conflict *internalerrors.ConflictError
	if !errors.As(err, &conflict) {
		t.Fatalf("expected ConflictError for offline target, got %T %v", err, err)
	}
	if !strings.Contains(conflict.Error(), "offline") {
		t.Errorf("conflict message should mention offline status, got: %s", conflict.Error())
	}
}

func TestMigrateAdmin_TargetMissingFromCluster(t *testing.T) {
	t.Parallel()
	fake := migrateFakePVE(t)
	svc, _, database := newTestService(t, fake)
	id := seedVM(t, database, "running")

	_, err := svc.MigrateAdmin(context.Background(), id, "ghost", false)
	var notFound *internalerrors.NotFoundError
	if !errors.As(err, &notFound) {
		t.Fatalf("expected NotFoundError for missing target, got %T %v", err, err)
	}
	if notFound.Resource != "node" {
		t.Errorf("expected Resource=node, got %s", notFound.Resource)
	}
}

func TestMigrateAdmin_OnlineFailsWithoutFallback(t *testing.T) {
	t.Parallel()
	fake := migrateFakePVE(t)
	fake.migrateVM = func(_ context.Context, _ string, _ int, _ string, online bool) (string, error) {
		if online {
			return "", errors.New("online migration impossible: VM has a snapshot")
		}
		t.Errorf("migrateVM called with online=false despite allow_offline=false")
		return "task:migrate", nil
	}

	svc, _, database := newTestService(t, fake)
	id := seedVM(t, database, "running")

	_, err := svc.MigrateAdmin(context.Background(), id, "beta", false)
	var onlineFail *internalerrors.OnlineMigrationFailedError
	if !errors.As(err, &onlineFail) {
		t.Fatalf("expected OnlineMigrationFailedError, got %T %v", err, err)
	}
	if !strings.Contains(onlineFail.Reason, "snapshot") {
		t.Errorf("expected reason to surface upstream message, got: %s", onlineFail.Reason)
	}
}

func TestMigrateAdmin_OnlineFailsWithFallback_StopMigrateStart(t *testing.T) {
	t.Parallel()
	fake := migrateFakePVE(t)

	var (
		shutdownCalls atomic.Int32
		startCalls    atomic.Int32
		offlineMig    atomic.Int32
		startTarget   atomic.Value
	)
	fake.migrateVM = func(_ context.Context, _ string, _ int, _ string, online bool) (string, error) {
		if online {
			return "", errors.New("online migration impossible: VM has a snapshot")
		}
		offlineMig.Add(1)
		return "task:migrate-offline", nil
	}
	fake.shutdownVM = func(_ context.Context, _ string, _ int) (string, error) {
		shutdownCalls.Add(1)
		return "task:shutdown", nil
	}
	fake.startVM = func(_ context.Context, target string, _ int) (string, error) {
		startCalls.Add(1)
		startTarget.Store(target)
		return "task:start", nil
	}

	svc, _, database := newTestService(t, fake)
	id := seedVM(t, database, "running")

	res, err := svc.MigrateAdmin(context.Background(), id, "beta", true)
	if err != nil {
		t.Fatalf("MigrateAdmin with allow_offline=true: %v", err)
	}
	if res.Mode != "offline" || !res.WasStopped {
		t.Errorf("unexpected result: %+v", res)
	}
	if shutdownCalls.Load() != 1 {
		t.Errorf("expected 1 shutdown, got %d", shutdownCalls.Load())
	}
	if offlineMig.Load() != 1 {
		t.Errorf("expected 1 offline migrate, got %d", offlineMig.Load())
	}
	if startCalls.Load() != 1 {
		t.Errorf("expected 1 start, got %d", startCalls.Load())
	}
	if got, _ := startTarget.Load().(string); got != "beta" {
		t.Errorf("expected start on target=beta, got %q", got)
	}

	var row db.VM
	if err := database.First(&row, id).Error; err != nil {
		t.Fatalf("reload vm: %v", err)
	}
	if row.Node != "beta" {
		t.Errorf("DB node not updated after offline path: %s", row.Node)
	}
}

func TestMigrateAdmin_StoppedVMUsesOfflineDirectly(t *testing.T) {
	t.Parallel()
	fake := migrateFakePVE(t)

	var shutdownCalls, startCalls, offlineMig atomic.Int32
	fake.migrateVM = func(_ context.Context, _ string, _ int, _ string, online bool) (string, error) {
		if online {
			t.Errorf("online migration should not be tried for stopped VM")
			return "", errors.New("unexpected online attempt")
		}
		offlineMig.Add(1)
		return "task:migrate", nil
	}
	fake.shutdownVM = func(_ context.Context, _ string, _ int) (string, error) {
		shutdownCalls.Add(1)
		return "task:shutdown", nil
	}
	fake.startVM = func(_ context.Context, _ string, _ int) (string, error) {
		startCalls.Add(1)
		return "task:start", nil
	}

	svc, _, database := newTestService(t, fake)
	id := seedVM(t, database, "stopped")

	res, err := svc.MigrateAdmin(context.Background(), id, "beta", false)
	if err != nil {
		t.Fatalf("MigrateAdmin (stopped): %v", err)
	}
	if res.Mode != "offline" || res.WasStopped {
		t.Errorf("stopped VM should report Mode=offline WasStopped=false, got %+v", res)
	}
	if shutdownCalls.Load() != 0 {
		t.Errorf("should not shut down an already-stopped VM, got %d calls", shutdownCalls.Load())
	}
	if startCalls.Load() != 0 {
		t.Errorf("should not start a VM that started stopped, got %d calls", startCalls.Load())
	}
	if offlineMig.Load() != 1 {
		t.Errorf("expected 1 offline migrate, got %d", offlineMig.Load())
	}
}

func TestMigrateAdmin_VMNotFound(t *testing.T) {
	t.Parallel()
	fake := migrateFakePVE(t)
	svc, _, _ := newTestService(t, fake)

	_, err := svc.MigrateAdmin(context.Background(), 9999, "beta", false)
	var notFound *internalerrors.NotFoundError
	if !errors.As(err, &notFound) {
		t.Fatalf("expected NotFoundError, got %T %v", err, err)
	}
	if notFound.Resource != "vm" {
		t.Errorf("expected Resource=vm, got %s", notFound.Resource)
	}
}

func TestMigrateAdmin_EmptyTargetRejected(t *testing.T) {
	t.Parallel()
	fake := migrateFakePVE(t)
	svc, _, database := newTestService(t, fake)
	id := seedVM(t, database, "running")

	_, err := svc.MigrateAdmin(context.Background(), id, "", false)
	var validation *internalerrors.ValidationError
	if !errors.As(err, &validation) {
		t.Fatalf("expected ValidationError, got %T %v", err, err)
	}
	if validation.Field != "target_node" {
		t.Errorf("expected field=target_node, got %s", validation.Field)
	}
}
