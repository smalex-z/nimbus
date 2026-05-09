package proxmox

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"
)

// LXCCreateOpts is the input to CreateLXC. Mirrors the shape of
// CreateVMOpts but for the Linux-container endpoint
// (/nodes/{n}/lxc).
//
// Nimbus uses LXCs for VPC gateway boxes — small Alpine containers
// that do nothing but `iptables -t nat -A POSTROUTING -j MASQUERADE`.
// We keep the option surface narrow to what those containers need;
// power users still go through the PVE web UI for anything fancier.
type LXCCreateOpts struct {
	// VMID is the LXC's cluster-wide identifier. Caller picks via
	// NextVMIDFrom (same allocation pool as VMs — Proxmox enforces
	// uniqueness across both QEMU + LXC).
	VMID int
	// OSTemplate is the volid of the LXC root template, e.g.
	// "local:vztmpl/alpine-3.20-default_20240908_amd64.tar.xz".
	// Use ListStorageContent(node, storage, "vztmpl") to find one
	// that's already cached, or DownloadStorageURL to fetch fresh.
	OSTemplate string
	// Hostname inside the container. ASCII letters/digits/hyphens;
	// Proxmox's validator rejects anything else with a generic 400.
	Hostname string
	// Storage is the disk-storage pool the rootfs lives on
	// (e.g. "local-lvm"). 256 MiB is plenty for a gateway LXC's
	// busybox + iptables footprint.
	Storage string
	// RootDiskGiB sizes the rootfs. 1 GiB is the realistic minimum
	// for an Alpine gateway with iptables/openrc/sysctl configs.
	RootDiskGiB int
	// MemoryMiB is the cgroup memory limit. 64 MiB is enough for
	// idle gateway duty; 128 MiB gives headroom for apk add /
	// iptables-restore on boot.
	MemoryMiB int
	// Cores is the CPU quota (number of vCPUs the container can
	// pin to). 1 is fine for a gateway LXC.
	Cores int
	// Net0/Net1 are the network interface specs. Each is a Proxmox
	// netN string like "name=eth0,bridge=vmbr0,ip=192.168.1.10/24,gw=192.168.1.1".
	// Empty string skips that NIC. The caller is responsible for
	// formatting; SDNNetSpec is a small helper.
	Net0 string
	Net1 string
	// Unprivileged=1 runs the container with user-namespace isolation
	// (more secure default; required for cgroupv2 hosts). The gateway
	// LXC needs CAP_NET_ADMIN for iptables, so callers may set this
	// to false plus Features below — but the default-on path is
	// safer and works on PVE 8.x with the right features flag.
	Unprivileged bool
	// Features enables container features that need extra perms.
	// Features turns on optional cgroup/namespace knobs (e.g.
	// `nesting=1`). PVE blocks every flag except `nesting` for API
	// tokens, so callers running as a token should leave this empty.
	Features string
	// Start=true brings the container up after create. Default false
	// so the caller can apply more config before first boot.
	Start bool
	// SSHPublicKeys, when non-empty, is written into the freshly-
	// created container's /root/.ssh/authorized_keys. PVE handles
	// the rootfs injection itself; we just pass the key string. Used
	// by gateway.Service to bootstrap NAT setup over SSH (PVE has no
	// REST endpoint for `pct exec`, so we authenticate to the
	// container's sshd over the host network instead).
	SSHPublicKeys string
}

// CreateLXC provisions a new Linux container on the given node with
// the supplied options. Returns the create-task UPID; caller polls
// via WaitForTask.
//
// The LXC is created in stopped state by default — caller does
// SetLXCConfig / StartLXC as a separate step. This matches our VM
// flow's clone → set-cloud-init → start ordering and keeps the
// failure modes in distinct stages.
func (c *Client) CreateLXC(ctx context.Context, node string, opts LXCCreateOpts) (string, error) {
	if opts.VMID == 0 {
		return "", fmt.Errorf("create lxc: vmid required")
	}
	if opts.OSTemplate == "" {
		return "", fmt.Errorf("create lxc: os template required")
	}
	if opts.Hostname == "" {
		return "", fmt.Errorf("create lxc: hostname required")
	}

	params := url.Values{}
	params.Set("vmid", strconv.Itoa(opts.VMID))
	params.Set("ostemplate", opts.OSTemplate)
	params.Set("hostname", opts.Hostname)
	if opts.Storage != "" && opts.RootDiskGiB > 0 {
		// Format: "<storage>:<size-in-GiB>" tells PVE to allocate a
		// fresh volume of the given size on the named storage. The
		// resulting volid is recorded on the container config.
		params.Set("rootfs", fmt.Sprintf("%s:%d", opts.Storage, opts.RootDiskGiB))
	}
	if opts.MemoryMiB > 0 {
		params.Set("memory", strconv.Itoa(opts.MemoryMiB))
	}
	if opts.Cores > 0 {
		params.Set("cores", strconv.Itoa(opts.Cores))
	}
	if opts.Net0 != "" {
		params.Set("net0", opts.Net0)
	}
	if opts.Net1 != "" {
		params.Set("net1", opts.Net1)
	}
	if opts.Unprivileged {
		params.Set("unprivileged", "1")
	}
	if opts.Features != "" {
		params.Set("features", opts.Features)
	}
	if opts.Start {
		params.Set("start", "1")
	}
	if opts.SSHPublicKeys != "" {
		params.Set("ssh-public-keys", opts.SSHPublicKeys)
	}

	var taskID string
	path := fmt.Sprintf("/nodes/%s/lxc", url.PathEscape(node))
	if err := c.do(ctx, http.MethodPost, path, params, &taskID); err != nil {
		return "", err
	}
	return taskID, nil
}

// LXCStatus returns the live `status` field for a single container,
// e.g. "running" / "stopped". Used by gateway.Service.SweepHealth
// to flip a VPC to `degraded` when its gateway LXC isn't running.
func (c *Client) LXCStatus(ctx context.Context, node string, vmid int) (string, error) {
	var out struct {
		Status string `json:"status"`
	}
	path := fmt.Sprintf("/nodes/%s/lxc/%d/status/current", url.PathEscape(node), vmid)
	if err := c.do(ctx, http.MethodGet, path, nil, &out); err != nil {
		return "", err
	}
	return out.Status, nil
}

// StartLXC powers on a stopped container. Returns the task UPID.
func (c *Client) StartLXC(ctx context.Context, node string, vmid int) (string, error) {
	var taskID string
	path := fmt.Sprintf("/nodes/%s/lxc/%d/status/start", url.PathEscape(node), vmid)
	if err := c.do(ctx, http.MethodPost, path, url.Values{}, &taskID); err != nil {
		return "", err
	}
	return taskID, nil
}

// StopLXC forces a container off (immediate, no graceful shutdown).
// Use this before DestroyLXC — Proxmox refuses to delete a running
// LXC. Returns the task UPID.
func (c *Client) StopLXC(ctx context.Context, node string, vmid int) (string, error) {
	var taskID string
	path := fmt.Sprintf("/nodes/%s/lxc/%d/status/stop", url.PathEscape(node), vmid)
	if err := c.do(ctx, http.MethodPost, path, url.Values{}, &taskID); err != nil {
		return "", err
	}
	return taskID, nil
}

// DestroyLXC removes a container from Proxmox. Sets purge=1 (clears
// HA + replication references) and destroy-unreferenced-disks=1
// (frees the rootfs volume). Caller must ensure the LXC is stopped
// first. Returns the task UPID.
func (c *Client) DestroyLXC(ctx context.Context, node string, vmid int) (string, error) {
	params := url.Values{}
	params.Set("purge", "1")
	params.Set("destroy-unreferenced-disks", "1")
	var taskID string
	path := fmt.Sprintf("/nodes/%s/lxc/%d", url.PathEscape(node), vmid)
	if err := c.do(ctx, http.MethodDelete, path, params, &taskID); err != nil {
		return "", err
	}
	return taskID, nil
}

// SetLXCConfig updates a stopped container's config. Mirrors the
// per-key form-encoded shape of /qemu/{vmid}/config; pass any of
// the netN/cores/memory/features/etc. keys you want to change.
//
// LXC config CAN be edited while the container is running, but most
// changes don't take effect until restart — Nimbus only uses this
// on freshly-created stopped containers, so we don't bother
// distinguishing.
func (c *Client) SetLXCConfig(ctx context.Context, node string, vmid int, params url.Values) error {
	if len(params) == 0 {
		return nil
	}
	path := fmt.Sprintf("/nodes/%s/lxc/%d/config", url.PathEscape(node), vmid)
	return c.do(ctx, http.MethodPut, path, params, nil)
}

// LXCExecResult is the parsed output of /lxc/{vmid}/exec — Proxmox
// runs the command synchronously and returns stdout/stderr as
// strings plus the exit code.
type LXCExecResult struct {
	OutData  string `json:"out-data"`
	ErrData  string `json:"err-data"`
	ExitCode int    `json:"exitcode"`
}

// LXCExec runs a command inside a running container via the
// /lxc/{vmid}/exec endpoint (synchronous, blocks until the command
// exits). The command is passed as a list — first element is the
// binary, subsequent elements are arguments.
//
// Used by the gateway-LXC bootstrap flow to install iptables,
// configure sysctl, and persist MASQUERADE rules across reboots.
// We don't use a TTY (interactive=0); stdout/stderr are captured.
//
// Note this endpoint requires `noVNC` permission in newer PVE
// versions; Nimbus's API token needs PVE permission
// `VM.Console` on /vms/<vmid> for the LXC.
func (c *Client) LXCExec(ctx context.Context, node string, vmid int, command []string) (*LXCExecResult, error) {
	if len(command) == 0 {
		return nil, fmt.Errorf("lxc exec: empty command")
	}
	params := url.Values{}
	for _, arg := range command {
		params.Add("command", arg)
	}
	var res LXCExecResult
	path := fmt.Sprintf("/nodes/%s/lxc/%d/exec", url.PathEscape(node), vmid)
	if err := c.do(ctx, http.MethodPost, path, params, &res); err != nil {
		return nil, err
	}
	return &res, nil
}

// LXCExecShell is a small convenience for running a multi-line
// shell script via `sh -c '...'`. Single-quote escaping handled
// here so the caller can paste arbitrary shell.
func (c *Client) LXCExecShell(ctx context.Context, node string, vmid int, script string) (*LXCExecResult, error) {
	// Single-quote the script and escape any embedded single quotes.
	quoted := "'" + strings.ReplaceAll(script, "'", `'\''`) + "'"
	return c.LXCExec(ctx, node, vmid, []string{"sh", "-c", quoted})
}

// AplinfoTemplate is one entry from `GET /nodes/{node}/aplinfo` —
// the curated list of LXC templates Proxmox can fetch from its
// official template repo. Same source `pveam available` reads.
type AplinfoTemplate struct {
	Template string `json:"template"` // e.g. "alpine-3.21-default_20241217_amd64.tar.xz"
	Section  string `json:"section"`  // "system", "turnkeylinux", etc
	OS       string `json:"os"`
	Version  string `json:"version"`
	Type     string `json:"type"` // "lxc"
}

// ListAvailableLXCTemplates returns every template the apt-like
// PVE template repo knows about, fetched from
// `GET /nodes/{node}/aplinfo`. The result is filtered to lxc-type
// system templates so callers can pick by name without sorting
// through TurnKey appliances.
//
// Cached server-side by Proxmox; cheap to call repeatedly.
func (c *Client) ListAvailableLXCTemplates(ctx context.Context, node string) ([]AplinfoTemplate, error) {
	var out []AplinfoTemplate
	path := fmt.Sprintf("/nodes/%s/aplinfo", url.PathEscape(node))
	if err := c.do(ctx, http.MethodGet, path, nil, &out); err != nil {
		return nil, err
	}
	keep := out[:0]
	for _, t := range out {
		if t.Type == "lxc" || t.Type == "" { // PVE 9 sometimes omits Type
			keep = append(keep, t)
		}
	}
	return keep, nil
}

// DownloadLXCTemplate dispatches an aplinfo template download to a
// node's storage, returning the worker UPID for caller-side
// WaitForTask. Uses `POST /nodes/{node}/aplinfo` — the same call
// `pveam download <storage> <template>` makes.
//
// templateName is the bare filename Proxmox knows from `pveam
// available` (e.g. "alpine-3.21-default_20241217_amd64.tar.xz") —
// NOT a volid. Downloaded files land at
// `<storage>:vztmpl/<templateName>`.
//
// Returns (upid, error). Idempotency is the caller's job — Proxmox
// 400s if the file already exists, so check StorageHasFile first.
func (c *Client) DownloadLXCTemplate(ctx context.Context, node, storage, templateName string) (string, error) {
	if storage == "" {
		return "", fmt.Errorf("download lxc template: storage required")
	}
	if templateName == "" {
		return "", fmt.Errorf("download lxc template: template name required")
	}
	params := url.Values{}
	params.Set("storage", storage)
	params.Set("template", templateName)
	var taskID string
	path := fmt.Sprintf("/nodes/%s/aplinfo", url.PathEscape(node))
	if err := c.do(ctx, http.MethodPost, path, params, &taskID); err != nil {
		return "", err
	}
	return taskID, nil
}
