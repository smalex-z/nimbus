package s3storage

import (
	"context"
	"errors"
	"fmt"
	"log"
	"net/http"
	"net/url"
	"strings"
	"time"

	"nimbus/internal/db"
	"nimbus/internal/provision"
)

// Provisioner is the subset of *provision.Service the deploy orchestrator
// needs. Defined here per the "accept interfaces" idiom — the s3storage
// package doesn't take a hard dependency on provision's full surface.
//
// AdminDelete is the post-merge full-teardown call: stops the VM, destroys
// it on Proxmox, releases the IP, and hard-deletes the vms row. Takes the
// Nimbus-side VM row ID, not the Proxmox VMID — callers look up the row
// via s.db before calling this.
type Provisioner interface {
	Provision(ctx context.Context, req provision.Request, progress provision.ProgressReporter) (*provision.Result, error)
	AdminDelete(ctx context.Context, id uint) error
}

// DeployParams captures the fields the user controls on the /s3 page.
type DeployParams struct {
	// DiskGB is the requested disk size. The orchestrator maps this to
	// the closest provision tier that fits — anything ≤ 30 → medium,
	// ≤ 60 → large, otherwise xl (120 GB cap). Online grow past the
	// tier cap is a follow-up.
	DiskGB int
}

// Deploy runs the end-to-end "deploy MinIO" flow:
//
//  1. Generate root credentials and insert the singleton s3_storage row
//     in status=deploying. (Credentials are persisted before the VM is
//     created so a crash mid-bootstrap leaves a recoverable state.)
//  2. Call provision.Service.Provision with hardcoded VM params + an
//     auto-generated SSH key. Failure here unwinds the s3_storage row.
//  3. SSH into the VM with the freshly minted key, run the install
//     script, wait for MinIO's HTTP endpoint to become reachable.
//  4. Mark the row ready with the resolved endpoint.
//
// On any post-Create failure the row is flipped to status=error with a
// short message; the VM is left in place so an operator can inspect.
// The /api/s3/storage DELETE handler is the recovery path — it tears
// down the VM and clears the row.
//
// progress is forwarded to the underlying Provision call; the ready
// step is reported separately so the UI's checklist can reflect MinIO
// install progress.
func (s *Service) Deploy(ctx context.Context, prov Provisioner, p DeployParams, progress provision.ProgressReporter) (*db.S3Storage, *provision.Result, error) {
	if prov == nil {
		return nil, nil, errors.New("nil provisioner")
	}
	tier := chooseTier(p.DiskGB)

	rootUser, rootPass, err := GenerateRootCredentials()
	if err != nil {
		return nil, nil, fmt.Errorf("generate creds: %w", err)
	}

	// Reserve the singleton row before the slow provision call so a
	// concurrent second deploy fails fast with ErrAlreadyDeployed
	// instead of racing on Proxmox API resources.
	if _, err := s.Get(); err == nil {
		return nil, nil, ErrAlreadyDeployed
	} else if !errors.Is(err, ErrNotDeployed) {
		return nil, nil, err
	}

	// We intentionally insert the row with placeholder VMID/Node = 0/""
	// here; we'll Update them once Provision returns. This row guards
	// against concurrent deploys.
	row, err := s.Create(CreateParams{
		VMID:         0,
		Node:         "pending",
		DiskGB:       p.DiskGB,
		RootUser:     rootUser,
		RootPassword: rootPass,
	})
	if err != nil {
		return nil, nil, err
	}

	res, err := prov.Provision(ctx, provision.Request{
		Hostname:    nimbusS3Hostname(),
		Tier:        tier,
		OSTemplate:  "ubuntu-24.04",
		GenerateKey: true,
		// Storage VM should not be exposed via Gopher; it lives on the
		// internal cluster network only.
		PublicTunnel: false,
	}, progress)
	if err != nil {
		// Provisioning failed before any VM was created (or it failed
		// after — Provision is responsible for releasing the IP and
		// surfacing a useful error). We just need to clear the storage
		// row so the user can retry.
		_ = s.Delete()
		return nil, nil, fmt.Errorf("provision: %w", err)
	}

	// Persist the real VMID/Node/IP now that the VM exists. The row has
	// to hold these so DELETE /api/s3/storage can find the VM to tear
	// down (and release its IP).
	updates := map[string]any{
		"vm_row_id": res.ID,
		"vmid":      res.VMID,
		"node":      res.Node,
		"ip":        res.IP,
	}
	// Look up the VM's auto-generated SSH key id so the storage row can
	// pin it. Marking the key system-generated lets the Keys UI hide it
	// by default and lets Service.Delete garbage-collect it on teardown.
	var sshKeyID *uint
	var vm db.VM
	if vmErr := s.db.First(&vm, res.ID).Error; vmErr == nil && vm.SSHKeyID != nil {
		sshKeyID = vm.SSHKeyID
		updates["ssh_key_id"] = *sshKeyID
		if mkErr := s.db.Model(&db.SSHKey{}).Where("id = ?", *sshKeyID).
			Update("system_generated", true).Error; mkErr != nil {
			log.Printf("s3 deploy: mark key %d system-generated: %v", *sshKeyID, mkErr)
		}
	} else if vmErr != nil {
		log.Printf("s3 deploy: lookup vm %d for ssh-key cleanup pin: %v", res.ID, vmErr)
	}
	if err := s.db.Model(&db.S3Storage{}).Where("id = ?", row.ID).Updates(updates).Error; err != nil {
		// Best-effort tear-down: we created a VM we can't book-keep.
		// AdminDelete needs the Nimbus-side row ID; res.ID carries it.
		_ = prov.AdminDelete(context.Background(), res.ID)
		_ = s.Delete()
		return nil, nil, fmt.Errorf("persist vm metadata: %w", err)
	}

	if res.Warning != "" {
		// Soft-success on Provision means the VM was created but Nimbus
		// couldn't confirm reachability. SSH bootstrap will likely fail
		// for the same reason; mark the row error and let the user retry
		// via Delete + redeploy.
		_ = s.MarkError("vm not reachable: " + res.Warning)
		return row, res, fmt.Errorf("vm unreachable for bootstrap: %s", res.Warning)
	}

	if err := runBootstrap(ctx, res.IP, res.Username, res.SSHPrivateKey, rootUser, rootPass); err != nil {
		_ = s.MarkError("bootstrap failed: " + err.Error())
		return row, res, fmt.Errorf("bootstrap: %w", err)
	}

	endpoint := fmt.Sprintf("http://%s:9000", res.IP)
	if err := waitMinIOLive(ctx, endpoint); err != nil {
		_ = s.MarkError("minio not responding: " + err.Error())
		return row, res, fmt.Errorf("minio liveness: %w", err)
	}

	if err := s.MarkReady(endpoint); err != nil {
		return row, res, fmt.Errorf("mark ready: %w", err)
	}

	updated, err := s.Get()
	if err != nil {
		return row, res, err
	}
	log.Printf("s3 storage deployed: vmid=%d node=%s endpoint=%s", res.VMID, res.Node, endpoint)
	return updated, res, nil
}

// chooseTier maps a user-requested disk size to the smallest provision
// tier whose disk allotment fits. Online grow beyond the tier cap is a
// follow-up — for now, requests > 120 GB are clamped down (and the
// handler-side validator rejects them before we get here).
func chooseTier(diskGB int) string {
	switch {
	case diskGB <= 0, diskGB <= 30:
		return "medium"
	case diskGB <= 60:
		return "large"
	default:
		return "xl"
	}
}

// nimbusS3Hostname returns this deploy's VM hostname. Each invocation
// gets a fresh random suffix so the underlying vms / ssh_keys rows
// (whose unique-index columns are derived from the hostname) never
// collide with leftover state from a previous failed deploy.
//
// The singleton constraint lives at the s3_storage row level, not at
// the hostname level — orphan vms/ssh_keys rows from past failures are
// a (small) leak but not blocking; cleanup lives in a follow-up that
// also removes user-VM rows on Delete.
func nimbusS3Hostname() string {
	suffix, err := randomHex(3) // 6 hex chars
	if err != nil {
		// crypto/rand failure is exceedingly unlikely; fall back to a
		// time-derived suffix so we still produce a unique name.
		return fmt.Sprintf("nimbus-s3-%d", time.Now().Unix())
	}
	return "nimbus-s3-" + suffix
}

// waitMinIOLive polls the MinIO health endpoint with a short total
// budget. MinIO's `/minio/health/live` is unauthenticated and returns
// 200 once the server has finished starting (roughly 2-3s after the
// container starts).
func waitMinIOLive(ctx context.Context, endpoint string) error {
	healthURL := strings.TrimSuffix(endpoint, "/") + "/minio/health/live"
	if _, err := url.Parse(healthURL); err != nil {
		return fmt.Errorf("invalid endpoint: %w", err)
	}

	client := &http.Client{Timeout: 3 * time.Second}
	deadline := time.Now().Add(60 * time.Second)
	var lastErr error
	for time.Now().Before(deadline) {
		select {
		case <-ctx.Done():
			return ctx.Err()
		default:
		}
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, healthURL, nil)
		if err != nil {
			return err
		}
		resp, err := client.Do(req)
		if err == nil {
			_ = resp.Body.Close()
			if resp.StatusCode == http.StatusOK {
				return nil
			}
			lastErr = fmt.Errorf("status %d", resp.StatusCode)
		} else {
			lastErr = err
		}
		time.Sleep(2 * time.Second)
	}
	if lastErr == nil {
		lastErr = errors.New("timeout")
	}
	return lastErr
}
