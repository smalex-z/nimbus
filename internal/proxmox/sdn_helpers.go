package proxmox

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"strings"
)

// SDN-naming helpers.
//
// Proxmox zone IDs and VNet names share a hard cap: 1–8 characters,
// must start with a letter, lowercase alphanumeric only — no
// hyphens, dots, or underscores. The same constraints apply to
// VNet names (and Nimbus uses zone == vnet for both Standalone VMs
// and VPCs since each owns exactly one VNet in v1).
//
// The hashing scheme below maps a stable identifier (VM UUID, VPC
// UUID) to an 8-char Proxmox-safe name with a 1-letter prefix:
//
//   - Standalone VM zone/vnet: "s" + 7 hex chars from sha256(uuid)
//   - VPC zone/vnet:           "v" + 7 hex chars from sha256(uuid)
//
// 7 hex chars = 28 bits = 268M possible names. Birthday paradox at
// ~16K entries gives 50% collision odds — fine for a single
// cluster, but callers should still verify against the existing
// zone list before committing. See FormatStandaloneZoneName /
// FormatVPCZoneName for the constructors.

// FormatStandaloneZoneName produces the per-VM Simple-zone name
// from a VM identifier. Format: "s<7 hex>" (8 chars total).
//
// Caller must check for collisions via ListSDNZones — if the
// hash collides with an existing zone, retry with a different
// derivation (e.g. salt the hash with a counter).
func FormatStandaloneZoneName(vmIdentifier string) string {
	return formatZoneName('s', vmIdentifier)
}

// FormatVPCZoneName produces the per-VPC VXLAN-zone name from a
// VPC identifier. Format: "v<7 hex>" (8 chars total).
func FormatVPCZoneName(vpcIdentifier string) string {
	return formatZoneName('v', vpcIdentifier)
}

func formatZoneName(prefix byte, identifier string) string {
	sum := sha256.Sum256([]byte(identifier))
	// Lowercase hex of first 4 bytes = 8 hex chars; we keep 7 to
	// leave room for the 1-letter prefix within the 8-char cap.
	hexStr := hex.EncodeToString(sum[:4])[:7]
	return string(prefix) + hexStr
}

// ResolveOnlinePeerIPs lists every online cluster node's advertised
// IP, comma-joined for the VXLAN zone's `peers=` field. Offline
// nodes are skipped — including them in `peers` causes ifupdown2
// errors during ApplySDN. The returned string is empty when no
// nodes are online (caller should treat as an error).
//
// Used by both vnetmgr.Bootstrap (legacy single-zone path) and
// vpcmgr.CreateVPC (per-VPC path) so the peer-resolution behavior
// stays in lockstep across both code paths.
func ResolveOnlinePeerIPs(ctx context.Context, c *Client) (string, error) {
	entries, err := c.GetClusterStatus(ctx)
	if err != nil {
		return "", err
	}
	var ips []string
	for _, e := range entries {
		if e.Type != "node" || e.Online != 1 || e.IP == "" {
			continue
		}
		ips = append(ips, e.IP)
	}
	return strings.Join(ips, ","), nil
}

// ZoneExists checks PVE for a zone with the given name. Returns
// (true, nil) when the zone exists, (false, nil) when not found,
// and (false, err) for transport errors. Used by collision-retry
// loops in zone-name allocation.
func ZoneExists(ctx context.Context, c *Client, zone string) (bool, error) {
	_, err := c.GetSDNZone(ctx, zone)
	switch {
	case err == nil:
		return true, nil
	case errIsNotFound(err):
		return false, nil
	default:
		return false, err
	}
}

// errIsNotFound is a small wrapper around errors.Is(err, ErrNotFound)
// that avoids dragging the errors package import into every caller.
func errIsNotFound(err error) bool {
	if err == nil {
		return false
	}
	if err == ErrNotFound { //nolint:errorlint // ErrNotFound is a sentinel
		return true
	}
	// Be conservative: walk Unwrap chain for ErrNotFound.
	for {
		u, ok := err.(interface{ Unwrap() error })
		if !ok {
			return false
		}
		err = u.Unwrap()
		if err == nil {
			return false
		}
		if err == ErrNotFound { //nolint:errorlint
			return true
		}
	}
}

// FormatLXCNetSpec builds a Proxmox netN value for an LXC config —
// the comma-separated form Proxmox expects on
// `net0=name=eth0,bridge=vmbr0,ip=...,gw=...`.
//
// Empty fields are dropped: caller can pass "" for ip/gw to leave
// them unset (e.g. for DHCP), or "" for hwaddr to let Proxmox
// generate one.
type LXCNetSpec struct {
	Name   string // e.g. "eth0"
	Bridge string // e.g. "vmbr0" or a vnet name
	IP     string // CIDR form, e.g. "192.168.1.10/24" or "dhcp"
	Gw     string // gateway IP, blank if no default route on this NIC
	Hwaddr string // MAC; blank → Proxmox autogenerates
}

func (s LXCNetSpec) String() string {
	parts := []string{}
	if s.Name != "" {
		parts = append(parts, "name="+s.Name)
	}
	if s.Bridge != "" {
		parts = append(parts, "bridge="+s.Bridge)
	}
	if s.IP != "" {
		parts = append(parts, "ip="+s.IP)
	}
	if s.Gw != "" {
		parts = append(parts, "gw="+s.Gw)
	}
	if s.Hwaddr != "" {
		parts = append(parts, "hwaddr="+s.Hwaddr)
	}
	return joinComma(parts)
}

func joinComma(parts []string) string {
	out := ""
	for i, p := range parts {
		if i > 0 {
			out += ","
		}
		out += p
	}
	return out
}
