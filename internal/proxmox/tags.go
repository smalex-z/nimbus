package proxmox

import (
	"fmt"
	"regexp"
	"strings"
)

// Nimbus marker scheme.
//
// Provisioned VMs carry two tags — NimbusMarkerTag for "this VM is Nimbus-
// managed" and `nimbus-id-<short>` for visual disambiguation in the PVE list.
// Full identity (tier, OS, full UUIDv7) lives in the VM `description` as a
// hidden HTML comment so the PVE dashboard isn't flooded with chips.
//
// The short tag is 8 hex chars from the front of the UUIDv7 — long enough to
// be uniquely identifying in a small cluster, short enough to render on one
// chip. The full UUID is the only thing the reconciler joins on.
//
// Older builds used three tags (`nimbus`, `nimbus-tier-*`, `nimbus-os-*`).
// Those legacy tags are still parsed (for VMs that haven't been backfilled
// yet) and stripped when this instance migrates them.
const (
	NimbusMarkerTag = "nimbus"

	// NimbusIDTagPrefix is the prefix for the short-form identity tag.
	// The full tag is `nimbus-id-<short>` where short is ShortNimbusID(uuid).
	NimbusIDTagPrefix = "nimbus-id-"

	// Legacy tag prefixes — kept so MergeNimbusTags can strip them during
	// migration and ParseNimbusTags can fall back to them when the VM's
	// description hasn't been written yet.
	legacyNimbusTierPrefix = "nimbus-tier-"
	legacyNimbusOSPrefix   = "nimbus-os-"

	// shortIDLen is the number of hex chars from the front of the UUID we
	// stamp into the tag. 8 hex = 32 bits of entropy, more than enough to
	// uniquely identify a VM in any realistic cluster.
	shortIDLen = 8
)

// nimbusDescRE matches the structured marker line Nimbus stamps into a VM's
// description. The `(?s)` flag is intentionally absent — the marker stays on
// one line so it doesn't accidentally swallow user prose between two `-->`s.
var nimbusDescRE = regexp.MustCompile(`<!--\s*nimbus:\s*([^>\n]*?)\s*-->`)

// ShortNimbusID returns the leading 8 hex chars of a UUIDv7 (hyphens stripped).
// Returns the empty string when the input is too short to extract from. Used
// for the short-form `nimbus-id-<short>` tag.
func ShortNimbusID(nimbusID string) string {
	stripped := strings.ReplaceAll(nimbusID, "-", "")
	if len(stripped) < shortIDLen {
		return ""
	}
	return stripped[:shortIDLen]
}

// EncodeNimbusTags returns the marker tag set Nimbus stamps onto a VM. The
// short-form identity tag is appended when nimbusID is non-empty — pass "" on
// legacy callers that haven't generated a UUID yet (they'll still get the
// bare marker so `HasNimbusTag` works).
func EncodeNimbusTags(nimbusID string) []string {
	tags := []string{NimbusMarkerTag}
	if short := ShortNimbusID(nimbusID); short != "" {
		tags = append(tags, NimbusIDTagPrefix+short)
	}
	return tags
}

// MergeNimbusTags merges the Nimbus marker set into existing into a deduped
// slice. User tags are preserved verbatim; any legacy `nimbus-tier-*` /
// `nimbus-os-*` tags AND any stale `nimbus-id-*` tag (older short id) are
// dropped so the result carries exactly the current canonical set.
func MergeNimbusTags(existing []string, nimbusID string) []string {
	out := make([]string, 0, len(existing)+2)
	seen := make(map[string]bool, len(existing)+2)
	for _, t := range existing {
		t = strings.TrimSpace(t)
		if t == "" {
			continue
		}
		if t == NimbusMarkerTag ||
			strings.HasPrefix(t, legacyNimbusTierPrefix) ||
			strings.HasPrefix(t, legacyNimbusOSPrefix) ||
			strings.HasPrefix(t, NimbusIDTagPrefix) {
			continue
		}
		if seen[t] {
			continue
		}
		seen[t] = true
		out = append(out, t)
	}
	for _, t := range EncodeNimbusTags(nimbusID) {
		if !seen[t] {
			seen[t] = true
			out = append(out, t)
		}
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

// ParseNimbusIDFromTags returns the short ID from the first `nimbus-id-<short>`
// tag, or "" if none is present. Useful for backfill: a short ID in the tags
// without a description marker means we can identify the VM as ours but the
// full UUID lives elsewhere (description, /etc/nimbus-id, smbios).
func ParseNimbusIDFromTags(tags []string) string {
	for _, t := range tags {
		if s := strings.TrimPrefix(t, NimbusIDTagPrefix); s != t {
			return s
		}
	}
	return ""
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
// VM's description field. Format:
//
//	<!-- nimbus: tier=X os=Y id=<full-uuid> -->
//
// The `id` field is omitted when nimbusID is empty — keeps the marker shape
// backward-compatible with pre-#297 callers and parsers.
func EncodeNimbusDescription(tier, osTemplate, nimbusID string) string {
	if nimbusID == "" {
		return fmt.Sprintf("<!-- nimbus: tier=%s os=%s -->", tier, osTemplate)
	}
	return fmt.Sprintf("<!-- nimbus: tier=%s os=%s id=%s -->", tier, osTemplate, nimbusID)
}

// ParseNimbusDescription extracts tier, OS, and the full nimbus_id UUID from
// a description marker. ok=false when the marker isn't present. Any of the
// returned strings may be empty for partial markers (e.g. a legacy VM whose
// description was stamped before `id=` was added).
func ParseNimbusDescription(desc string) (tier, osTemplate, nimbusID string, ok bool) {
	m := nimbusDescRE.FindStringSubmatch(desc)
	if m == nil {
		return "", "", "", false
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
		case "id":
			nimbusID = val
		}
	}
	return tier, osTemplate, nimbusID, true
}

// MergeNimbusDescription returns the new description body after stamping (or
// updating) the Nimbus marker line. User-written prose is preserved; only
// the marker line itself is rewritten.
func MergeNimbusDescription(existing, tier, osTemplate, nimbusID string) string {
	marker := EncodeNimbusDescription(tier, osTemplate, nimbusID)
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

// ParseSMBIOSUUID extracts the `uuid=...` value from a VM's `smbios1` config
// string (e.g. `uuid=01234567-89ab-cdef-0123-456789abcdef,family=...`).
// Returns "" when the field is missing or malformed. Used by the post-clone
// capture step in provision and by the backfill path for legacy VMs.
func ParseSMBIOSUUID(smbios1 string) string {
	for _, field := range strings.Split(smbios1, ",") {
		key, val, found := strings.Cut(strings.TrimSpace(field), "=")
		if !found {
			continue
		}
		if key == "uuid" {
			return val
		}
	}
	return ""
}

// decodeOSTag reverses the legacy `.` → `_` substitution used when the OS
// template lived in a tag (where `.` is invalid). Only used to read pre-
// migration tags; new VMs encode the OS in the description verbatim.
func decodeOSTag(encoded string) string {
	return strings.ReplaceAll(encoded, "_", ".")
}
