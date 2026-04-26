package proxmox

import (
	"fmt"
	"regexp"
	"strings"
)

// Nimbus marker scheme.
//
// Provisioned VMs carry a single Proxmox tag — NimbusMarkerTag — and stash
// their tier/OS metadata in the VM's `description` field as a hidden HTML
// comment. One tag means one chip in the Proxmox UI list, which keeps the
// dashboard uncluttered; the description marker is invisible when the field
// is rendered as markdown but readable verbatim when an admin edits it.
//
// Older builds used three tags (`nimbus`, `nimbus-tier-*`, `nimbus-os-*`).
// Those legacy tags are still parsed (for VMs that haven't been backfilled
// yet) and stripped when this instance migrates them.
const (
	NimbusMarkerTag = "nimbus"

	// Legacy tag prefixes — kept so MergeNimbusTags can strip them during
	// migration and ParseNimbusTags can fall back to them when the VM's
	// description hasn't been written yet.
	legacyNimbusTierPrefix = "nimbus-tier-"
	legacyNimbusOSPrefix   = "nimbus-os-"
)

// nimbusDescRE matches the structured marker line Nimbus stamps into a VM's
// description. The `(?s)` flag is intentionally absent — the marker stays on
// one line so it doesn't accidentally swallow user prose between two `-->`s.
var nimbusDescRE = regexp.MustCompile(`<!--\s*nimbus:\s*([^>\n]*?)\s*-->`)

// EncodeNimbusTags returns the marker tag set Nimbus stamps onto a VM. The
// tier and OS now live in the description; a single tag is enough.
func EncodeNimbusTags() []string {
	return []string{NimbusMarkerTag}
}

// MergeNimbusTags merges the Nimbus marker into existing into a deduped slice.
// User tags are preserved verbatim; any legacy `nimbus-tier-*` / `nimbus-os-*`
// tags are dropped so the migrated VM ends up with a single `nimbus` chip in
// the Proxmox UI.
func MergeNimbusTags(existing []string) []string {
	out := make([]string, 0, len(existing)+1)
	seen := make(map[string]bool, len(existing)+1)
	for _, t := range existing {
		t = strings.TrimSpace(t)
		if t == "" {
			continue
		}
		if t == NimbusMarkerTag ||
			strings.HasPrefix(t, legacyNimbusTierPrefix) ||
			strings.HasPrefix(t, legacyNimbusOSPrefix) {
			continue
		}
		if seen[t] {
			continue
		}
		seen[t] = true
		out = append(out, t)
	}
	if !seen[NimbusMarkerTag] {
		out = append(out, NimbusMarkerTag)
	}
	return out
}

// HasNimbusTag reports whether the bare nimbus marker is present.
func HasNimbusTag(tags []string) bool {
	for _, t := range tags {
		if t == NimbusMarkerTag {
			return true
		}
	}
	return false
}

// ParseNimbusTags pulls tier/OS out of legacy `nimbus-tier-*` / `nimbus-os-*`
// tags. Returns isNimbus=true when the bare marker is present (the metadata
// fields may still be empty for VMs that have only been freshly tagged but
// not yet backfilled into the description).
//
// The description is the authoritative source on migrated VMs; this function
// stays as a fallback for foreign-Nimbus VMs whose owning instance hasn't
// been upgraded yet.
func ParseNimbusTags(tags []string) (tier, osTemplate string, isNimbus bool) {
	for _, t := range tags {
		switch {
		case t == NimbusMarkerTag:
			isNimbus = true
		case strings.HasPrefix(t, legacyNimbusTierPrefix):
			tier = strings.TrimPrefix(t, legacyNimbusTierPrefix)
		case strings.HasPrefix(t, legacyNimbusOSPrefix):
			osTemplate = decodeOSTag(strings.TrimPrefix(t, legacyNimbusOSPrefix))
		}
	}
	return tier, osTemplate, isNimbus
}

// EncodeNimbusDescription renders the metadata marker that goes inside the
// VM's description field. Format: `<!-- nimbus: tier=X os=Y -->` — an HTML
// comment that markdown renderers hide.
func EncodeNimbusDescription(tier, osTemplate string) string {
	return fmt.Sprintf("<!-- nimbus: tier=%s os=%s -->", tier, osTemplate)
}

// ParseNimbusDescription extracts the tier and OS from a Nimbus marker line
// inside a VM description. ok=false when the marker isn't present.
func ParseNimbusDescription(desc string) (tier, osTemplate string, ok bool) {
	m := nimbusDescRE.FindStringSubmatch(desc)
	if m == nil {
		return "", "", false
	}
	for _, field := range strings.Fields(m[1]) {
		key, val, found := strings.Cut(field, "=")
		if !found {
			continue
		}
		switch key {
		case "tier":
			tier = val
		case "os":
			osTemplate = val
		}
	}
	return tier, osTemplate, true
}

// MergeNimbusDescription returns the new description body after stamping (or
// updating) the Nimbus marker line. User-written prose is preserved; only
// the marker line itself is rewritten.
func MergeNimbusDescription(existing, tier, osTemplate string) string {
	marker := EncodeNimbusDescription(tier, osTemplate)
	if nimbusDescRE.MatchString(existing) {
		return nimbusDescRE.ReplaceAllString(existing, marker)
	}
	if existing == "" {
		return marker
	}
	return strings.TrimRight(existing, "\n") + "\n\n" + marker
}

// SplitTags parses Proxmox's tag string ("a;b;c" or "a,b,c") into a slice.
// Whitespace and empties are dropped.
func SplitTags(raw string) []string {
	if raw == "" {
		return nil
	}
	// Proxmox accepts ; , and space as separators; normalize all to ;.
	r := strings.NewReplacer(",", ";", " ", ";").Replace(raw)
	parts := strings.Split(r, ";")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}

// JoinTags renders a tag slice back to Proxmox's `;`-separated wire format.
func JoinTags(tags []string) string {
	return strings.Join(tags, ";")
}

// decodeOSTag reverses the legacy `.` → `_` substitution used when the OS
// template lived in a tag (where `.` is invalid). Only used to read pre-
// migration tags; new VMs encode the OS in the description verbatim.
func decodeOSTag(encoded string) string {
	return strings.ReplaceAll(encoded, "_", ".")
}
