package proxmox_test

import (
	"reflect"
	"strings"
	"testing"

	"nimbus/internal/proxmox"
)

// sampleUUID is a fixed UUIDv7-shaped string used across the tag tests. The
// short form is the first 8 hex chars of the byte stream — "0193abcd".
const (
	sampleUUID    = "0193abcd-4321-7000-89ab-cdef01234567"
	sampleShortID = "0193abcd"
)

func TestShortNimbusID(t *testing.T) {
	t.Parallel()
	tests := []struct {
		in, want string
	}{
		{sampleUUID, sampleShortID},
		{"0193abcd4321700089abcdef01234567", sampleShortID}, // already hyphenless
		{"", ""},
		{"short", ""},
		{"0193abc", ""}, // 7 chars — below threshold
	}
	for _, tt := range tests {
		if got := proxmox.ShortNimbusID(tt.in); got != tt.want {
			t.Errorf("ShortNimbusID(%q) = %q, want %q", tt.in, got, tt.want)
		}
	}
}

func TestEncodeNimbusTags(t *testing.T) {
	t.Parallel()
	t.Run("with id appends short tag", func(t *testing.T) {
		t.Parallel()
		got := proxmox.EncodeNimbusTags(sampleUUID)
		want := []string{"nimbus", "nimbus-id-" + sampleShortID}
		if !reflect.DeepEqual(got, want) {
			t.Errorf("EncodeNimbusTags(uuid) = %v, want %v", got, want)
		}
	})
	t.Run("without id returns bare marker", func(t *testing.T) {
		t.Parallel()
		got := proxmox.EncodeNimbusTags("")
		want := []string{"nimbus"}
		if !reflect.DeepEqual(got, want) {
			t.Errorf("EncodeNimbusTags(\"\") = %v, want %v", got, want)
		}
	})
}

func TestMergeNimbusTags_StripsLegacyAndStaleShortTags(t *testing.T) {
	t.Parallel()
	existing := []string{
		"production",
		"nimbus-tier-small",      // legacy — must drop
		"nimbus-os-ubuntu-22_04", // legacy — must drop
		"nimbus",                 // already present — dedupe
		"nimbus-id-deadbeef",     // stale short id from prior provision — must drop
		"team-data",
	}
	got := proxmox.MergeNimbusTags(existing, sampleUUID)

	want := []string{"production", "team-data", "nimbus", "nimbus-id-" + sampleShortID}
	wantSet := make(map[string]bool, len(want))
	for _, t := range want {
		wantSet[t] = true
	}
	if len(got) != len(wantSet) {
		t.Fatalf("got %d tags %v, want %d %v", len(got), got, len(wantSet), wantSet)
	}
	for _, tag := range got {
		if !wantSet[tag] {
			t.Errorf("unexpected tag %q in result", tag)
		}
	}
}

func TestHasNimbusTag(t *testing.T) {
	t.Parallel()
	if !proxmox.HasNimbusTag([]string{"nimbus", "production"}) {
		t.Error("HasNimbusTag(nimbus + others) = false, want true")
	}
	if proxmox.HasNimbusTag([]string{"production", "team-data"}) {
		t.Error("HasNimbusTag(no marker) = true, want false")
	}
	if proxmox.HasNimbusTag(nil) {
		t.Error("HasNimbusTag(nil) = true, want false")
	}
}

func TestParseNimbusIDFromTags(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		tags []string
		want string
	}{
		{"present", []string{"nimbus", "nimbus-id-abc12345", "production"}, "abc12345"},
		{"absent (legacy nimbus VM)", []string{"nimbus", "production"}, ""},
		{"absent (no nimbus marker at all)", []string{"production", "team-data"}, ""},
		{"nil", nil, ""},
	}
	for _, tt := range tests {
		if got := proxmox.ParseNimbusIDFromTags(tt.tags); got != tt.want {
			t.Errorf("%s: ParseNimbusIDFromTags(%v) = %q, want %q", tt.name, tt.tags, got, tt.want)
		}
	}
}

func TestParseNimbusTags_LegacyFallback(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name           string
		tags           []string
		wantTier       string
		wantOSTemplate string
		wantIsNimbus   bool
	}{
		{
			name:           "legacy three-tag VM",
			tags:           []string{"nimbus", "nimbus-tier-medium", "nimbus-os-ubuntu-22_04"},
			wantTier:       "medium",
			wantOSTemplate: "ubuntu-22.04",
			wantIsNimbus:   true,
		},
		{
			name:           "marker only — newly tagged but no legacy fields",
			tags:           []string{"nimbus"},
			wantTier:       "",
			wantOSTemplate: "",
			wantIsNimbus:   true,
		},
		{
			name:           "no nimbus marker — purely external VM",
			tags:           []string{"production", "team-data"},
			wantTier:       "",
			wantOSTemplate: "",
			wantIsNimbus:   false,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			tier, os, isNimbus := proxmox.ParseNimbusTags(tt.tags)
			if tier != tt.wantTier {
				t.Errorf("tier = %q, want %q", tier, tt.wantTier)
			}
			if os != tt.wantOSTemplate {
				t.Errorf("osTemplate = %q, want %q", os, tt.wantOSTemplate)
			}
			if isNimbus != tt.wantIsNimbus {
				t.Errorf("isNimbus = %v, want %v", isNimbus, tt.wantIsNimbus)
			}
		})
	}
}

func TestEncodeNimbusDescription(t *testing.T) {
	t.Parallel()
	t.Run("with id", func(t *testing.T) {
		t.Parallel()
		got := proxmox.EncodeNimbusDescription("medium", "ubuntu-22.04", sampleUUID)
		want := "<!-- nimbus: tier=medium os=ubuntu-22.04 id=" + sampleUUID + " -->"
		if got != want {
			t.Errorf("EncodeNimbusDescription = %q, want %q", got, want)
		}
	})
	t.Run("without id omits the field for back-compat", func(t *testing.T) {
		t.Parallel()
		got := proxmox.EncodeNimbusDescription("medium", "ubuntu-22.04", "")
		want := "<!-- nimbus: tier=medium os=ubuntu-22.04 -->"
		if got != want {
			t.Errorf("EncodeNimbusDescription = %q, want %q", got, want)
		}
	})
}

func TestParseNimbusDescription(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name     string
		desc     string
		wantOK   bool
		wantTier string
		wantOS   string
		wantID   string
	}{
		{
			name:     "marker with id",
			desc:     "<!-- nimbus: tier=medium os=ubuntu-22.04 id=" + sampleUUID + " -->",
			wantOK:   true,
			wantTier: "medium",
			wantOS:   "ubuntu-22.04",
			wantID:   sampleUUID,
		},
		{
			name:     "legacy marker without id",
			desc:     "<!-- nimbus: tier=medium os=ubuntu-22.04 -->",
			wantOK:   true,
			wantTier: "medium",
			wantOS:   "ubuntu-22.04",
			wantID:   "",
		},
		{
			name:     "marker after user prose",
			desc:     "Production database server.\n\nOwned by data team.\n\n<!-- nimbus: tier=large os=debian-12 id=" + sampleUUID + " -->",
			wantOK:   true,
			wantTier: "large",
			wantOS:   "debian-12",
			wantID:   sampleUUID,
		},
		{
			name:   "no marker",
			desc:   "Just a regular VM description.",
			wantOK: false,
		},
		{
			name:   "empty description",
			desc:   "",
			wantOK: false,
		},
		{
			name:     "tolerates extra whitespace",
			desc:     "<!--   nimbus:    tier=small  os=debian-11   -->",
			wantOK:   true,
			wantTier: "small",
			wantOS:   "debian-11",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			tier, os, id, ok := proxmox.ParseNimbusDescription(tt.desc)
			if ok != tt.wantOK {
				t.Errorf("ok = %v, want %v", ok, tt.wantOK)
			}
			if tier != tt.wantTier {
				t.Errorf("tier = %q, want %q", tier, tt.wantTier)
			}
			if os != tt.wantOS {
				t.Errorf("os = %q, want %q", os, tt.wantOS)
			}
			if id != tt.wantID {
				t.Errorf("id = %q, want %q", id, tt.wantID)
			}
		})
	}
}

func TestMergeNimbusDescription(t *testing.T) {
	t.Parallel()
	t.Run("appends to empty", func(t *testing.T) {
		t.Parallel()
		got := proxmox.MergeNimbusDescription("", "small", "debian-12", sampleUUID)
		want := "<!-- nimbus: tier=small os=debian-12 id=" + sampleUUID + " -->"
		if got != want {
			t.Errorf("got %q, want %q", got, want)
		}
	})
	t.Run("appends to existing prose", func(t *testing.T) {
		t.Parallel()
		got := proxmox.MergeNimbusDescription("Production DB server.", "large", "ubuntu-22.04", sampleUUID)
		want := "Production DB server.\n\n<!-- nimbus: tier=large os=ubuntu-22.04 id=" + sampleUUID + " -->"
		if got != want {
			t.Errorf("got %q, want %q", got, want)
		}
	})
	t.Run("replaces existing marker without touching prose", func(t *testing.T) {
		t.Parallel()
		input := "Production DB server.\n\n<!-- nimbus: tier=small os=debian-11 -->"
		got := proxmox.MergeNimbusDescription(input, "large", "ubuntu-24.04", sampleUUID)
		want := "Production DB server.\n\n<!-- nimbus: tier=large os=ubuntu-24.04 id=" + sampleUUID + " -->"
		if got != want {
			t.Errorf("got %q, want %q", got, want)
		}
	})
	t.Run("idempotent on identical input", func(t *testing.T) {
		t.Parallel()
		input := "Notes\n\n<!-- nimbus: tier=medium os=ubuntu-22.04 id=" + sampleUUID + " -->"
		got := proxmox.MergeNimbusDescription(input, "medium", "ubuntu-22.04", sampleUUID)
		if got != input {
			t.Errorf("expected idempotent, got %q != %q", got, input)
		}
	})
}

func TestParseSMBIOSUUID(t *testing.T) {
	t.Parallel()
	tests := []struct {
		in, want string
	}{
		{"uuid=01234567-89ab-cdef-0123-456789abcdef,family=acme", "01234567-89ab-cdef-0123-456789abcdef"},
		{"family=acme,uuid=01234567-89ab-cdef-0123-456789abcdef", "01234567-89ab-cdef-0123-456789abcdef"},
		{"uuid=01234567-89ab-cdef-0123-456789abcdef", "01234567-89ab-cdef-0123-456789abcdef"},
		{"family=acme,manufacturer=widget-co", ""},
		{"", ""},
		{"malformed-without-equals", ""},
	}
	for _, tt := range tests {
		if got := proxmox.ParseSMBIOSUUID(tt.in); got != tt.want {
			t.Errorf("ParseSMBIOSUUID(%q) = %q, want %q", tt.in, got, tt.want)
		}
	}
}

func TestSplitTags(t *testing.T) {
	t.Parallel()
	tests := []struct {
		raw  string
		want []string
	}{
		{"", nil},
		{"nimbus", []string{"nimbus"}},
		{"nimbus;production", []string{"nimbus", "production"}},
		{"nimbus,production", []string{"nimbus", "production"}},
		{"nimbus production", []string{"nimbus", "production"}},
		{"  nimbus ;; production  ", []string{"nimbus", "production"}},
	}
	for _, tt := range tests {
		got := proxmox.SplitTags(tt.raw)
		if !reflect.DeepEqual(got, tt.want) {
			t.Errorf("SplitTags(%q) = %v, want %v", tt.raw, got, tt.want)
		}
	}
}

// Smoke test that JoinTags + SplitTags round-trip cleanly.
func TestJoinSplitRoundTrip(t *testing.T) {
	t.Parallel()
	tags := []string{"nimbus", "production", "team-data"}
	joined := proxmox.JoinTags(tags)
	if !strings.Contains(joined, "nimbus") {
		t.Errorf("JoinTags lost nimbus marker: %q", joined)
	}
	got := proxmox.SplitTags(joined)
	if !reflect.DeepEqual(got, tags) {
		t.Errorf("round-trip = %v, want %v", got, tags)
	}
}
