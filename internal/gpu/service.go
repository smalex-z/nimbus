// Package gpu manages the GX10 (or any single-host GPU plane) job queue.
//
// The queue is a `gpu_jobs` SQLite table — no Redis or external broker. A
// single worker on the GX10 polls Nimbus on a fixed cadence; ClaimNextJob
// uses a transactional UPDATE with `WHERE status='queued' ORDER BY queued_at
// LIMIT 1` so two simultaneous polls can't both grab the same row.
//
// Logs are split: the last LogTailMax bytes mirror into the DB row for the
// API response, while the full log is appended to a per-job file at
// /var/lib/nimbus/gpu-jobs/<id>.log. PruneOldLogs handles disk cleanup.
//
// The service is intentionally agnostic about how the worker runs jobs
// (Docker, podman, raw binary). It records what was submitted and where the
// logs end up; the worker decides the rest.
package gpu

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"gorm.io/gorm"

	"nimbus/internal/db"
	internalerrors "nimbus/internal/errors"
)

// LogTailMax caps how many trailing bytes of a job's combined log we mirror
// into the DB row. Bigger gives more context to the API consumer at the
// cost of larger response bodies and SQLite row size; 64 KB is the sweet
// spot for a typical training run that prints ~lines/s.
const LogTailMax = 64 * 1024

// DefaultStuckJobTimeout is how long a job can sit in `running` before the
// startup sweep gives up and marks it failed. One hour matches the prompt's
// default; ops can override at construction time.
const DefaultStuckJobTimeout = 1 * time.Hour

// DefaultLogRetention is how long full on-disk log files survive after a
// job finishes. 30 days lets users dig back into a recent run; logs older
// than that get pruned by PruneOldLogs at startup.
const DefaultLogRetention = 30 * 24 * time.Hour

// Status constants. Stored verbatim in the gpu_jobs.status column.
const (
	StatusQueued    = "queued"
	StatusRunning   = "running"
	StatusSucceeded = "succeeded"
	StatusFailed    = "failed"
	StatusCancelled = "cancelled"
)

// IsTerminal reports whether the status is a terminal state (no more
// transitions possible). Used by guards so cancelled / finished jobs can't
// be re-claimed or re-cancelled.
func IsTerminal(status string) bool {
	return status == StatusSucceeded || status == StatusFailed || status == StatusCancelled
}

// Service is the GPU plane's coordination layer.
type Service struct {
	db     *gorm.DB
	logDir string
	clock  func() time.Time
}

// Option tunes a Service at construction.
type Option func(*Service)

// WithClock injects a clock for tests.
func WithClock(f func() time.Time) Option {
	return func(s *Service) {
		if f != nil {
			s.clock = f
		}
	}
}

// New constructs a Service. logDir is created if it doesn't exist; failure
// to create is fatal because the worker needs somewhere to stream logs.
func New(database *gorm.DB, logDir string, opts ...Option) (*Service, error) {
	if err := os.MkdirAll(logDir, 0o750); err != nil {
		return nil, fmt.Errorf("ensure log dir %s: %w", logDir, err)
	}
	s := &Service{db: database, logDir: logDir, clock: time.Now}
	for _, opt := range opts {
		opt(s)
	}
	return s, nil
}

// LogPath returns the absolute path to a job's full on-disk log file.
// Exposed so tests can read it without poking at internals.
func (s *Service) LogPath(jobID uint) string {
	return filepath.Join(s.logDir, fmt.Sprintf("%d.log", jobID))
}

// EnqueueRequest is the input to EnqueueJob. Env is encoded to JSON for
// storage (the worker decodes back to map[string]string at run time).
type EnqueueRequest struct {
	OwnerID uint
	VMID    *uint
	Image   string
	Command string
	Env     map[string]string
}

// EnqueueJob inserts a new row in the queued state. Image is required;
// command may be empty (uses the container's default ENTRYPOINT/CMD).
func (s *Service) EnqueueJob(ctx context.Context, req EnqueueRequest) (*db.GPUJob, error) {
	image := strings.TrimSpace(req.Image)
	if image == "" {
		return nil, &internalerrors.ValidationError{Field: "image", Message: "image is required"}
	}
	envJSON := ""
	if len(req.Env) > 0 {
		raw, err := json.Marshal(req.Env)
		if err != nil {
			return nil, fmt.Errorf("encode env: %w", err)
		}
		envJSON = string(raw)
	}
	job := &db.GPUJob{
		OwnerID:  req.OwnerID,
		VMID:     req.VMID,
		Status:   StatusQueued,
		Image:    image,
		Command:  req.Command,
		EnvJSON:  envJSON,
		QueuedAt: s.clock().UTC(),
	}
	if err := s.db.WithContext(ctx).Create(job).Error; err != nil {
		return nil, fmt.Errorf("insert gpu job: %w", err)
	}
	return job, nil
}

// ListFilter narrows the result of ListJobs. OwnerID is mandatory unless
// IncludeAllOwners is true (admin path). Status filters to a single state
// when non-empty.
type ListFilter struct {
	OwnerID          uint
	IncludeAllOwners bool
	Status           string
	Limit            int // 0 = no limit
}

// ListJobs returns jobs matching the filter, newest first.
func (s *Service) ListJobs(ctx context.Context, f ListFilter) ([]db.GPUJob, error) {
	q := s.db.WithContext(ctx).Order("queued_at DESC")
	if !f.IncludeAllOwners {
		q = q.Where("owner_id = ?", f.OwnerID)
	}
	if f.Status != "" {
		q = q.Where("status = ?", f.Status)
	}
	if f.Limit > 0 {
		q = q.Limit(f.Limit)
	}
	var jobs []db.GPUJob
	if err := q.Find(&jobs).Error; err != nil {
		return nil, fmt.Errorf("list gpu jobs: %w", err)
	}
	return jobs, nil
}

// GetJob returns a single job. requesterID + isAdmin enforce the
// owner-or-admin gate; non-owners get NotFound (we don't reveal existence).
func (s *Service) GetJob(ctx context.Context, id uint, requesterID uint, isAdmin bool) (*db.GPUJob, error) {
	var job db.GPUJob
	if err := s.db.WithContext(ctx).First(&job, id).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return nil, &internalerrors.NotFoundError{Resource: "gpu_job", ID: fmt.Sprintf("%d", id)}
		}
		return nil, fmt.Errorf("get gpu job: %w", err)
	}
	if !isAdmin && job.OwnerID != requesterID {
		return nil, &internalerrors.NotFoundError{Resource: "gpu_job", ID: fmt.Sprintf("%d", id)}
	}
	return &job, nil
}

// CancelJob flips a queued job to cancelled directly, or marks a running
// job as cancelled (the worker will observe this on its next status post
// and SIGTERM the container). Terminal jobs return ConflictError.
func (s *Service) CancelJob(ctx context.Context, id uint, requesterID uint, isAdmin bool) (*db.GPUJob, error) {
	job, err := s.GetJob(ctx, id, requesterID, isAdmin)
	if err != nil {
		return nil, err
	}
	if IsTerminal(job.Status) {
		return nil, &internalerrors.ConflictError{Message: "job is already in a terminal state"}
	}
	now := s.clock().UTC()
	updates := map[string]any{
		"status":      StatusCancelled,
		"finished_at": &now,
	}
	if err := s.db.WithContext(ctx).Model(&db.GPUJob{}).Where("id = ?", id).Updates(updates).Error; err != nil {
		return nil, fmt.Errorf("cancel job: %w", err)
	}
	job.Status = StatusCancelled
	job.FinishedAt = &now
	return job, nil
}

// ClaimNextJob is the worker's poll endpoint. Returns (job, true) when a
// queued job was claimed and flipped to running, or (nil, false, nil) when
// the queue is empty. workerID is recorded on the job for observability.
//
// Implemented as a transactional select-then-update so two concurrent
// claims can't both succeed: SQLite's single-writer lock serializes the
// transactions, and the inner UPDATE checks status='queued' so the loser
// updates 0 rows and falls through to "no job".
func (s *Service) ClaimNextJob(ctx context.Context, workerID string) (*db.GPUJob, bool, error) {
	var claimed *db.GPUJob
	err := s.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		var job db.GPUJob
		if err := tx.
			Where("status = ?", StatusQueued).
			Order("queued_at ASC").
			Limit(1).
			First(&job).Error; err != nil {
			if errors.Is(err, gorm.ErrRecordNotFound) {
				return nil
			}
			return fmt.Errorf("select queued job: %w", err)
		}
		now := s.clock().UTC()
		// Status guard in WHERE — if a parallel transaction beat us to it,
		// RowsAffected is 0 and we fall through to "no job".
		res := tx.Model(&db.GPUJob{}).
			Where("id = ? AND status = ?", job.ID, StatusQueued).
			Updates(map[string]any{
				"status":     StatusRunning,
				"started_at": &now,
				"worker_id":  workerID,
			})
		if res.Error != nil {
			return fmt.Errorf("claim job %d: %w", job.ID, res.Error)
		}
		if res.RowsAffected == 0 {
			return nil // raced with another worker; bail this transaction
		}
		job.Status = StatusRunning
		job.StartedAt = &now
		job.WorkerID = workerID
		claimed = &job
		return nil
	})
	if err != nil {
		return nil, false, err
	}
	return claimed, claimed != nil, nil
}

// AppendLogs streams a chunk of bytes into both the on-disk log file and
// the inline LogTail. Tail is truncated to the last LogTailMax bytes after
// each append so the DB row stays bounded.
//
// Best-effort on the disk write: if the file open fails, we still update
// the tail and return the error so the caller can decide. The worker logs
// the error and keeps going — losing some log chunks is preferable to
// stalling job execution.
func (s *Service) AppendLogs(ctx context.Context, jobID uint, chunk []byte) error {
	if len(chunk) == 0 {
		return nil
	}
	// Disk write first — we want the truth on disk even if the DB hiccups.
	f, err := os.OpenFile(s.LogPath(jobID), os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o640)
	if err != nil {
		return fmt.Errorf("open log file: %w", err)
	}
	if _, werr := f.Write(chunk); werr != nil {
		_ = f.Close()
		return fmt.Errorf("write log file: %w", werr)
	}
	if cerr := f.Close(); cerr != nil {
		return fmt.Errorf("close log file: %w", cerr)
	}
	// Read current tail, append, truncate. Done in one transaction to
	// avoid two concurrent appends interleaving.
	return s.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		var job db.GPUJob
		if err := tx.Select("id", "log_tail").First(&job, jobID).Error; err != nil {
			return fmt.Errorf("load job for tail update: %w", err)
		}
		combined := job.LogTail + string(chunk)
		if len(combined) > LogTailMax {
			combined = combined[len(combined)-LogTailMax:]
		}
		return tx.Model(&db.GPUJob{}).Where("id = ?", jobID).
			Update("log_tail", combined).Error
	})
}

// ReportStatusRequest is the worker's terminal-status post.
type ReportStatusRequest struct {
	Status       string // succeeded | failed
	ExitCode     int
	ArtifactPath string
	ErrorMsg     string
}

// ReportStatus marks a job terminal. Only callable on running jobs;
// re-reporting on a job that's already terminal returns ConflictError.
// Cancelled jobs that finish are accepted as final state without changing
// to succeeded/failed (cancellation wins).
func (s *Service) ReportStatus(ctx context.Context, jobID uint, req ReportStatusRequest) error {
	if req.Status != StatusSucceeded && req.Status != StatusFailed {
		return &internalerrors.ValidationError{
			Field:   "status",
			Message: "status must be succeeded or failed",
		}
	}
	now := s.clock().UTC()
	res := s.db.WithContext(ctx).Model(&db.GPUJob{}).
		Where("id = ? AND status = ?", jobID, StatusRunning).
		Updates(map[string]any{
			"status":        req.Status,
			"exit_code":     &req.ExitCode,
			"artifact_path": req.ArtifactPath,
			"error_msg":     req.ErrorMsg,
			"finished_at":   &now,
		})
	if res.Error != nil {
		return fmt.Errorf("report status: %w", res.Error)
	}
	if res.RowsAffected == 0 {
		// Either the job doesn't exist, or it's already cancelled / terminal.
		// Treat cancelled-then-completed as a no-op (worker raced our cancel).
		var job db.GPUJob
		if err := s.db.WithContext(ctx).First(&job, jobID).Error; err != nil {
			return fmt.Errorf("locate job for status report: %w", err)
		}
		if IsTerminal(job.Status) {
			return nil
		}
		return &internalerrors.ConflictError{
			Message: fmt.Sprintf("job is in unexpected state %q", job.Status),
		}
	}
	return nil
}

// ReapStuckJobs marks any job that's been in `running` longer than
// `timeout` as failed with a synthetic error message. Run from main.go on
// every startup so a GX10 reboot mid-job doesn't leave a job permanently
// "running" with no worker watching.
//
// Returns the number of reaped rows for logging.
func (s *Service) ReapStuckJobs(ctx context.Context, timeout time.Duration) (int, error) {
	cutoff := s.clock().UTC().Add(-timeout)
	now := s.clock().UTC()
	res := s.db.WithContext(ctx).Model(&db.GPUJob{}).
		Where("status = ? AND started_at IS NOT NULL AND started_at < ?", StatusRunning, cutoff).
		Updates(map[string]any{
			"status":      StatusFailed,
			"finished_at": &now,
			"error_msg":   fmt.Sprintf("reaped: stuck in running state for over %s", timeout),
		})
	if res.Error != nil {
		return 0, fmt.Errorf("reap stuck jobs: %w", res.Error)
	}
	return int(res.RowsAffected), nil
}

// PruneOldLogs deletes on-disk log files for jobs that finished longer
// ago than maxAge. The DB row keeps its inline log tail forever so the
// API can still answer historical queries, just without full-history
// drilldown.
//
// Returns (count, freedBytes, error).
func (s *Service) PruneOldLogs(ctx context.Context, maxAge time.Duration) (int, int64, error) {
	entries, err := os.ReadDir(s.logDir)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return 0, 0, nil
		}
		return 0, 0, fmt.Errorf("read log dir: %w", err)
	}
	cutoff := s.clock().Add(-maxAge)
	var pruned int
	var freed int64
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		info, err := e.Info()
		if err != nil {
			continue
		}
		if info.ModTime().After(cutoff) {
			continue
		}
		path := filepath.Join(s.logDir, e.Name())
		size := info.Size()
		if rmErr := os.Remove(path); rmErr != nil {
			continue
		}
		pruned++
		freed += size
	}
	_ = ctx // currently unused; kept on signature so future quota checks can take a deadline
	return pruned, freed, nil
}

// ReadFullLog returns the entire on-disk log for a job. Caller is
// responsible for closing the returned reader. Returns NotFound when
// neither the row nor the file exists.
func (s *Service) ReadFullLog(jobID uint) (io.ReadCloser, error) {
	f, err := os.Open(s.LogPath(jobID))
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, &internalerrors.NotFoundError{Resource: "gpu_job_log", ID: fmt.Sprintf("%d", jobID)}
		}
		return nil, fmt.Errorf("open log: %w", err)
	}
	return f, nil
}
