package proxmox

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// Proxmox SDN endpoints surface as `/cluster/sdn/{zones,vnets,subnets}` —
// staged-then-applied. Each create/delete writes pending state; nothing
// reaches the running config until ApplySDN (PUT /cluster/sdn) reloads
// pve-firewall + ifupdown2 across every cluster node. That two-phase
// commit isn't a Nimbus invention — it's how Proxmox's SDN system is
// designed — but it means callers must always invoke ApplySDN after
// any Create/Delete or the changes are invisible to running VMs.
//
// Zone types and their tradeoffs (relevant to Nimbus's per-user VNet
// design):
//
//   - simple: per-host bridge with NAT to the upstream gateway. No
//     switch / VLAN config required, no cross-node L2. Single-host or
//     "L3-routed cluster" deployments. Default for Nimbus.
//   - vlan:   real VLAN tag on a trunk interface — needs the switch
//     configured to trunk the tag. Cross-node L2 works. Operator
//     overhead.
//   - vxlan:  software-defined cross-node L2. Requires inter-node UDP
//     4789, an MTU 50 bytes lower than the underlay, and an explicit
//     `exitnodes=...` setting if outbound NAT is needed.
//   - qinq:   double-tagged VLAN; not relevant to Nimbus.
//
// The Proxmox VNet *name* has a hard 8-character limit, must start with
// a letter, lowercase alphanumeric only. Validate at the caller.

// SDNZone is one zone definition under /cluster/sdn/zones. The wire
// shape varies by Type — VLAN zones need Bridge, VXLAN zones need
// Peers, simple zones need neither — so the unused fields are tagged
// omitempty and only sent when populated.
type SDNZone struct {
	Zone   string `json:"zone"`
	Type   string `json:"type"`             // simple | vlan | vxlan | qinq
	IPAM   string `json:"ipam,omitempty"`   // optional IPAM plugin
	Peers  string `json:"peers,omitempty"`  // VXLAN: comma-sep node IPs
	Bridge string `json:"bridge,omitempty"` // VLAN: underlying bridge
	// Nodes restricts the zone to a subset of cluster members. Empty
	// means "all nodes". We don't set this from Nimbus today — leaving
	// the zone cluster-wide matches the per-host-bridge auto-create
	// behavior of simple zones.
	Nodes string `json:"nodes,omitempty"`
	// Exitnodes designates which node(s) handle outbound NAT for the
	// zone. Required for VXLAN zones that need internet egress (without
	// it, VMs can talk to each other across nodes but can't reach the
	// outside world). Comma-separated PVE node hostnames.
	Exitnodes string `json:"exitnodes,omitempty"`
}

// SDNVNet is one VLAN-segment definition under /cluster/sdn/vnets.
// Tag is the VLAN ID for vlan zones / VNI for vxlan; ignored on simple.
type SDNVNet struct {
	VNet  string `json:"vnet"`
	Zone  string `json:"zone"`
	Tag   int    `json:"tag,omitempty"`
	Alias string `json:"alias,omitempty"`
}

// SDNSubnet is one CIDR + gateway definition attached to a VNet
// under /cluster/sdn/vnets/{vnet}/subnets. Subnet is the CIDR (e.g.
// "10.42.42.0/24"); ID is the PVE-internal identifier used for
// DELETE — the format is "<zone>-<ip>-<prefix>" (e.g.
// "nimbus-10.42.0.0-24"), populated by ListSDNSubnets and computable
// from FormatSDNSubnetID. SNAT=true enables iptables MASQUERADE on
// the bridge — gives VMs outbound internet without an external
// router (works for both simple AND vxlan zones, despite docs that
// only mention simple).
type SDNSubnet struct {
	// ID is PVE's full subnet identifier ("<zone>-<ip>-<prefix>").
	// Populated by ListSDNSubnets; ignored by CreateSDNSubnet (PVE
	// derives the ID from zone+cidr at create time).
	ID        string `json:"id,omitempty"`
	Subnet    string `json:"subnet"`
	VNet      string `json:"vnet"`
	Gateway   string `json:"gateway,omitempty"`
	SNAT      bool   `json:"snat,omitempty"`
	DNSServer string `json:"dnsserver,omitempty"`
}

// FormatSDNSubnetID computes the PVE subnet identifier for use in
// the DELETE URL. Format: "<zone>-<ip>-<prefix>". Example:
// FormatSDNSubnetID("nimbus", "10.42.0.0/24") → "nimbus-10.42.0.0-24".
//
// PVE rejects other forms with cryptic errors (raw CIDR → "invalid
// format - value does not look like a valid CIDR network"; raw
// dash-form without zone prefix → same; URL-escaped slash → 501
// because Mojolicious decodes %2F to / and the path-component count
// goes wrong). The zone-prefixed dash form is what PVE actually
// uses internally and what its routes match against.
func FormatSDNSubnetID(zone, cidr string) string {
	dashed := cidr
	if i := strings.LastIndex(cidr, "/"); i >= 0 {
		dashed = cidr[:i] + "-" + cidr[i+1:]
	}
	return zone + "-" + dashed
}

// ListSDNZones returns every zone Proxmox knows about (running + pending).
func (c *Client) ListSDNZones(ctx context.Context) ([]SDNZone, error) {
	var out []SDNZone
	if err := c.do(ctx, http.MethodGet, "/cluster/sdn/zones", nil, &out); err != nil {
		return nil, err
	}
	return out, nil
}

// GetSDNZone fetches one zone by name. Returns ErrNotFound when the
// zone doesn't exist; callers can errors.Is-test for "needs creating".
//
// Proxmox returns 500 (not 404) for missing SDN zones with the body
// `{"data":null,"message":"sdn 'X' does not exist\n"}` — same quirk
// GetVMConfig + GetLXCConfig already normalize. Map both 404 and the
// 500-with-"does not exist" body to ErrNotFound so callers don't have
// to know about the inconsistency.
func (c *Client) GetSDNZone(ctx context.Context, zone string) (*SDNZone, error) {
	var out SDNZone
	path := "/cluster/sdn/zones/" + url.PathEscape(zone)
	if err := c.do(ctx, http.MethodGet, path, nil, &out); err != nil {
		if errors.Is(err, ErrNotFound) {
			return nil, ErrNotFound
		}
		var httpErr *HTTPError
		if errors.As(err, &httpErr) && strings.Contains(httpErr.Body, "does not exist") {
			return nil, ErrNotFound
		}
		return nil, err
	}
	return &out, nil
}

// CreateSDNZone creates a zone in pending state. Caller must ApplySDN
// before VMs can attach to VNets in this zone.
func (c *Client) CreateSDNZone(ctx context.Context, z SDNZone) error {
	params := url.Values{}
	params.Set("zone", z.Zone)
	params.Set("type", z.Type)
	if z.IPAM != "" {
		params.Set("ipam", z.IPAM)
	}
	if z.Peers != "" {
		params.Set("peers", z.Peers)
	}
	if z.Bridge != "" {
		params.Set("bridge", z.Bridge)
	}
	if z.Nodes != "" {
		params.Set("nodes", z.Nodes)
	}
	if z.Exitnodes != "" {
		params.Set("exitnodes", z.Exitnodes)
	}
	return c.do(ctx, http.MethodPost, "/cluster/sdn/zones", params, nil)
}

// DeleteSDNZone removes a zone. Proxmox refuses if any VNets still
// reference the zone — callers must delete VNets first.
func (c *Client) DeleteSDNZone(ctx context.Context, zone string) error {
	path := "/cluster/sdn/zones/" + url.PathEscape(zone)
	return c.do(ctx, http.MethodDelete, path, nil, nil)
}

// ListSDNVNets returns every VNet across every zone.
func (c *Client) ListSDNVNets(ctx context.Context) ([]SDNVNet, error) {
	var out []SDNVNet
	if err := c.do(ctx, http.MethodGet, "/cluster/sdn/vnets", nil, &out); err != nil {
		return nil, err
	}
	return out, nil
}

// CreateSDNVNet creates a VNet in pending state. The Tag field is
// required for vlan/vxlan zones; ignored on simple.
func (c *Client) CreateSDNVNet(ctx context.Context, v SDNVNet) error {
	params := url.Values{}
	params.Set("vnet", v.VNet)
	params.Set("zone", v.Zone)
	if v.Tag != 0 {
		params.Set("tag", strconv.Itoa(v.Tag))
	}
	if v.Alias != "" {
		params.Set("alias", v.Alias)
	}
	return c.do(ctx, http.MethodPost, "/cluster/sdn/vnets", params, nil)
}

// DeleteSDNVNet removes a VNet. Proxmox refuses if any subnets still
// reference the VNet — callers must delete subnets first.
func (c *Client) DeleteSDNVNet(ctx context.Context, vnet string) error {
	path := "/cluster/sdn/vnets/" + url.PathEscape(vnet)
	return c.do(ctx, http.MethodDelete, path, nil, nil)
}

// ListSDNSubnets returns every subnet attached to one VNet. Used by
// Reset to enumerate orphan subnets that aren't tracked in Nimbus's
// DB — those still need to be torn down so the vnet (and then the
// zone) can be deleted cleanly.
//
// Proxmox returns each row with its `cidr` field set to the
// slash-form CIDR; Subnet is populated from that for caller use.
func (c *Client) ListSDNSubnets(ctx context.Context, vnet string) ([]SDNSubnet, error) {
	type sdnSubnetRow struct {
		ID      string `json:"id"`      // dash-encoded ID, e.g. "10.42.0.0-24"
		CIDR    string `json:"cidr"`    // slash-form, e.g. "10.42.0.0/24"
		Gateway string `json:"gateway"` // optional
		VNet    string `json:"vnet"`
	}
	var rows []sdnSubnetRow
	path := "/cluster/sdn/vnets/" + url.PathEscape(vnet) + "/subnets"
	if err := c.do(ctx, http.MethodGet, path, nil, &rows); err != nil {
		return nil, err
	}
	out := make([]SDNSubnet, 0, len(rows))
	for _, r := range rows {
		out = append(out, SDNSubnet{
			ID:      r.ID,
			Subnet:  r.CIDR,
			VNet:    r.VNet,
			Gateway: r.Gateway,
		})
	}
	return out, nil
}

// CreateSDNSubnet attaches a subnet (CIDR + gateway) to an existing
// VNet. SNAT=true on a simple zone enables outbound NAT, which is what
// gives isolated Nimbus VMs internet reachability.
func (c *Client) CreateSDNSubnet(ctx context.Context, s SDNSubnet) error {
	params := url.Values{}
	params.Set("subnet", s.Subnet)
	params.Set("type", "subnet")
	if s.Gateway != "" {
		params.Set("gateway", s.Gateway)
	}
	if s.SNAT {
		params.Set("snat", "1")
	}
	if s.DNSServer != "" {
		params.Set("dnsserver", s.DNSServer)
	}
	path := "/cluster/sdn/vnets/" + url.PathEscape(s.VNet) + "/subnets"
	return c.do(ctx, http.MethodPost, path, params, nil)
}

// DeleteSDNSubnet removes a subnet from a VNet by its PVE subnet ID.
//
// The ID format is "<zone>-<ip>-<prefix>" (e.g.
// "nimbus-10.42.0.0-24"). Use FormatSDNSubnetID to compute it from
// (zone, cidr); when iterating ListSDNSubnets, use each row's ID
// field directly.
//
// Other formats fail in confusing ways: raw CIDR `10.42.0.0/24`
// returns 501 (Mojolicious decodes %2F → /, path-component count
// goes wrong); raw dash-form without zone `10.42.0.0-24` returns
// 400 ("invalid format - value does not look like a valid CIDR
// network"). Only the zone-prefixed dash form works.
func (c *Client) DeleteSDNSubnet(ctx context.Context, vnet, subnetID string) error {
	path := "/cluster/sdn/vnets/" + url.PathEscape(vnet) + "/subnets/" + url.PathEscape(subnetID)
	return c.do(ctx, http.MethodDelete, path, nil, nil)
}

// ApplySDN reloads pve-firewall + ifupdown2 across every cluster node,
// pushing pending zone/vnet/subnet definitions into the running config.
// Expensive (~1-2 s per node) — call after a batch of changes, not per
// individual create. Without this call, every Create/Delete above is
// invisible to running VMs.
//
// IMPORTANT: ApplySDN commits /etc/pve/sdn/* and writes
// /etc/network/interfaces.d/sdn on every node, but it does NOT reliably
// trigger `ifreload -a` on freshly-added cluster nodes — pveproxy on
// those nodes may not have re-resolved the cluster topology yet, so
// the file lands but the bridge never comes up. Callers that
// provisioned a per-node bridge (Standalone) or a per-cluster overlay
// (VPC) should follow up with ReloadNodeNetwork on the affected
// node(s) to force the local reload.
func (c *Client) ApplySDN(ctx context.Context) error {
	return c.do(ctx, http.MethodPut, "/cluster/sdn", nil, nil)
}

// ReloadNodeNetwork triggers `ifreload -a` on a single node via PVE's
// PUT /nodes/{node}/network endpoint. Mirrors what the web UI's
// "Apply" button does on the per-node Network tab — the missing piece
// after ApplySDN when a fresh cluster member's per-node reload hasn't
// fired yet.
//
// PVE returns a UPID for the reload worker, NOT a synchronous result
// — so callers (e.g. gateway.Provision before StartLXC) need the
// reload to be FINISHED, not just dispatched. We block on the UPID
// here so the function returning means the bridge is actually up.
//
// Idempotent at the network layer (ifreload over an already-up
// bridge is a cheap no-op).
func (c *Client) ReloadNodeNetwork(ctx context.Context, node string) error {
	if node == "" {
		return errors.New("reload node network: node required")
	}
	var taskID string
	path := fmt.Sprintf("/nodes/%s/network", url.PathEscape(node))
	if err := c.do(ctx, http.MethodPut, path, nil, &taskID); err != nil {
		return err
	}
	if taskID == "" {
		// Older PVE responded synchronously — nothing to wait for.
		return nil
	}
	return c.WaitForTask(ctx, node, taskID, 500*time.Millisecond)
}

// SetVMNetwork rewrites the bridge on a VM's network device after
// clone. Used by Nimbus to override the inherited `bridge=vmbr0` from
// the template, pointing the VM at its owner's SDN VNet instead.
//
// The MAC is intentionally omitted from the wire value: Proxmox
// auto-generates a fresh MAC when `net0=` is rewritten without a
// `macaddr=...` component, which avoids same-MAC-across-isolated-VLANs
// surprises during debug. (No L2 collision because the VLANs are
// isolated, but human eyes assume otherwise.)
func (c *Client) SetVMNetwork(ctx context.Context, node string, vmid int, dev, bridge string) error {
	if dev == "" {
		dev = "net0"
	}
	if bridge == "" {
		return errors.New("bridge is required")
	}
	params := url.Values{}
	params.Set(dev, fmt.Sprintf("virtio,bridge=%s", bridge))
	path := fmt.Sprintf("/nodes/%s/qemu/%d/config", url.PathEscape(node), vmid)
	return c.do(ctx, http.MethodPost, path, params, nil)
}
