package proxmox

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
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
}

// SDNVNet is one VLAN-segment definition under /cluster/sdn/vnets.
// Tag is the VLAN ID for vlan zones / VNI for vxlan; ignored on simple.
type SDNVNet struct {
	VNet  string `json:"vnet"`
	Zone  string `json:"zone"`
	Tag   int    `json:"tag,omitempty"`
	Alias string `json:"alias,omitempty"`
}

// SDNSubnet is one CIDR + gateway definition attached to a VNet under
// /cluster/sdn/vnets/{vnet}/subnets. Subnet is the CIDR (e.g.
// "10.42.42.0/24") and is also the resource name in the URL path.
// SNAT=true on a simple zone enables outbound NAT through the host's
// upstream gateway — the cleanest way to give isolated VMs internet
// reachability without an external router.
type SDNSubnet struct {
	Subnet    string `json:"subnet"`
	VNet      string `json:"vnet"`
	Gateway   string `json:"gateway,omitempty"`
	SNAT      bool   `json:"snat,omitempty"`
	DNSServer string `json:"dnsserver,omitempty"`
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
func (c *Client) GetSDNZone(ctx context.Context, zone string) (*SDNZone, error) {
	var out SDNZone
	path := "/cluster/sdn/zones/" + url.PathEscape(zone)
	if err := c.do(ctx, http.MethodGet, path, nil, &out); err != nil {
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

// DeleteSDNSubnet removes a subnet from a VNet.
func (c *Client) DeleteSDNSubnet(ctx context.Context, vnet, subnet string) error {
	// Proxmox path encodes the subnet CIDR with the slash replaced by
	// a hyphen — the literal "10.42.42.0/24" becomes "10.42.42.0-24"
	// in the URL. Documented quirk; not URL escaping.
	encoded := pveEncodeSubnet(subnet)
	path := "/cluster/sdn/vnets/" + url.PathEscape(vnet) + "/subnets/" + encoded
	return c.do(ctx, http.MethodDelete, path, nil, nil)
}

// ApplySDN reloads pve-firewall + ifupdown2 across every cluster node,
// pushing pending zone/vnet/subnet definitions into the running config.
// Expensive (~1-2 s per node) — call after a batch of changes, not per
// individual create. Without this call, every Create/Delete above is
// invisible to running VMs.
func (c *Client) ApplySDN(ctx context.Context) error {
	return c.do(ctx, http.MethodPut, "/cluster/sdn", nil, nil)
}

// SetVMNetwork rewrites the bridge (and optionally MAC) on a VM's
// network device after clone. Used by Nimbus to override the inherited
// `bridge=vmbr0` from the template, pointing the VM at its owner's
// SDN VNet instead.
//
// Pass empty mac to leave the cloned MAC alone; pass "auto" to ask
// Proxmox to assign a fresh one (avoids same-MAC-across-isolated-VLANs
// surprises during debug — they're not L2-colliding because the VLANs
// are isolated, but human eyes assume otherwise).
func (c *Client) SetVMNetwork(ctx context.Context, node string, vmid int, dev, bridge, mac string) error {
	if dev == "" {
		dev = "net0"
	}
	if bridge == "" {
		return errors.New("bridge is required")
	}
	model := "virtio"
	val := fmt.Sprintf("%s,bridge=%s", model, bridge)
	if mac != "" {
		val = fmt.Sprintf("%s=%s,bridge=%s", model, mac, bridge)
	}
	params := url.Values{}
	params.Set(dev, val)
	path := fmt.Sprintf("/nodes/%s/qemu/%d/config", url.PathEscape(node), vmid)
	return c.do(ctx, http.MethodPost, path, params, nil)
}

// pveEncodeSubnet rewrites a CIDR to Proxmox's URL-path encoding for
// SDN subnet IDs: the slash before the prefix length becomes a hyphen.
// "10.42.42.0/24" → "10.42.42.0-24". Returns the input unchanged if no
// slash is present (defensive — never observed in practice).
func pveEncodeSubnet(cidr string) string {
	for i := len(cidr) - 1; i >= 0; i-- {
		if cidr[i] == '/' {
			return cidr[:i] + "-" + cidr[i+1:]
		}
	}
	return cidr
}
