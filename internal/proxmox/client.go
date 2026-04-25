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

// Client is a Proxmox API client bound to one cluster endpoint and one token.
// Safe for concurrent use.
type Client struct {
	host    string // e.g. https://hppve.uclaacm.com:8006
	authHdr string // "PVEAPIToken=user@realm!tokenname=secret"
	http    *http.Client
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

// NextVMID asks Proxmox for the next free cluster-wide VMID.
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
	params.Set("pool", "nimbus")

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

// SetCloudInit applies cloud-init config to a VM. The sshkeys field is
// URL-encoded automatically (handled by url.Values.Encode in c.do).
func (c *Client) SetCloudInit(ctx context.Context, node string, vmid int, cfg CloudInitConfig) error {
	params := url.Values{}
	if cfg.CIUser != "" {
		params.Set("ciuser", cfg.CIUser)
	}
	if cfg.SSHKeys != "" {
		params.Set("sshkeys", cfg.SSHKeys)
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

	path := fmt.Sprintf("/nodes/%s/qemu/%d/config", url.PathEscape(node), vmid)
	return c.do(ctx, http.MethodPost, path, params, nil)
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

// collectNodeIPs lists VMs on a single node and parses each one's ipconfig0.
// Returns whatever it managed to collect alongside any non-fatal errors so that
// a single bad VMID config doesn't drop the whole node.
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
	return out, nil
}
