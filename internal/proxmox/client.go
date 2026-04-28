// Package proxmox is a focused REST client for the Proxmox VE API.
//
// Scope is intentionally narrow — only the endpoints Nimbus's provision flow
// needs are wrapped (clone, set-cloud-init, resize, start, agent IP probe,
// node listing, task polling). Everything else is left to direct HTTP if it's
// ever required.
//
// All write operations are sent as application/x-www-form-urlencoded payloads
// per the Proxmox API spec. The client always uses Bearer-style PVEAPIToken
// auth; it does not implement ticket+CSRF auth because tokens are sufficient
// and skip CSRF entirely.
//
// TLS verification is disabled by default — Proxmox ships with a self-signed
// certificate. In production with a CA-signed cert this should be revisited.
package proxmox

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"
)

// listClusterIPsConcurrency caps the number of in-flight per-node walks during
// ListClusterIPs. Eight is a balance between throughput on small clusters and
// not stampeding the Proxmox API on a cluster with many nodes.
const listClusterIPsConcurrency = 8

// perNodeVMConcurrency caps the number of per-VM enrichment goroutines a
// single node walk fans out during GetClusterVMDetails. Most of the per-VM
// work is HTTP roundtrips to Proxmox (config + agent), so 8 in flight per
// node turns a 50-VM serial chain (~15s on cold cache) into ~2s without
// pushing pveproxy into rate-limit territory.
const perNodeVMConcurrency = 8

// Client is a Proxmox API client bound to one cluster endpoint and one token.
// Safe for concurrent use.
type Client struct {
	host    string // e.g. https://hppve.uclaacm.com:8006
	authHdr string // "PVEAPIToken=user@realm!tokenname=secret"
	http    *http.Client

	// osInfoCache memoizes guest-get-osinfo results across calls so the admin
	// view (which polls every 15s) doesn't repeatedly hit every running VM's
	// agent. OS details rarely change on a running VM — a 24h TTL is fine.
	// Negative entries (agent unreachable) get a much shorter TTL so freshly
	// booted VMs surface their info promptly.
	osInfoCache *osInfoCache
}

// osInfoCache caches qemu-guest-agent osinfo by (node, vmid). Positive entries
// live for osInfoTTLPositive; negative ("agent unreachable") entries live for
// osInfoTTLNegative so a VM that just got the agent up is rediscovered within
// minutes. Safe for concurrent use.
type osInfoCache struct {
	mu      sync.Mutex
	entries map[osInfoKey]osInfoEntry
}

type osInfoKey struct {
	node string
	vmid int
}

type osInfoEntry struct {
	info      *OSInfo // nil for negative entries
	expiresAt time.Time
}

const (
	osInfoTTLPositive = 24 * time.Hour
	osInfoTTLNegative = 5 * time.Minute
)

func newOSInfoCache() *osInfoCache {
	return &osInfoCache{entries: make(map[osInfoKey]osInfoEntry)}
}

// get returns (info, hit). hit=true means the cache had a non-stale entry —
// info may still be nil (negative cache for agentless VMs).
func (c *osInfoCache) get(node string, vmid int, now time.Time) (*OSInfo, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	e, ok := c.entries[osInfoKey{node, vmid}]
	if !ok || now.After(e.expiresAt) {
		return nil, false
	}
	return e.info, true
}

func (c *osInfoCache) put(node string, vmid int, info *OSInfo, now time.Time) {
	ttl := osInfoTTLPositive
	if info == nil {
		ttl = osInfoTTLNegative
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.entries[osInfoKey{node, vmid}] = osInfoEntry{info: info, expiresAt: now.Add(ttl)}
}

// New constructs a Client. host should be a fully-qualified URL including
// scheme and port (e.g. "https://localhost:8006"). tokenID and secret are the
// halves of the PVEAPIToken header value.
//
// timeout caps each individual HTTP request; pass 0 for the default 30s.
func New(host, tokenID, secret string, timeout time.Duration) *Client {
	if timeout == 0 {
		timeout = 30 * time.Second
	}
	return &Client{
		host:    strings.TrimRight(host, "/"),
		authHdr: fmt.Sprintf("PVEAPIToken=%s=%s", tokenID, secret),
		http: &http.Client{
			Timeout: timeout,
			Transport: &http.Transport{
				// Required: Proxmox ships with a self-signed cert.
				TLSClientConfig: &tls.Config{InsecureSkipVerify: true}, //nolint:gosec
			},
		},
		osInfoCache: newOSInfoCache(),
	}
}

// envelope is the standard Proxmox response wrapper. data is left as
// json.RawMessage so callers can decode it into the type they need.
type envelope struct {
	Data json.RawMessage `json:"data"`
}

// do executes an HTTP request and decodes the {data: ...} envelope into out.
// The body of write operations is form-encoded (NOT JSON) — Proxmox expects
// application/x-www-form-urlencoded for POST/PUT.
//
// out may be nil when the caller doesn't care about the payload.
func (c *Client) do(ctx context.Context, method, path string, params url.Values, out any) error {
	endpoint := c.host + "/api2/json" + path

	var body io.Reader
	if method != http.MethodGet && method != http.MethodDelete && params != nil {
		body = strings.NewReader(params.Encode())
	} else if (method == http.MethodGet || method == http.MethodDelete) && params != nil {
		endpoint += "?" + params.Encode()
	}

	req, err := http.NewRequestWithContext(ctx, method, endpoint, body)
	if err != nil {
		return fmt.Errorf("build %s %s: %w", method, path, err)
	}
	req.Header.Set("Authorization", c.authHdr)
	if body != nil {
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	}
	req.Header.Set("Accept", "application/json")

	resp, err := c.http.Do(req)
	if err != nil {
		return fmt.Errorf("%s %s: %w", method, path, err)
	}
	defer resp.Body.Close() //nolint:errcheck

	respBody, _ := io.ReadAll(resp.Body)

	if resp.StatusCode == http.StatusNotFound {
		return ErrNotFound
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return &HTTPError{
			Status: resp.StatusCode,
			Method: method,
			Path:   path,
			Body:   string(respBody),
		}
	}
	if out == nil {
		return nil
	}

	var env envelope
	if err := json.Unmarshal(respBody, &env); err != nil {
		return fmt.Errorf("decode envelope from %s %s: %w", method, path, err)
	}
	if len(env.Data) == 0 || string(env.Data) == "null" {
		return nil
	}
	if err := json.Unmarshal(env.Data, out); err != nil {
		return fmt.Errorf("decode data from %s %s: %w", method, path, err)
	}
	return nil
}

// GetNodes returns one entry per cluster node — used for scoring.
func (c *Client) GetNodes(ctx context.Context) ([]Node, error) {
	var nodes []Node
	if err := c.do(ctx, http.MethodGet, "/nodes", nil, &nodes); err != nil {
		return nil, err
	}
	return nodes, nil
}

// GetNodeStatus returns the per-node status block. Used to read swap counters
// (`/nodes` only reports physical memory). Callers fan this out across nodes
// in parallel.
func (c *Client) GetNodeStatus(ctx context.Context, node string) (*NodeStatus, error) {
	var status NodeStatus
	path := fmt.Sprintf("/nodes/%s/status", url.PathEscape(node))
	if err := c.do(ctx, http.MethodGet, path, nil, &status); err != nil {
		return nil, err
	}
	return &status, nil
}

// ListVMs returns the QEMU VMs on a single node — used for tie-break VM
// counts and for confirming a template VMID exists locally.
func (c *Client) ListVMs(ctx context.Context, node string) ([]VMStatus, error) {
	var vms []VMStatus
	path := fmt.Sprintf("/nodes/%s/qemu", url.PathEscape(node))
	if err := c.do(ctx, http.MethodGet, path, nil, &vms); err != nil {
		return nil, err
	}
	return vms, nil
}

// ListLXCs returns the LXC containers on a single node. Used by the IP
// reconciler so containers' static IPs are recognized (and not handed out
// to a new VM that would collide with them).
func (c *Client) ListLXCs(ctx context.Context, node string) ([]LXCStatus, error) {
	var cts []LXCStatus
	path := fmt.Sprintf("/nodes/%s/lxc", url.PathEscape(node))
	if err := c.do(ctx, http.MethodGet, path, nil, &cts); err != nil {
		return nil, err
	}
	return cts, nil
}

// GetLXCConfig fetches the raw config map for one LXC container. Same
// 500-with-"does-not-exist" normalization as GetVMConfig — Proxmox returns
// 500 (not 404) when the container is missing on the node.
func (c *Client) GetLXCConfig(ctx context.Context, node string, vmid int) (map[string]any, error) {
	var cfg vmConfig
	path := fmt.Sprintf("/nodes/%s/lxc/%d/config", url.PathEscape(node), vmid)
	err := c.do(ctx, http.MethodGet, path, nil, &cfg)
	if err != nil {
		if errors.Is(err, ErrNotFound) {
			return nil, ErrNotFound
		}
		var httpErr *HTTPError
		if errors.As(err, &httpErr) && strings.Contains(httpErr.Body, "does not exist") {
			return nil, ErrNotFound
		}
		return nil, fmt.Errorf("get lxc config %s/%d: %w", node, vmid, err)
	}
	return cfg, nil
}

// vmConfig is the raw config map returned by Proxmox. We only inspect a few
// fields so leave it as a generic map.
type vmConfig map[string]any

// GetVMConfig fetches the raw config map for one VM. Callers receive ErrNotFound
// when the VM is missing — including Proxmox's odd "500 with body 'does not
// exist'" response, which is normalized here so callers don't have to repeat
// the check.
func (c *Client) GetVMConfig(ctx context.Context, node string, vmid int) (map[string]any, error) {
	var cfg vmConfig
	path := fmt.Sprintf("/nodes/%s/qemu/%d/config", url.PathEscape(node), vmid)
	err := c.do(ctx, http.MethodGet, path, nil, &cfg)
	if err != nil {
		if errors.Is(err, ErrNotFound) {
			return nil, ErrNotFound
		}
		var httpErr *HTTPError
		if errors.As(err, &httpErr) && strings.Contains(httpErr.Body, "does not exist") {
			return nil, ErrNotFound
		}
		return nil, fmt.Errorf("get vm config %s/%d: %w", node, vmid, err)
	}
	return cfg, nil
}

// TemplateExists reports whether the template VMID exists on the node AND has
// a cloud-init drive attached. Without the cloud-init drive, SetCloudInit
// silently succeeds but the cloud-init config never reaches the booted VM —
// see design-doc gotcha #4.
func (c *Client) TemplateExists(ctx context.Context, node string, vmid int) (bool, error) {
	cfg, err := c.GetVMConfig(ctx, node, vmid)
	if err != nil {
		if errors.Is(err, ErrNotFound) {
			return false, nil
		}
		return false, err
	}
	// Look for any drive (ide*, scsi*, sata*, virtio*) whose value mentions
	// "cloudinit". This is the canonical way Proxmox attaches the cloud-init
	// drive — `qm set <vmid> --ide2 local-lvm:cloudinit` for example.
	for _, v := range cfg {
		s, ok := v.(string)
		if !ok {
			continue
		}
		if strings.Contains(s, "cloudinit") {
			return true, nil
		}
	}
	return false, nil
}

// NextVMID asks Proxmox for the next free cluster-wide VMID, starting from
// Proxmox's own floor (100). Use NextVMIDFrom when the caller needs to enforce
// a higher floor (e.g. templates in the 9000+ range).
func (c *Client) NextVMID(ctx context.Context) (int, error) {
	var raw string
	if err := c.do(ctx, http.MethodGet, "/cluster/nextid", nil, &raw); err != nil {
		return 0, err
	}
	id, err := strconv.Atoi(raw)
	if err != nil {
		return 0, fmt.Errorf("parse nextid %q: %w", raw, err)
	}
	return id, nil
}

// NextVMIDFrom returns the lowest free cluster-wide VMID at or above minVMID.
//
// Proxmox's /cluster/nextid endpoint always starts from 100 and has no
// "minimum" parameter, so we list every VM in the cluster ourselves and pick
// the lowest gap >= minVMID. One API call regardless of cluster size.
//
// Callers that need cluster-wide-unique VMID assignment in a reserved range
// (templates land in 9000+ to keep them out of the user-VM range) should use
// this instead of NextVMID. Callers MUST serialize the
// NextVMIDFrom → CreateVM pair (e.g. with a mutex) — there's no atomic
// "reserve" in the Proxmox API.
func (c *Client) NextVMIDFrom(ctx context.Context, minVMID int) (int, error) {
	if minVMID < 100 {
		minVMID = 100
	}
	var resources []struct {
		VMID int `json:"vmid"`
	}
	params := url.Values{}
	params.Set("type", "vm")
	if err := c.do(ctx, http.MethodGet, "/cluster/resources", params, &resources); err != nil {
		return 0, fmt.Errorf("list cluster vm resources: %w", err)
	}
	taken := make(map[int]bool, len(resources))
	for _, r := range resources {
		taken[r.VMID] = true
	}
	for id := minVMID; id <= 999_999_999; id++ {
		if !taken[id] {
			return id, nil
		}
	}
	return 0, fmt.Errorf("no free VMID at or above %d", minVMID)
}

// CloneVM clones a template into a new VM on the chosen target node. The
// `target` parameter is mandatory — without it Proxmox clones onto the
// template's source node, defeating Nimbus's node selection (gotcha #3 in the
// plan).
//
// Returns the task UPID for caller-side polling via WaitForTask.
func (c *Client) CloneVM(ctx context.Context, sourceNode, targetNode string, templateVMID, newVMID int, name string) (string, error) {
	params := url.Values{}
	params.Set("newid", strconv.Itoa(newVMID))
	params.Set("name", name)
	params.Set("target", targetNode)
	params.Set("full", "1")
	// NOTE: omitting `pool` here — assigning to a Proxmox pool requires the
	// pool to pre-exist on the cluster. Pools are an organizational feature,
	// not a functional requirement. Future enhancement: auto-create a
	// "nimbus" pool on first run via POST /pools, then opt VMs into it.

	var taskID string
	path := fmt.Sprintf("/nodes/%s/qemu/%d/clone", url.PathEscape(sourceNode), templateVMID)
	if err := c.do(ctx, http.MethodPost, path, params, &taskID); err != nil {
		return "", err
	}
	return taskID, nil
}

// WaitForTask polls a task's status until it reports stopped, returning an
// error if exitstatus != "OK". Polls every interval; total wait is bounded by
// ctx deadline.
func (c *Client) WaitForTask(ctx context.Context, node, taskID string, interval time.Duration) error {
	if interval == 0 {
		interval = 2 * time.Second
	}
	path := fmt.Sprintf("/nodes/%s/tasks/%s/status", url.PathEscape(node), url.PathEscape(taskID))

	for {
		var st taskStatus
		if err := c.do(ctx, http.MethodGet, path, nil, &st); err != nil {
			return fmt.Errorf("poll task %s: %w", taskID, err)
		}
		if st.Status == "stopped" {
			if st.ExitStatus == "OK" {
				return nil
			}
			return fmt.Errorf("task %s failed: %s", taskID, st.ExitStatus)
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("task %s wait: %w", taskID, ctx.Err())
		case <-time.After(interval):
		}
	}
}

// SetCloudInit applies cloud-init config to a VM.
//
// CRITICAL: the `sshkeys` field MUST be pre-URL-encoded before being placed
// in the form body. Proxmox does its own URL-decode of the value AFTER the
// form layer decodes it once — so the wire format is double-encoded. If
// passed plain ("ssh-ed25519 AAAA..."), Proxmox rejects with
// "invalid format - invalid urlencoded string". Documented in design doc
// §5.2 NOTE and confirmed by forum thread:
// https://forum.proxmox.com/threads/injecting-qemu-ssh-keys-via-the-api.118449/
//
// We use percent-encoding (%20 for spaces) instead of `+` for spaces, since
// some Proxmox parsers treat `+` as a literal in this field.
func (c *Client) SetCloudInit(ctx context.Context, node string, vmid int, cfg CloudInitConfig) error {
	params := url.Values{}
	if cfg.CIUser != "" {
		params.Set("ciuser", cfg.CIUser)
	}
	if cfg.SSHKeys != "" {
		// QueryEscape uses + for spaces; replace with %20 to be safe.
		encoded := strings.ReplaceAll(url.QueryEscape(cfg.SSHKeys), "+", "%20")
		params.Set("sshkeys", encoded)
	}
	if cfg.IPConfig0 != "" {
		params.Set("ipconfig0", cfg.IPConfig0)
	}
	if cfg.Nameserver != "" {
		params.Set("nameserver", cfg.Nameserver)
	}
	if cfg.SearchDomain != "" {
		params.Set("searchdomain", cfg.SearchDomain)
	}
	if cfg.Cores > 0 {
		params.Set("cores", strconv.Itoa(cfg.Cores))
	}
	if cfg.Memory > 0 {
		params.Set("memory", strconv.Itoa(cfg.Memory))
	}
	if cfg.CPU != "" {
		params.Set("cpu", cfg.CPU)
	}

	path := fmt.Sprintf("/nodes/%s/qemu/%d/config", url.PathEscape(node), vmid)
	return c.do(ctx, http.MethodPost, path, params, nil)
}

// SetVMTags writes the given tag list to a VM's `tags` config field, replacing
// any prior value. Pass an empty slice to clear all tags. Tags are sent as a
// `;`-separated string per Proxmox's wire format.
//
// Callers that need to preserve user-applied tags should read existing tags
// first (via GetVMConfig) and pass the merged list in.
func (c *Client) SetVMTags(ctx context.Context, node string, vmid int, tags []string) error {
	params := url.Values{}
	params.Set("tags", JoinTags(tags))
	path := fmt.Sprintf("/nodes/%s/qemu/%d/config", url.PathEscape(node), vmid)
	return c.do(ctx, http.MethodPost, path, params, nil)
}

// SetVMDescription writes a VM's `description` field, replacing the prior
// value verbatim. Pass an empty string to clear. Like tags, callers that
// need to preserve user-written prose should read existing first (via
// GetVMConfig) and pass the merged body in.
func (c *Client) SetVMDescription(ctx context.Context, node string, vmid int, description string) error {
	params := url.Values{}
	params.Set("description", description)
	path := fmt.Sprintf("/nodes/%s/qemu/%d/config", url.PathEscape(node), vmid)
	return c.do(ctx, http.MethodPost, path, params, nil)
}

// GetClusterTagStyle reads the `tag-style` property from /cluster/options.
// Returns the empty string when the option is unset on the cluster (which
// is the Proxmox default — every tag gets a hash-derived random color).
//
// Requires `Sys.Audit` on `/`. Errors propagate; the caller decides whether
// a missing option is a hard failure.
func (c *Client) GetClusterTagStyle(ctx context.Context) (string, error) {
	var opts map[string]any
	if err := c.do(ctx, http.MethodGet, "/cluster/options", nil, &opts); err != nil {
		return "", err
	}
	raw, _ := opts["tag-style"].(string)
	return raw, nil
}

// SetClusterTagStyle writes the `tag-style` property on /cluster/options,
// replacing any prior value. Requires `Sys.Modify` on `/` — a token without
// it gets a 403, which the caller should treat as "operator hasn't granted
// us permission to set tag colors" and degrade gracefully.
func (c *Client) SetClusterTagStyle(ctx context.Context, tagStyle string) error {
	params := url.Values{}
	params.Set("tag-style", tagStyle)
	return c.do(ctx, http.MethodPut, "/cluster/options", params, nil)
}

// ResizeDisk grows a disk on a stopped VM. size is the Proxmox-style delta —
// "+10G" adds 10 gigabytes.
func (c *Client) ResizeDisk(ctx context.Context, node string, vmid int, disk, size string) error {
	params := url.Values{}
	params.Set("disk", disk)
	params.Set("size", size)
	path := fmt.Sprintf("/nodes/%s/qemu/%d/resize", url.PathEscape(node), vmid)
	return c.do(ctx, http.MethodPut, path, params, nil)
}

// StartVM powers on a VM. Returns the task UPID for the start task.
func (c *Client) StartVM(ctx context.Context, node string, vmid int) (string, error) {
	var taskID string
	path := fmt.Sprintf("/nodes/%s/qemu/%d/status/start", url.PathEscape(node), vmid)
	if err := c.do(ctx, http.MethodPost, path, url.Values{}, &taskID); err != nil {
		return "", err
	}
	return taskID, nil
}

// StopVM forces a VM off (equivalent of pulling the plug). Use this before
// DestroyVM since Proxmox refuses to delete a running VM. The graceful
// alternative is `shutdown`, which depends on the guest agent — for the
// destroy path we want a guaranteed power-down within seconds.
func (c *Client) StopVM(ctx context.Context, node string, vmid int) (string, error) {
	var taskID string
	path := fmt.Sprintf("/nodes/%s/qemu/%d/status/stop", url.PathEscape(node), vmid)
	if err := c.do(ctx, http.MethodPost, path, url.Values{}, &taskID); err != nil {
		return "", err
	}
	return taskID, nil
}

// ShutdownVM asks Proxmox to stop a VM gracefully — talks to the guest agent
// (or sends ACPI when no agent) so the OS can flush filesystems and exit
// cleanly. The Proxmox endpoint falls back to forceStop after its own
// configured timeout, so a hung guest still ends up off without our caller
// babysitting. Used for the user-facing "Shutdown" button.
func (c *Client) ShutdownVM(ctx context.Context, node string, vmid int) (string, error) {
	var taskID string
	path := fmt.Sprintf("/nodes/%s/qemu/%d/status/shutdown", url.PathEscape(node), vmid)
	if err := c.do(ctx, http.MethodPost, path, url.Values{}, &taskID); err != nil {
		return "", err
	}
	return taskID, nil
}

// RebootVM triggers a graceful reboot through the guest agent (or ACPI when
// no agent is present). Used after a cloud-init network change so the VM picks
// up the new ipconfig0 on next boot. Returns the task UPID.
func (c *Client) RebootVM(ctx context.Context, node string, vmid int) (string, error) {
	var taskID string
	path := fmt.Sprintf("/nodes/%s/qemu/%d/status/reboot", url.PathEscape(node), vmid)
	if err := c.do(ctx, http.MethodPost, path, url.Values{}, &taskID); err != nil {
		return "", err
	}
	return taskID, nil
}

// DestroyVM removes a VM from Proxmox.
//
//   - purge=1 also clears job/replication/HA references (otherwise stale
//     entries get left behind).
//   - destroy-unreferenced-disks=1 frees attached disks rather than orphaning
//     them on the storage backend.
//
// Caller must ensure the VM is stopped first; Proxmox returns an error if
// it's still running. Returns the task UPID for caller-side polling.
func (c *Client) DestroyVM(ctx context.Context, node string, vmid int) (string, error) {
	params := url.Values{}
	params.Set("purge", "1")
	params.Set("destroy-unreferenced-disks", "1")

	var taskID string
	path := fmt.Sprintf("/nodes/%s/qemu/%d", url.PathEscape(node), vmid)
	if err := c.do(ctx, http.MethodDelete, path, params, &taskID); err != nil {
		return "", err
	}
	return taskID, nil
}

// agentResult is what the guest-agent endpoint actually returns: a wrapper
// around an array of NetworkInterface records.
type agentResult struct {
	Result []NetworkInterface `json:"result"`
}

// GetAgentInterfaces reads the qemu-guest-agent's network-get-interfaces
// output. The agent returns 500 if the VM hasn't booted or doesn't have the
// agent installed/running — callers should treat this as "not ready" and
// retry, not as a hard failure.
func (c *Client) GetAgentInterfaces(ctx context.Context, node string, vmid int) ([]NetworkInterface, error) {
	var res agentResult
	path := fmt.Sprintf("/nodes/%s/qemu/%d/agent/network-get-interfaces", url.PathEscape(node), vmid)
	if err := c.do(ctx, http.MethodGet, path, nil, &res); err != nil {
		return nil, err
	}
	return res.Result, nil
}

// agentOSInfoResult wraps the qemu-guest-agent's get-osinfo response, which
// nests the actual record under a `result` key.
type agentOSInfoResult struct {
	Result OSInfo `json:"result"`
}

// GetAgentOSInfo reads guest-get-osinfo from the qemu-guest-agent. Same
// caveats as GetAgentInterfaces — returns 500 when the agent is unavailable.
// Callers should treat any error as "info not available right now".
func (c *Client) GetAgentOSInfo(ctx context.Context, node string, vmid int) (*OSInfo, error) {
	var res agentOSInfoResult
	path := fmt.Sprintf("/nodes/%s/qemu/%d/agent/get-osinfo", url.PathEscape(node), vmid)
	if err := c.do(ctx, http.MethodGet, path, nil, &res); err != nil {
		return nil, err
	}
	return &res.Result, nil
}

// GetAgentOSInfoCached returns the qemu-guest-agent osinfo for a VM, served
// from a 24h TTL cache. Cache misses fall through to GetAgentOSInfo; agent
// failures are negatively cached for 5 minutes so a freshly booted VM gets
// rediscovered without re-probing every poll cycle.
//
// A nil return means the agent didn't yield usable info (still cached).
// Callers should treat that as "no enriched OS data available".
func (c *Client) GetAgentOSInfoCached(ctx context.Context, node string, vmid int) *OSInfo {
	now := time.Now()
	if info, hit := c.osInfoCache.get(node, vmid, now); hit {
		return info
	}
	info, err := c.GetAgentOSInfo(ctx, node, vmid)
	if err != nil || info == nil || info.ID == "" {
		c.osInfoCache.put(node, vmid, nil, now)
		return nil
	}
	c.osInfoCache.put(node, vmid, info, now)
	return info
}

// ProbeReachability TCP-dials each address concurrently and returns the set
// of node names that didn't answer within timeout. An empty map means all
// addresses responded (or the input map was empty).
//
// The reconcilers use this as a skip-the-reaper guard: if a node went off
// the network briefly (kernel update, ifdown, broken switch port), Proxmox's
// /cluster/resources stops listing its VMs, every IP allocated to one of
// those VMs flows through the missed-cycle path, and after VACATE_MISS_THRESHOLD
// cycles the rows get vacated and soft-deleted. Skipping the bump on
// unreachable nodes keeps the local DB intact across short outages —
// Proxmox's `online` flag is unreliable when nimbus's own host is the one
// with bad cluster connectivity.
//
// Port 8006 is pveproxy's default. We're not authenticating — a successful
// TCP handshake is enough to say "the box is alive and routing".
func ProbeReachability(ctx context.Context, addresses map[string]string, timeout time.Duration) map[string]bool {
	if len(addresses) == 0 {
		return nil
	}
	out := make(map[string]bool)
	var mu sync.Mutex
	var wg sync.WaitGroup
	for name, ip := range addresses {
		wg.Add(1)
		go func(name, ip string) {
			defer wg.Done()
			d := net.Dialer{Timeout: timeout}
			conn, err := d.DialContext(ctx, "tcp", net.JoinHostPort(ip, "8006"))
			if err != nil {
				mu.Lock()
				out[name] = true
				mu.Unlock()
				return
			}
			_ = conn.Close()
		}(name, ip)
	}
	wg.Wait()
	if len(out) == 0 {
		return nil
	}
	return out
}

// GetClusterStatus returns the heterogeneous /cluster/status payload — one
// entry per node carrying its corosync address, plus a cluster-summary row.
// Callers usually want NodeAddresses, not raw entries.
func (c *Client) GetClusterStatus(ctx context.Context) ([]ClusterStatusEntry, error) {
	var out []ClusterStatusEntry
	if err := c.do(ctx, http.MethodGet, "/cluster/status", nil, &out); err != nil {
		return nil, err
	}
	return out, nil
}

// NodeAddresses returns name → IP for every node in the cluster, derived from
// /cluster/status. Used by the IP-pool labeller (so a hypervisor's own LAN
// address shows as "PROXMOX NODE foo" instead of generic EXTERNAL) and the
// reconciler reachability guard (TCP-probe before counting a missing-VM
// cycle so a brief node outage doesn't reap rows en masse).
func (c *Client) NodeAddresses(ctx context.Context) (map[string]string, error) {
	entries, err := c.GetClusterStatus(ctx)
	if err != nil {
		return nil, err
	}
	out := make(map[string]string, len(entries))
	for _, e := range entries {
		if e.Type == "node" && e.Name != "" && e.IP != "" {
			out[e.Name] = e.IP
		}
	}
	return out, nil
}

// GetClusterStorage returns every storage entry visible at the cluster level.
// Shared storage appears once per node; callers must dedupe by Storage name.
func (c *Client) GetClusterStorage(ctx context.Context) ([]ClusterStorage, error) {
	var out []ClusterStorage
	params := url.Values{}
	params.Set("type", "storage")
	if err := c.do(ctx, http.MethodGet, "/cluster/resources", params, &out); err != nil {
		return nil, err
	}
	return out, nil
}

// GetClusterVMs returns every VM (and template) on every node in one call.
// Replaces the per-node ListVMs fan-out for callers that need cluster-wide
// VM telemetry — notably the node scorer, which needs both VM counts and
// per-node committed-RAM totals (sum of MaxMem across non-template rows).
func (c *Client) GetClusterVMs(ctx context.Context) ([]ClusterVM, error) {
	var out []ClusterVM
	params := url.Values{}
	params.Set("type", "vm")
	if err := c.do(ctx, http.MethodGet, "/cluster/resources", params, &out); err != nil {
		return nil, err
	}
	return out, nil
}

// GetStorages lists the storage backends configured on a node. Used by the
// bootstrap flow to detect which storage to use for downloaded cloud images
// (needs `iso` content) vs. VM disks (needs `images` content).
func (c *Client) GetStorages(ctx context.Context, node string) ([]Storage, error) {
	var out []Storage
	path := fmt.Sprintf("/nodes/%s/storage", url.PathEscape(node))
	if err := c.do(ctx, http.MethodGet, path, nil, &out); err != nil {
		return nil, err
	}
	return out, nil
}

// GetStorageConfig returns the cluster-wide configuration for one storage,
// including the comma-separated list of content types it accepts.
func (c *Client) GetStorageConfig(ctx context.Context, storage string) (*Storage, error) {
	var s Storage
	path := fmt.Sprintf("/storage/%s", url.PathEscape(storage))
	if err := c.do(ctx, http.MethodGet, path, nil, &s); err != nil {
		return nil, err
	}
	// /storage/{name} returns the storage config without "storage" populated;
	// fill it in for the caller.
	if s.Storage == "" {
		s.Storage = storage
	}
	return &s, nil
}

// EnsureStorageContent guarantees the given storage accepts a particular
// content type (e.g. "import"). If the type is already in the storage's
// content list this is a no-op; otherwise the storage is reconfigured.
//
// This is a cluster-wide operation that affects every node sharing the
// storage. It only ADDS a content type — never removes — so it's safe to
// call repeatedly and won't disturb existing usage.
//
// Used by bootstrap to make a `dir` storage usable as both an ISO download
// target AND a source for `import-from` (which requires `import` or `images`
// content type, while download-url plus the existing usage requires `iso`).
func (c *Client) EnsureStorageContent(ctx context.Context, storage, contentType string) error {
	cur, err := c.GetStorageConfig(ctx, storage)
	if err != nil {
		return fmt.Errorf("get storage %s config: %w", storage, err)
	}
	parts := strings.Split(cur.Content, ",")
	for _, p := range parts {
		if strings.TrimSpace(p) == contentType {
			return nil // already there, no-op
		}
	}
	parts = append(parts, contentType)
	for i, p := range parts {
		parts[i] = strings.TrimSpace(p)
	}

	params := url.Values{}
	params.Set("content", strings.Join(parts, ","))
	path := fmt.Sprintf("/storage/%s", url.PathEscape(storage))
	if err := c.do(ctx, http.MethodPut, path, params, nil); err != nil {
		return fmt.Errorf("update storage %s content types: %w", storage, err)
	}
	return nil
}

// StorageContentItem is one entry returned by GET /nodes/{n}/storage/{s}/content.
// We only care about the volid (the canonical "<storage>:<type>/<filename>"
// reference) for existence checks before downloading.
type StorageContentItem struct {
	Volid   string `json:"volid"`
	Format  string `json:"format"`
	Size    uint64 `json:"size"`
	Content string `json:"content"`
}

// StorageHasFile reports whether a given filename exists in a node's storage
// under the named content type. Used by bootstrap to skip downloads when the
// cloud image is already cached — Proxmox's download-url refuses to overwrite,
// so we check first.
//
// volid format used for matching is "<storage>:<contentType>/<filename>".
func (c *Client) StorageHasFile(
	ctx context.Context,
	node, storage, contentType, filename string,
) (bool, error) {
	var items []StorageContentItem
	params := url.Values{}
	params.Set("content", contentType)
	path := fmt.Sprintf("/nodes/%s/storage/%s/content",
		url.PathEscape(node), url.PathEscape(storage))
	if err := c.do(ctx, http.MethodGet, path, params, &items); err != nil {
		return false, err
	}
	want := fmt.Sprintf("%s:%s/%s", storage, contentType, filename)
	for _, it := range items {
		if it.Volid == want {
			return true, nil
		}
	}
	return false, nil
}

// DownloadStorageURL downloads a remote URL into a node-local storage as the
// given content type (typically "import" for cloud-image disks Nimbus will
// later use as VM-creation sources). Returns a task UPID for caller-side
// polling via WaitForTask.
//
// PVE 8+ feature. The download happens on the Proxmox node — Nimbus only
// dispatches the request, then polls for completion.
//
// The content parameter must match a content type the storage is configured
// to accept; see EnsureStorageContent to add types programmatically.
//
// NOTE: Proxmox refuses to overwrite an existing file ("refusing to override").
// Callers must check StorageHasFile first if they need idempotent behavior.
func (c *Client) DownloadStorageURL(
	ctx context.Context,
	node, storage, contentType, urlStr, filename string,
) (string, error) {
	params := url.Values{}
	params.Set("url", urlStr)
	params.Set("content", contentType)
	params.Set("filename", filename)
	// "verify-certificates" defaults to true in Proxmox; cloud-images.ubuntu.com
	// has a valid cert so we don't override.

	var taskID string
	path := fmt.Sprintf("/nodes/%s/storage/%s/download-url",
		url.PathEscape(node), url.PathEscape(storage))
	if err := c.do(ctx, http.MethodPost, path, params, &taskID); err != nil {
		return "", err
	}
	return taskID, nil
}

// CreateVMWithImport creates a new VM on a node whose primary disk is imported
// from a file already present on that node. Used by the bootstrap flow to turn
// a downloaded cloud image into a Proxmox VM (which we then convert to a
// template).
//
// The wire-level magic happens in the scsi0 parameter:
//
//	scsi0=<DiskStorage>:0,import-from=<absolute path on node>
//
// The literal "0" tells Proxmox to derive the disk size from the source image.
// The import-from path is the file path on the node — typically
// /var/lib/vz/template/iso/<filename> for downloads to "local" storage.
//
// Cloud images need a serial console to boot headless (the SerialOnly option
// adds serial0=socket + vga=serial0). The qemu-guest-agent flag enables agent
// support but does NOT install the agent inside the guest — that ships with
// the cloud image.
//
// Returns the task UPID for caller-side polling.
func (c *Client) CreateVMWithImport(
	ctx context.Context,
	node string, vmid int, opts CreateVMOpts,
) (string, error) {
	bridge := opts.Bridge
	if bridge == "" {
		bridge = "vmbr0"
	}
	osType := opts.OSType
	if osType == "" {
		osType = "l26"
	}
	memory := opts.Memory
	if memory == 0 {
		memory = 1024
	}
	cores := opts.Cores
	if cores == 0 {
		cores = 1
	}

	params := url.Values{}
	params.Set("vmid", strconv.Itoa(vmid))
	params.Set("name", opts.Name)
	params.Set("memory", strconv.Itoa(memory))
	params.Set("cores", strconv.Itoa(cores))
	params.Set("ostype", osType)
	params.Set("net0", fmt.Sprintf("virtio,bridge=%s", bridge))
	params.Set("scsihw", "virtio-scsi-pci")
	params.Set("scsi0", fmt.Sprintf("%s:0,import-from=%s", opts.DiskStorage, opts.ImagePath))
	params.Set("boot", "c")
	params.Set("bootdisk", "scsi0")
	if opts.SerialOnly {
		params.Set("serial0", "socket")
		params.Set("vga", "serial0")
	}
	if opts.AgentEnabled {
		params.Set("agent", "enabled=1")
	}

	var taskID string
	path := fmt.Sprintf("/nodes/%s/qemu", url.PathEscape(node))
	if err := c.do(ctx, http.MethodPost, path, params, &taskID); err != nil {
		return "", err
	}
	return taskID, nil
}

// SetCloudInitDrive attaches a cloud-init drive to an existing VM. This is
// required before the VM can accept cloud-init config (ciuser, sshkeys, etc.)
// at clone time — without the drive, cloud-init has nowhere to read its config
// and the SetCloudInit values are silently ignored at boot.
//
// `ide2=<storage>:cloudinit` is the canonical attachment form, matching what
// `qm set <vmid> --ide2 <storage>:cloudinit` produces.
func (c *Client) SetCloudInitDrive(
	ctx context.Context,
	node string, vmid int, storage string,
) error {
	params := url.Values{}
	params.Set("ide2", fmt.Sprintf("%s:cloudinit", storage))
	path := fmt.Sprintf("/nodes/%s/qemu/%d/config", url.PathEscape(node), vmid)
	return c.do(ctx, http.MethodPost, path, params, nil)
}

// ConvertToTemplate marks a VM as a Proxmox template. Templates are immutable
// — they cannot be booted, only cloned. This is the final step of bootstrap.
func (c *Client) ConvertToTemplate(ctx context.Context, node string, vmid int) error {
	path := fmt.Sprintf("/nodes/%s/qemu/%d/template", url.PathEscape(node), vmid)
	return c.do(ctx, http.MethodPost, path, url.Values{}, nil)
}

// Version returns the Proxmox VE version string. Used by /api/health.
func (c *Client) Version(ctx context.Context) (string, error) {
	var v struct {
		Version string `json:"version"`
		Release string `json:"release"`
	}
	if err := c.do(ctx, http.MethodGet, "/version", nil, &v); err != nil {
		return "", err
	}
	if v.Release != "" {
		return v.Version + "-" + v.Release, nil
	}
	return v.Version, nil
}

// ParseLXCNetIP extracts the bare IP from an LXC `netN` config value. The
// format is comma-separated key=value pairs much like ipconfig0 but with a
// different vocabulary, e.g.:
//
//	"name=eth0,bridge=vmbr0,gw=192.168.0.1,hwaddr=BC:24:11:00:00:00,ip=192.168.0.50/24,type=veth"
//
// `ip=dhcp` and `ip=manual` (LXC's "ifupdown handles it") are skipped — the
// container has no static claim on a specific address. Returns the bare IP
// (CIDR suffix stripped) when a static IPv4 / IPv6 is present.
func ParseLXCNetIP(s string) (string, bool) {
	if s == "" {
		return "", false
	}
	for _, part := range strings.Split(s, ",") {
		kv := strings.SplitN(strings.TrimSpace(part), "=", 2)
		if len(kv) != 2 || strings.TrimSpace(kv[0]) != "ip" {
			continue
		}
		v := strings.TrimSpace(kv[1])
		if v == "" || v == "dhcp" || v == "manual" || v == "auto" {
			return "", false
		}
		if slash := strings.Index(v, "/"); slash >= 0 {
			v = v[:slash]
		}
		if net.ParseIP(v) == nil {
			return "", false
		}
		return v, true
	}
	return "", false
}

// ParseIPConfig0 extracts the bare IP from a Proxmox cloud-init ipconfig0 value.
// The format is a comma-separated list of key=value pairs; we want the value of
// the `ip=` key, with any CIDR suffix stripped.
//
//	"ip=192.168.0.142/24,gw=192.168.0.1"  → "192.168.0.142", true
//	"ip=10.0.0.5"                          → "10.0.0.5",     true
//	"ip=dhcp,gw=auto"                      → "",             false  (skip dynamic)
//	"" or malformed                        → "",             false
//
// Both IPv4 and IPv6 are accepted. DHCP / "auto" / non-IP values are skipped so
// reconciliation does not pretend they claim a specific static IP.
func ParseIPConfig0(s string) (string, bool) {
	if s == "" {
		return "", false
	}
	for _, part := range strings.Split(s, ",") {
		kv := strings.SplitN(strings.TrimSpace(part), "=", 2)
		if len(kv) != 2 {
			continue
		}
		if strings.TrimSpace(kv[0]) != "ip" {
			continue
		}
		v := strings.TrimSpace(kv[1])
		if v == "" || v == "dhcp" || v == "auto" {
			return "", false
		}
		if slash := strings.Index(v, "/"); slash >= 0 {
			v = v[:slash]
		}
		if net.ParseIP(v) == nil {
			return "", false
		}
		return v, true
	}
	return "", false
}

// ListClusterIPs walks every online node, lists each node's QEMU VMs (templates
// excluded), reads each VM's config, and returns one ClusterIP per VM that has
// a parseable static ipconfig0 value.
//
// Per-node walks run concurrently up to listClusterIPsConcurrency. A failure on
// one node does not abort the whole walk — partial results are returned alongside
// a joined error so the caller can decide whether to use them. A failure to
// list nodes IS fatal because there is no partial truth without that list.
//
// VMs that vanish mid-walk (a destroy raced our config GET) are silently
// skipped; they cannot be claiming an IP if they no longer exist.
func (c *Client) ListClusterIPs(ctx context.Context) ([]ClusterIP, error) {
	nodes, err := c.GetNodes(ctx)
	if err != nil {
		return nil, fmt.Errorf("list nodes: %w", err)
	}

	var (
		mu   sync.Mutex
		out  []ClusterIP
		errs []error
		wg   sync.WaitGroup
		sem  = make(chan struct{}, listClusterIPsConcurrency)
	)

	for _, n := range nodes {
		if n.Status != "online" {
			continue
		}
		nodeName := n.Name
		wg.Add(1)
		sem <- struct{}{}
		go func() {
			defer wg.Done()
			defer func() { <-sem }()
			ips, err := c.collectNodeIPs(ctx, nodeName)
			mu.Lock()
			defer mu.Unlock()
			if err != nil {
				errs = append(errs, fmt.Errorf("node %s: %w", nodeName, err))
			}
			out = append(out, ips...)
		}()
	}
	wg.Wait()

	if len(errs) > 0 {
		return out, errors.Join(errs...)
	}
	return out, nil
}

// GetClusterVMDetails walks every online node and returns a per-VM snapshot
// enriched with IP (cloud-init `ipconfig0`, falling back to qemu-guest-agent),
// raw tags, and `ostype`. Templates are excluded.
//
// This is the data source for the admin cluster-VM view. It is intentionally
// separate from ListClusterIPs (which the IP-pool reconciler depends on) so
// failures in this richer walk can't poison the IP-allocation truth source.
//
// Per-node walks run concurrently up to listClusterIPsConcurrency. Per-VM
// agent probes are best-effort: a VM whose agent is disabled or unreachable
// produces an entry with IP="" rather than failing the walk.
func (c *Client) GetClusterVMDetails(ctx context.Context) ([]ClusterVMDetail, error) {
	nodes, err := c.GetNodes(ctx)
	if err != nil {
		return nil, fmt.Errorf("list nodes: %w", err)
	}

	var (
		mu   sync.Mutex
		out  []ClusterVMDetail
		errs []error
		wg   sync.WaitGroup
		sem  = make(chan struct{}, listClusterIPsConcurrency)
	)

	for _, n := range nodes {
		if n.Status != "online" {
			continue
		}
		nodeName := n.Name
		wg.Add(1)
		sem <- struct{}{}
		go func() {
			defer wg.Done()
			defer func() { <-sem }()
			details, err := c.collectNodeDetails(ctx, nodeName)
			mu.Lock()
			defer mu.Unlock()
			if err != nil {
				errs = append(errs, fmt.Errorf("node %s: %w", nodeName, err))
			}
			out = append(out, details...)
		}()
	}
	wg.Wait()

	if len(errs) > 0 {
		return out, errors.Join(errs...)
	}
	return out, nil
}

// collectNodeDetails enriches every non-template VM on one node with its
// config (ipconfig0, tags, ostype) and an agent IP fallback. A single bad VMID
// config does not drop the whole node — the failed entry is returned with
// whatever fields we managed to populate.
//
// Per-VM enrichment runs concurrently up to perNodeVMConcurrency. Each VM's
// work is independent (separate Proxmox endpoints), so serialising them was
// the dashboard's main 15s cold-cache bottleneck.
func (c *Client) collectNodeDetails(ctx context.Context, node string) ([]ClusterVMDetail, error) {
	vms, err := c.ListVMs(ctx, node)
	if err != nil {
		return nil, fmt.Errorf("list vms: %w", err)
	}

	results := make([]ClusterVMDetail, len(vms))
	keep := make([]bool, len(vms))

	var wg sync.WaitGroup
	sem := make(chan struct{}, perNodeVMConcurrency)
	for i, vm := range vms {
		if vm.Template != 0 {
			continue
		}
		i, vm := i, vm
		wg.Add(1)
		sem <- struct{}{}
		go func() {
			defer wg.Done()
			defer func() { <-sem }()
			detail, ok := c.enrichVMDetail(ctx, node, vm)
			if !ok {
				return
			}
			results[i] = detail
			keep[i] = true
		}()
	}
	wg.Wait()

	// Compact in original VM order so the result is deterministic across
	// runs (otherwise admins see the table reshuffle on every poll).
	out := make([]ClusterVMDetail, 0, len(vms))
	for i := range results {
		if keep[i] {
			out = append(out, results[i])
		}
	}
	return out, nil
}

// enrichVMDetail performs the per-VM enrichment work for collectNodeDetails:
// fetch config (tags / description / ostype / ipconfig0), fall back to the
// guest agent for IP discovery, and cache OS info. Returns (detail, true)
// when the VM should be included in the response; (_, false) when the VM
// has gone missing on the node since ListVMs returned (cluster race).
func (c *Client) enrichVMDetail(ctx context.Context, node string, vm VMStatus) (ClusterVMDetail, bool) {
	detail := ClusterVMDetail{
		VMID:   vm.VMID,
		Node:   node,
		Name:   vm.Name,
		Status: vm.Status,
	}
	cfg, err := c.GetVMConfig(ctx, node, vm.VMID)
	if err != nil {
		if errors.Is(err, ErrNotFound) {
			return detail, false
		}
		return detail, true
	}
	if raw, _ := cfg["tags"].(string); raw != "" {
		detail.Tags = SplitTags(raw)
	}
	if desc, _ := cfg["description"].(string); desc != "" {
		detail.Description = desc
	}
	if ostype, _ := cfg["ostype"].(string); ostype != "" {
		detail.OSType = ostype
	}
	if raw, _ := cfg["ipconfig0"].(string); raw != "" {
		if ip, ok := ParseIPConfig0(raw); ok {
			detail.IP = ip
			detail.IPSource = "ipconfig0"
		}
	}
	// Fall back to qemu-guest-agent when the VM is running and lacks a
	// parseable static IP (typical for externally-created VMs).
	if detail.IP == "" && vm.Status == "running" {
		if ip := c.firstAgentIPv4(ctx, node, vm.VMID); ip != "" {
			detail.IP = ip
			detail.IPSource = "agent"
		}
	}
	// Best-effort enriched OS info from the agent. Cached 24h on hit,
	// 5min on miss — see osInfoCache. Stopped VMs skip the probe.
	if vm.Status == "running" {
		detail.OS = c.GetAgentOSInfoCached(ctx, node, vm.VMID)
	}
	return detail, true
}

// firstAgentIPv4 returns the first non-loopback IPv4 reported by the guest
// agent, or "" on any error / no usable address. Best-effort — the agent
// returns 500 on VMs without it installed/running, which we silently skip.
func (c *Client) firstAgentIPv4(ctx context.Context, node string, vmid int) string {
	ifaces, err := c.GetAgentInterfaces(ctx, node, vmid)
	if err != nil {
		return ""
	}
	for _, iface := range ifaces {
		if iface.Name == "lo" {
			continue
		}
		for _, addr := range iface.IPAddresses {
			if addr.IPAddressType != "ipv4" {
				continue
			}
			if addr.IPAddress == "" || strings.HasPrefix(addr.IPAddress, "127.") {
				continue
			}
			return addr.IPAddress
		}
	}
	return ""
}

// collectNodeIPs lists VMs and LXC containers on a single node, parses each
// QEMU VM's ipconfig0 and each container's `netN` static IP, and returns
// the union. Returns whatever it managed to collect alongside any non-fatal
// errors so a single bad VMID config doesn't drop the whole node.
//
// LXC containers share the cluster-wide VMID namespace with QEMU VMs so the
// caller can treat the result as a flat per-IP slice.
func (c *Client) collectNodeIPs(ctx context.Context, node string) ([]ClusterIP, error) {
	vms, err := c.ListVMs(ctx, node)
	if err != nil {
		return nil, fmt.Errorf("list vms: %w", err)
	}
	out := make([]ClusterIP, 0, len(vms))
	for _, vm := range vms {
		if vm.Template != 0 {
			continue
		}
		cfg, err := c.GetVMConfig(ctx, node, vm.VMID)
		if err != nil {
			if errors.Is(err, ErrNotFound) {
				continue
			}
			return out, fmt.Errorf("get config vmid=%d: %w", vm.VMID, err)
		}
		raw, _ := cfg["ipconfig0"].(string)
		if raw == "" {
			continue
		}
		ip, ok := ParseIPConfig0(raw)
		if !ok {
			continue
		}
		out = append(out, ClusterIP{
			IP:        ip,
			VMID:      vm.VMID,
			Node:      node,
			Hostname:  vm.Name,
			Source:    "ipconfig0",
			RawConfig: raw,
		})
	}

	// LXC containers — same per-node walk but with the netN config parser.
	// LXC failures are logged via the returned error joined with VM errors;
	// a missing /lxc endpoint (older Proxmox?) is treated as "no containers"
	// rather than a hard failure.
	lxcs, err := c.ListLXCs(ctx, node)
	if err != nil {
		// Don't escalate — the caller still gets the QEMU IPs and the error
		// surfaces in the joined errors.Join chain.
		return out, fmt.Errorf("list lxc: %w", err)
	}
	for _, ct := range lxcs {
		if ct.Template != 0 {
			continue
		}
		cfg, err := c.GetLXCConfig(ctx, node, ct.VMID)
		if err != nil {
			if errors.Is(err, ErrNotFound) {
				continue
			}
			return out, fmt.Errorf("get lxc config vmid=%d: %w", ct.VMID, err)
		}
		// Walk net0..net9 — Proxmox supports up to 10 netN per container.
		for i := 0; i < 10; i++ {
			raw, _ := cfg[fmt.Sprintf("net%d", i)].(string)
			if raw == "" {
				continue
			}
			ip, ok := ParseLXCNetIP(raw)
			if !ok {
				continue
			}
			out = append(out, ClusterIP{
				IP:        ip,
				VMID:      ct.VMID,
				Node:      node,
				Hostname:  ct.Name,
				Source:    fmt.Sprintf("lxc-net%d", i),
				RawConfig: raw,
			})
		}
	}
	return out, nil
}
