package proxmox

import "strings"

// Nimbus tag scheme. Every Nimbus-provisioned VM carries:
//   - NimbusMarkerTag — bare "nimbus" marker for cross-instance recognition
//   - "nimbus-tier-<tier>"     — small / medium / large / xl
//   - "nimbus-os-<encoded-os>" — ubuntu-22_04, debian-12, …
//
// OS template names contain '.' which Proxmox's default tag validator rejects;
// EncodeOSTag swaps '.' for '_' on the way out and DecodeOSTag swaps it back.
const (
	NimbusMarkerTag  = "nimbus"
	nimbusTierPrefix = "nimbus-tier-"
	nimbusOSPrefix   = "nimbus-os-"
)

// EncodeNimbusTags returns the marker tags Nimbus stamps onto a VM it
// provisions. Callers that already have user-applied tags should merge these
// in (see MergeNimbusTags) instead of overwriting.
func EncodeNimbusTags(tier, osTemplate string) []string {
	return []string{
		NimbusMarkerTag,
		nimbusTierPrefix + tier,
		nimbusOSPrefix + encodeOSTag(osTemplate),
	}
}

// MergeNimbusTags merges the Nimbus marker tags into existing into a deduped
// slice. Existing user tags are preserved verbatim; any prior nimbus-* tags
// are dropped so a tier change (e.g. small → medium) doesn't leave the old
// marker behind.
func MergeNimbusTags(existing []string, tier, osTemplate string) []string {
	out := make([]string, 0, len(existing)+3)
	seen := make(map[string]bool, len(existing)+3)
	for _, t := range existing {
		t = strings.TrimSpace(t)
		if t == "" {
			continue
		}
		if t == NimbusMarkerTag || strings.HasPrefix(t, nimbusTierPrefix) || strings.HasPrefix(t, nimbusOSPrefix) {
			continue
		}
		if seen[t] {
			continue
		}
		seen[t] = true
		out = append(out, t)
	}
	for _, t := range EncodeNimbusTags(tier, osTemplate) {
		if !seen[t] {
			seen[t] = true
			out = append(out, t)
		}
	}
	return out
}

// ParseNimbusTags extracts the Nimbus tier and OS template from a VM's tags.
// Returns isNimbus=true when the bare "nimbus" marker is present, regardless
// of whether tier/os tags were also found — a VM may legitimately carry only
// the marker (e.g. backfilled rows whose tier is unknown).
func ParseNimbusTags(tags []string) (tier, osTemplate string, isNimbus bool) {
	for _, t := range tags {
		switch {
		case t == NimbusMarkerTag:
			isNimbus = true
		case strings.HasPrefix(t, nimbusTierPrefix):
			tier = strings.TrimPrefix(t, nimbusTierPrefix)
		case strings.HasPrefix(t, nimbusOSPrefix):
			osTemplate = decodeOSTag(strings.TrimPrefix(t, nimbusOSPrefix))
		}
	}
	return tier, osTemplate, isNimbus
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

func encodeOSTag(osTemplate string) string {
	return strings.ReplaceAll(osTemplate, ".", "_")
}

func decodeOSTag(encoded string) string {
	return strings.ReplaceAll(encoded, "_", ".")
}
