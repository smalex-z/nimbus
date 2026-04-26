package gpu_test

import (
	"context"
	"errors"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"nimbus/internal/db"
	internalerrors "nimbus/internal/errors"
	"nimbus/internal/gpu"
)

// newTestService spins up a per-test SQLite DB + log dir. Mirrors the
// pattern used by the ippool tests so behavior matches production
// single-writer SQLite.
func newTestService(t *testing.T) (*gpu.Service, *db.DB) {
	t.Helper()
	dbPath := filepath.Join(t.TempDir(), "test.db")
	database, err := db.New(dbPath, &db.GPUJob{}, &db.GPUSettings{})
	if err != nil {
		t.Fatalf("db.New: %v", err)
	}
	logDir := filepath.Join(t.TempDir(), "logs")
	svc, err := gpu.New(database.DB, logDir)
	if err != nil {
		t.Fatalf("gpu.New: %v", err)
	}
	return svc, database
}

func TestEnqueueJob_RejectsEmptyImage(t *testing.T) {
	t.Parallel()
	svc, _ := newTestService(t)
	_, err := svc.EnqueueJob(context.Background(), gpu.EnqueueRequest{
		OwnerID: 1,
	})
	if err == nil {
		t.Fatal("expected error for empty image, got nil")
	}
	var ve *internalerrors.ValidationError
	if !errorsAs(err, &ve) {
		t.Errorf("err = %T, want *ValidationError", err)
	}
}

func TestEnqueueJob_PersistsAndOrdersByQueuedAt(t *testing.T) {
	t.Parallel()
	svc, _ := newTestService(t)
	ctx := context.Background()
	_, err := svc.EnqueueJob(ctx, gpu.EnqueueRequest{OwnerID: 1, Image: "alpine"})
	if err != nil {
		t.Fatalf("first enqueue: %v", err)
	}
	// Tiny sleep guarantees distinct queued_at values without flakiness — the
	// claim test uses ordering, not exact timestamps.
	time.Sleep(2 * time.Millisecond)
	_, err = svc.EnqueueJob(ctx, gpu.EnqueueRequest{OwnerID: 1, Image: "ubuntu"})
	if err != nil {
		t.Fatalf("second enqueue: %v", err)
	}

	jobs, err := svc.ListJobs(ctx, gpu.ListFilter{OwnerID: 1})
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(jobs) != 2 {
		t.Fatalf("got %d jobs, want 2", len(jobs))
	}
	for _, j := range jobs {
		if j.Status != gpu.StatusQueued {
			t.Errorf("status = %s, want queued", j.Status)
		}
	}
}

// ClaimNextJob's transactional claim is the centerpiece of the queue. Two
// goroutines hammering it on the same single queued row must produce
// exactly one winner.
func TestClaimNextJob_RaceProducesOneWinner(t *testing.T) {
	t.Parallel()
	svc, _ := newTestService(t)
	ctx := context.Background()
	_, err := svc.EnqueueJob(ctx, gpu.EnqueueRequest{OwnerID: 1, Image: "alpine"})
	if err != nil {
		t.Fatalf("enqueue: %v", err)
	}

	const attempts = 8
	var wg sync.WaitGroup
	results := make(chan bool, attempts)
	for i := 0; i < attempts; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			_, ok, err := svc.ClaimNextJob(ctx, "worker")
			if err != nil {
				t.Errorf("claim %d: %v", i, err)
				return
			}
			results <- ok
		}(i)
	}
	wg.Wait()
	close(results)

	winners := 0
	for ok := range results {
		if ok {
			winners++
		}
	}
	if winners != 1 {
		t.Errorf("got %d winners, want exactly 1", winners)
	}
}

func TestClaimNextJob_EmptyQueueReturnsFalse(t *testing.T) {
	t.Parallel()
	svc, _ := newTestService(t)
	job, ok, err := svc.ClaimNextJob(context.Background(), "worker")
	if err != nil {
		t.Fatalf("claim: %v", err)
	}
	if ok || job != nil {
		t.Errorf("got (%+v, %v), want (nil, false) on empty queue", job, ok)
	}
}

func TestCancelJob_QueuedTransitionsToCancelled(t *testing.T) {
	t.Parallel()
	svc, _ := newTestService(t)
	ctx := context.Background()
	enq, err := svc.EnqueueJob(ctx, gpu.EnqueueRequest{OwnerID: 1, Image: "alpine"})
	if err != nil {
		t.Fatalf("enqueue: %v", err)
	}
	got, err := svc.CancelJob(ctx, enq.ID, 1, false)
	if err != nil {
		t.Fatalf("cancel: %v", err)
	}
	if got.Status != gpu.StatusCancelled {
		t.Errorf("status = %s, want cancelled", got.Status)
	}
	if got.FinishedAt == nil {
		t.Error("FinishedAt = nil, want a timestamp")
	}
}

func TestCancelJob_RejectsNonOwnerByPretendingNotFound(t *testing.T) {
	t.Parallel()
	svc, _ := newTestService(t)
	ctx := context.Background()
	enq, err := svc.EnqueueJob(ctx, gpu.EnqueueRequest{OwnerID: 1, Image: "alpine"})
	if err != nil {
		t.Fatalf("enqueue: %v", err)
	}
	_, err = svc.CancelJob(ctx, enq.ID, 2 /* different user */, false)
	var nf *internalerrors.NotFoundError
	if !errorsAs(err, &nf) {
		t.Errorf("err = %v, want *NotFoundError", err)
	}
}

func TestReportStatus_RunningToSucceeded(t *testing.T) {
	t.Parallel()
	svc, _ := newTestService(t)
	ctx := context.Background()
	enq, err := svc.EnqueueJob(ctx, gpu.EnqueueRequest{OwnerID: 1, Image: "alpine"})
	if err != nil {
		t.Fatalf("enqueue: %v", err)
	}
	if _, _, err := svc.ClaimNextJob(ctx, "worker"); err != nil {
		t.Fatalf("claim: %v", err)
	}
	if err := svc.ReportStatus(ctx, enq.ID, gpu.ReportStatusRequest{
		Status:   gpu.StatusSucceeded,
		ExitCode: 0,
	}); err != nil {
		t.Fatalf("report: %v", err)
	}
	got, err := svc.GetJob(ctx, enq.ID, 1, false)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.Status != gpu.StatusSucceeded {
		t.Errorf("status = %s, want succeeded", got.Status)
	}
	if got.ExitCode == nil || *got.ExitCode != 0 {
		t.Errorf("exit_code = %v, want 0", got.ExitCode)
	}
}

func TestAppendLogs_TailTruncatesAtMax(t *testing.T) {
	t.Parallel()
	svc, _ := newTestService(t)
	ctx := context.Background()
	enq, err := svc.EnqueueJob(ctx, gpu.EnqueueRequest{OwnerID: 1, Image: "alpine"})
	if err != nil {
		t.Fatalf("enqueue: %v", err)
	}
	// Write more than LogTailMax bytes so we can verify truncation.
	chunk := make([]byte, gpu.LogTailMax/2)
	for i := range chunk {
		chunk[i] = 'a'
	}
	for i := 0; i < 4; i++ { // 2x LogTailMax total
		if err := svc.AppendLogs(ctx, enq.ID, chunk); err != nil {
			t.Fatalf("append %d: %v", i, err)
		}
	}
	got, err := svc.GetJob(ctx, enq.ID, 1, false)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if len(got.LogTail) != gpu.LogTailMax {
		t.Errorf("LogTail length = %d, want %d", len(got.LogTail), gpu.LogTailMax)
	}
}

func TestReapStuckJobs_FlipsLongRunningJobsToFailed(t *testing.T) {
	t.Parallel()
	svc, _ := newTestService(t)
	ctx := context.Background()
	enq, err := svc.EnqueueJob(ctx, gpu.EnqueueRequest{OwnerID: 1, Image: "alpine"})
	if err != nil {
		t.Fatalf("enqueue: %v", err)
	}
	if _, _, err := svc.ClaimNextJob(ctx, "worker"); err != nil {
		t.Fatalf("claim: %v", err)
	}

	// Reap with a near-zero timeout — the job we just claimed had
	// started_at set to "now", so a 1ns timeout makes it stuck immediately.
	time.Sleep(2 * time.Millisecond)
	n, err := svc.ReapStuckJobs(ctx, time.Nanosecond)
	if err != nil {
		t.Fatalf("reap: %v", err)
	}
	if n != 1 {
		t.Errorf("reaped = %d, want 1", n)
	}
	got, err := svc.GetJob(ctx, enq.ID, 1, false)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.Status != gpu.StatusFailed {
		t.Errorf("status = %s, want failed", got.Status)
	}
	if got.ErrorMsg == "" {
		t.Error("expected non-empty error_msg on reaped job")
	}
}

// errorsAs wraps errors.As with the loose target type our test cases use.
func errorsAs(err error, target any) bool {
	return errors.As(err, target)
}
