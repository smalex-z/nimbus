// Package bootstrap creates Proxmox VM templates from cloud images.
//
// Goal: replace the manual "SSH to a node, run qm commands" workflow with a
// single API call (or wizard step) that downloads the cloud images and
// converts them into ready-to-clone templates on the Proxmox cluster.
//
// All work happens via the Proxmox REST API — no `qm` CLI dependency, no
// SSH — so this works whether Nimbus is installed on a Proxmox host, in a
// guest VM, or on an external machine.
package bootstrap

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"golang.org/x/sync/errgroup"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"

	"nimbus/internal/db"
	"nimbus/internal/proxmox"
)

// Default Proxmox storage names. Overridable via Config.
const (
	DefaultDiskStorage  = "local-lvm" // where the imported VM disk lives
	DefaultImageStorage = "local"     // where downloaded cloud images land
	DefaultTemplateBase = 9000

	// Per-template timeouts. Cloud images are 200MB-1GB; downloads take 30-180s
	// depending on the node's bandwidth. Disk import is fast (local).
	downloadPollInterval = 5 * time.Second
	createPollInterval   = 2 * time.Second

	// Bound parallelism so a 50-node cluster doesn't simultaneously hammer
	// cloud-images.ubuntu.com from every node.
	maxParallelNodes = 5
)

// ProxmoxClient is the small subset of *proxmox.Client we depend on. Defined
// here (in the consumer) so tests can substitute a mock — same accept-interfaces
// pattern used in internal/provision.
type ProxmoxClient interface {
	GetNodes(ctx context.Context) ([]proxmox.Node, error)
	TemplateExists(ctx context.Context, node string, vmid int) (bool, error)
	NextVMID(ctx context.Context) (int, error)
	EnsureStorageContent(ctx context.Context, storage, contentType string) error
	StorageHasFile(ctx context.Context, node, storage, contentType, filename string) (bool, error)
	DownloadStorageURL(ctx context.Context, node, storage, contentType, url, filename string) (string, error)
	WaitForTask(ctx context.Context, node, taskID string, interval time.Duration) error
	CreateVMWithImport(ctx context.Context, node string, vmid int, opts proxmox.CreateVMOpts) (string, error)
	SetCloudInitDrive(ctx context.Context, node string, vmid int, storage string) error
	ConvertToTemplate(ctx context.Context, node string, vmid int) error
}

// importContentType is the Proxmox content type Nimbus uses for cloud-image
// downloads. It's also what `import-from` requires when creating a VM.
// Lives in /var/lib/vz/import/ on a `dir`-type storage.
const importContentType = "import"

// Config holds the deployment-specific knobs. All values have sensible
// defaults so a bare Config{} works on a stock Proxmox install.
//
// TemplateBaseVMID is now only used as a hint / floor for `NextVMID` callers
// — the actual VMIDs assigned to templates are whatever Proxmox returns from
// `cluster/nextid` at bootstrap time. This avoids the cluster-wide-unique
// VMID conflict that the old "base + offset" scheme hit.
type Config struct {
	TemplateBaseVMID int
	DiskStorage      string
	ImageStorage     string
}

// Service is the bootstrap orchestrator. Safe for concurrent use across
// multiple Bootstrap calls (each fan-out is independent).
//
// The DB is needed to track (node, OS) → VMID mappings — the source of truth
// for which template lives where.
type Service struct {
	px  ProxmoxClient
	db  *gorm.DB
	cfg Config

	// nextVMIDMu serializes the NextVMID → CreateVMWithImport pair across
	// all bootstrap goroutines. Without it, two parallel goroutines on
	// different nodes can both query NextVMID, both get the same value,
	// and the second create fails with "VM X already exists on node Y".
	// Same pattern provision uses with cloneMu.
	nextVMIDMu sync.Mutex
}

// New constructs a Service, applying defaults for any unset Config fields.
func New(px ProxmoxClient, database *gorm.DB, cfg Config) *Service {
	if cfg.TemplateBaseVMID == 0 {
		cfg.TemplateBaseVMID = DefaultTemplateBase
	}
	if cfg.DiskStorage == "" {
		cfg.DiskStorage = DefaultDiskStorage
	}
	if cfg.ImageStorage == "" {
		cfg.ImageStorage = DefaultImageStorage
	}
	return &Service{px: px, db: database, cfg: cfg}
}

// Request is one bootstrap invocation. All fields are optional — empty Request
// means "all OSes in the catalogue, on every online node, idempotent".
type Request struct {
	Nodes []string // target nodes; empty = all online nodes
	OS    []string // OS keys; empty = all 4 catalog entries
	Force bool     // re-create even if the template already exists
}

// TemplateOutcome reports the result of one OS-on-one-node bootstrap step.
type TemplateOutcome struct {
	OS       string        `json:"os"`
	VMID     int           `json:"vmid"`
	Node     string        `json:"node"`
	Duration time.Duration `json:"duration"`
	Error    string        `json:"error,omitempty"`
}

// Result aggregates per-template outcomes from a Bootstrap call.
type Result struct {
	Created []TemplateOutcome `json:"created"`
	Skipped []TemplateOutcome `json:"skipped"`
	Failed  []TemplateOutcome `json:"failed"`
}

// Bootstrap runs the full template creation flow. It is the only public
// entrypoint of the Service.
//
// The flow fans out across nodes in parallel (up to maxParallelNodes at a
// time). Within each node, OSes are processed sequentially — Proxmox can
// handle concurrent `qm create` calls on the same node, but a single node's
// disk + network is the real bottleneck for large image downloads.
//
// Bootstrap does not abort on the first failure. It runs every requested
// (node, OS) pair to completion (or failure) and returns a comprehensive
// Result. The caller decides what counts as a "success" overall.
func (s *Service) Bootstrap(ctx context.Context, req Request) (*Result, error) {
	nodes, err := s.resolveNodes(ctx, req.Nodes)
	if err != nil {
		return nil, fmt.Errorf("resolve nodes: %w", err)
	}
	if len(nodes) == 0 {
		return nil, errors.New("no online nodes available")
	}

	osList, err := s.resolveOSes(req.OS)
	if err != nil {
		return nil, err
	}

	// One-time setup: ensure the image storage is configured to accept the
	// "import" content type. This is what cloud-image downloads use AND what
	// the import-from VM-creation parameter requires. Most stock Proxmox
	// installs only have iso/backup/vztmpl on `local` — we add `import` if
	// missing. Idempotent.
	if err := s.px.EnsureStorageContent(ctx, s.cfg.ImageStorage, importContentType); err != nil {
		return nil, fmt.Errorf("ensure %s accepts %q content: %w",
			s.cfg.ImageStorage, importContentType, err)
	}

	var (
		mu     sync.Mutex
		result = &Result{}
	)
	g, gctx := errgroup.WithContext(ctx)
	g.SetLimit(maxParallelNodes)

	for _, node := range nodes {
		node := node // capture for goroutine
		g.Go(func() error {
			for _, entry := range osList {
				outcome := s.bootstrapOne(gctx, node, entry, req.Force)
				mu.Lock()
				switch {
				case outcome.Error != "" && outcome.Error != errSkipped:
					result.Failed = append(result.Failed, outcome)
				case outcome.Error == errSkipped:
					outcome.Error = "" // don't surface the sentinel to callers
					result.Skipped = append(result.Skipped, outcome)
				default:
					result.Created = append(result.Created, outcome)
				}
				mu.Unlock()
			}
			return nil // never propagate failures via errgroup — we collect them in Result
		})
	}
	_ = g.Wait()

	return result, nil
}

// errSkipped is a sentinel string used internally to distinguish "already
// existed" from real failures. Never surfaced to callers.
const errSkipped = "skipped: template already exists"

// bootstrapOne executes the full per-(node, OS) pipeline. Always returns a
// populated TemplateOutcome — never an error directly, since one failure
// shouldn't abort the surrounding loop.
func (s *Service) bootstrapOne(
	ctx context.Context,
	node string,
	entry OSEntry,
	force bool,
) TemplateOutcome {
	start := time.Now()
	outcome := TemplateOutcome{OS: entry.OS, Node: node}

	finish := func(vmid int, errMsg string) TemplateOutcome {
		outcome.VMID = vmid
		outcome.Duration = time.Since(start).Round(time.Second)
		outcome.Error = errMsg
		return outcome
	}

	// Step 1: idempotency check via the (node, os) → vmid mapping in SQLite.
	// If a row exists AND Proxmox confirms the template is still there, skip.
	// If the DB row is stale (template manually destroyed), drop it and
	// continue as fresh.
	if !force {
		var existing db.NodeTemplate
		err := s.db.WithContext(ctx).
			Where("node = ? AND os = ?", node, entry.OS).
			First(&existing).Error
		if err == nil {
			alive, terr := s.px.TemplateExists(ctx, node, existing.VMID)
			if terr != nil {
				return finish(existing.VMID, fmt.Sprintf("template existence check: %v", terr))
			}
			if alive {
				return finish(existing.VMID, errSkipped)
			}
			// Template gone but DB row remains → drop it and re-create.
			if err := s.db.WithContext(ctx).
				Where("node = ? AND os = ?", node, entry.OS).
				Delete(&db.NodeTemplate{}).Error; err != nil {
				return finish(existing.VMID, fmt.Sprintf("clear stale db row: %v", err))
			}
		} else if !errors.Is(err, gorm.ErrRecordNotFound) {
			return finish(0, fmt.Sprintf("db lookup: %v", err))
		}
	}

	// Step 2: download cloud image to ImageStorage on this node — but skip
	// if it's already cached. Proxmox refuses to overwrite existing files,
	// so a naive re-run would fail with "refusing to override".
	hasFile, err := s.px.StorageHasFile(ctx, node, s.cfg.ImageStorage, importContentType, entry.Filename)
	if err != nil {
		return finish(0, fmt.Sprintf("storage existence check: %v", err))
	}
	if !hasFile {
		dlTask, err := s.px.DownloadStorageURL(ctx, node, s.cfg.ImageStorage, importContentType, entry.URL, entry.Filename)
		if err != nil {
			return finish(0, fmt.Sprintf("download dispatch: %v", err))
		}
		if err := s.px.WaitForTask(ctx, node, dlTask, downloadPollInterval); err != nil {
			return finish(0, fmt.Sprintf("download task: %v", err))
		}
	}

	// Steps 3+4: get a Proxmox-assigned VMID and immediately create the VM
	// with it. These two MUST be serialized cluster-wide — if two goroutines
	// both query NextVMID before either creates, Proxmox can return the
	// same VMID to both. Once Create succeeds, the VMID is "taken" and the
	// next caller's NextVMID returns a higher value.
	s.nextVMIDMu.Lock()
	vmid, err := s.px.NextVMID(ctx)
	if err != nil {
		s.nextVMIDMu.Unlock()
		return finish(0, fmt.Sprintf("nextid: %v", err))
	}

	// Step 4: create VM with the downloaded image as scsi0 (import-from).
	//
	// IMPORTANT: API tokens (even root@pam!*) cannot pass raw filesystem
	// paths — Proxmox refuses with "Only root can pass arbitrary filesystem
	// paths." The workaround is to use Proxmox volume references (volids):
	//   <storage>:import/<filename>
	// The content type MUST be "import" (or "images"); "iso" is rejected by
	// import-from with "wrong type 'iso' - needs to be 'images' or 'import'".
	imageVolid := fmt.Sprintf("%s:%s/%s", s.cfg.ImageStorage, importContentType, entry.Filename)
	createTask, err := s.px.CreateVMWithImport(ctx, node, vmid, proxmox.CreateVMOpts{
		Name:         fmt.Sprintf("%s-template", entry.OS),
		Memory:       1024,
		Cores:        1,
		DiskStorage:  s.cfg.DiskStorage,
		ImagePath:    imageVolid,
		SerialOnly:   true,
		AgentEnabled: true,
	})
	// Release the NextVMID lock as soon as the create call returns — the
	// VMID is now claimed in Proxmox's view. We hold it across the dispatch
	// call (not just the WaitForTask) because Proxmox marks the VMID as
	// taken at dispatch time, not task-completion time.
	s.nextVMIDMu.Unlock()
	if err != nil {
		return finish(vmid, fmt.Sprintf("vm create dispatch: %v", err))
	}
	if err := s.px.WaitForTask(ctx, node, createTask, createPollInterval); err != nil {
		return finish(vmid, fmt.Sprintf("vm create task: %v", err))
	}

	// Step 5: attach cloud-init drive (required, see provision gotcha #4)
	if err := s.px.SetCloudInitDrive(ctx, node, vmid, s.cfg.DiskStorage); err != nil {
		return finish(vmid, fmt.Sprintf("attach cloud-init drive: %v", err))
	}

	// Step 6: convert to immutable template
	if err := s.px.ConvertToTemplate(ctx, node, vmid); err != nil {
		return finish(vmid, fmt.Sprintf("convert to template: %v", err))
	}

	// Step 7: persist the (node, OS) → vmid mapping so future bootstrap and
	// provision lookups can find it. ON CONFLICT DO NOTHING handles the
	// rare race where two concurrent bootstrap calls both create — first
	// writer wins, second silently no-ops.
	if err := s.db.WithContext(ctx).
		Clauses(clause.OnConflict{DoNothing: true}).
		Create(&db.NodeTemplate{Node: node, OS: entry.OS, VMID: vmid}).Error; err != nil {
		return finish(vmid, fmt.Sprintf("persist node_template row: %v", err))
	}

	return finish(vmid, "")
}

// resolveNodes returns the target node list. If the request is empty, all
// online nodes from the cluster are used. Offline nodes are silently skipped
// even when explicitly requested — there's nothing useful we could do for
// them and surfacing a per-node error for "node down" is noise.
func (s *Service) resolveNodes(ctx context.Context, requested []string) ([]string, error) {
	clusterNodes, err := s.px.GetNodes(ctx)
	if err != nil {
		return nil, err
	}
	online := map[string]bool{}
	for _, n := range clusterNodes {
		if n.Status == "online" {
			online[n.Name] = true
		}
	}
	if len(requested) == 0 {
		out := make([]string, 0, len(online))
		for name := range online {
			out = append(out, name)
		}
		return out, nil
	}
	out := make([]string, 0, len(requested))
	for _, name := range requested {
		if online[name] {
			out = append(out, name)
		}
	}
	return out, nil
}

// resolveOSes validates the requested OS keys against the catalogue. Returns
// the full catalogue when nothing is specified.
func (s *Service) resolveOSes(requested []string) ([]OSEntry, error) {
	if len(requested) == 0 {
		out := make([]OSEntry, len(Catalog))
		copy(out, Catalog)
		return out, nil
	}
	out := make([]OSEntry, 0, len(requested))
	for _, k := range requested {
		entry := LookupOS(k)
		if entry == nil {
			return nil, fmt.Errorf("unknown OS %q (must be one of: %v)", k, AllOSKeys())
		}
		out = append(out, *entry)
	}
	return out, nil
}

// Compile-time assertion that *proxmox.Client satisfies our interface.
var _ ProxmoxClient = (*proxmox.Client)(nil)
