package proxmox

import (
	"fmt"
	"sort"
	"strings"
)

// NimbusTagBG / NimbusTagFG are the colors Nimbus pins the `nimbus` tag chip
// to in the Proxmox UI. Light grey background with dark text — visible
// enough to find a Nimbus VM in a list, quiet enough that a row of them
// doesn't pull the eye. Hex without the leading `#` per Proxmox's wire
// format.
const (
	NimbusTagBG = "e8e6e1"
	NimbusTagFG = "3a3543"
)

// TagStyle holds the parsed Proxmox `tag-style` datacenter option.
//
// Wire format (per Proxmox property-string syntax):
//
//	[case-sensitive=<1|0>],[color-map=<tag>:<bg>[:<fg>][;...]],[ordering=<...>],[shape=<...>]
//
// Top-level sub-properties separated by `,`; entries inside `color-map`
// separated by `;`; tag/bg/fg inside one entry separated by `:`.
type TagStyle struct {
	CaseSensitive *bool               // nil = field unset on the cluster
	ColorMap      map[string]TagColor // tag → color override
	Ordering      string              // "config" / "alphabetical" / ""
	Shape         string              // "circle" / "dense" / "full" / "none" / ""
}

// TagColor is one (background, foreground) override for a single tag.
// Foreground may be empty when the user only set a background — Proxmox
// auto-picks a contrasting text color in that case.
type TagColor struct {
	BG string // hex without `#`, e.g. "e8e6e1"
	FG string // hex without `#`, may be empty
}

// ParseTagStyle decodes Proxmox's tag-style property string. Unknown
// sub-properties are dropped silently — if Proxmox grows new ones, callers
// who only care about color-map (us) won't accidentally write them back
// missing.
func ParseTagStyle(raw string) TagStyle {
	out := TagStyle{ColorMap: map[string]TagColor{}}
	if strings.TrimSpace(raw) == "" {
		return out
	}
	for _, part := range strings.Split(raw, ",") {
		key, val, ok := strings.Cut(strings.TrimSpace(part), "=")
		if !ok {
			continue
		}
		switch key {
		case "case-sensitive":
			b := val == "1"
			out.CaseSensitive = &b
		case "color-map":
			for _, entry := range strings.Split(val, ";") {
				entry = strings.TrimSpace(entry)
				if entry == "" {
					continue
				}
				fields := strings.Split(entry, ":")
				if len(fields) < 2 {
					continue
				}
				tc := TagColor{BG: fields[1]}
				if len(fields) >= 3 {
					tc.FG = fields[2]
				}
				out.ColorMap[fields[0]] = tc
			}
		case "ordering":
			out.Ordering = val
		case "shape":
			out.Shape = val
		}
	}
	return out
}

// String renders the TagStyle back to Proxmox's property-string format.
// Returns "" when every field is unset, signaling "leave the cluster
// option empty / use defaults".
func (t TagStyle) String() string {
	parts := make([]string, 0, 4)
	if t.CaseSensitive != nil {
		v := "0"
		if *t.CaseSensitive {
			v = "1"
		}
		parts = append(parts, "case-sensitive="+v)
	}
	if len(t.ColorMap) > 0 {
		// Sort tag names so the output is deterministic — easier to test
		// and produces stable diffs when an admin views the option.
		tags := make([]string, 0, len(t.ColorMap))
		for k := range t.ColorMap {
			tags = append(tags, k)
		}
		sort.Strings(tags)
		entries := make([]string, 0, len(tags))
		for _, tag := range tags {
			c := t.ColorMap[tag]
			if c.FG == "" {
				entries = append(entries, fmt.Sprintf("%s:%s", tag, c.BG))
				continue
			}
			entries = append(entries, fmt.Sprintf("%s:%s:%s", tag, c.BG, c.FG))
		}
		parts = append(parts, "color-map="+strings.Join(entries, ";"))
	}
	if t.Ordering != "" {
		parts = append(parts, "ordering="+t.Ordering)
	}
	if t.Shape != "" {
		parts = append(parts, "shape="+t.Shape)
	}
	return strings.Join(parts, ",")
}

// EnsureNimbusColor applies the Nimbus default color to the nimbus tag,
// preserving every other entry. Returns true when the style changed (i.e.
// the caller should write it back).
func (t *TagStyle) EnsureNimbusColor(bg, fg string) bool {
	if t.ColorMap == nil {
		t.ColorMap = map[string]TagColor{}
	}
	current, exists := t.ColorMap[NimbusMarkerTag]
	if exists && current.BG == bg && current.FG == fg {
		return false
	}
	t.ColorMap[NimbusMarkerTag] = TagColor{BG: bg, FG: fg}
	return true
}
