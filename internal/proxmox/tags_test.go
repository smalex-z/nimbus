package proxmox_test

import (
	"reflect"
	"testing"

	"nimbus/internal/proxmox"
)

func TestEncodeNimbusTags(t *testing.T) {
	t.Parallel()
	got := proxmox.EncodeNimbusTags("medium", "ubuntu-22.04")
	want := []string{"nimbus", "nimbus-tier-medium", "nimbus-os-ubuntu-22_04"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("EncodeNimbusTags = %v, want %v", got, want)
	}
}

func TestParseNimbusTags(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name           string
		tags           []string
		wantTier       string
		wantOSTemplate string
		wantIsNimbus   bool
	}{
		{
			name:           "full marker set",
			tags:           []string{"nimbus", "nimbus-tier-medium", "nimbus-os-ubuntu-22_04"},
			wantTier:       "medium",
			wantOSTemplate: "ubuntu-22.04",
			wantIsNimbus:   true,
		},
		{
			name:           "marker only — backfilled row with unknown tier/os",
			tags:           []string{"nimbus"},
			wantTier:       "",
			wantOSTemplate: "",
			wantIsNimbus:   true,
		},
		{
			name:           "no nimbus tag — purely external VM",
			tags:           []string{"production", "team-data"},
			wantTier:       "",
			wantOSTemplate: "",
			wantIsNimbus:   false,
		},
		{
			name:           "marker mixed with user tags is preserved",
			tags:           []string{"production", "nimbus", "nimbus-tier-large", "nimbus-os-debian-12"},
			wantTier:       "large",
			wantOSTemplate: "debian-12",
			wantIsNimbus:   true,
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

func TestMergeNimbusTags_PreservesUserTagsAndReplacesOldMarkers(t *testing.T) {
	t.Parallel()
	existing := []string{"production", "nimbus-tier-small", "nimbus-os-ubuntu-22_04", "nimbus", "team-data"}
	got := proxmox.MergeNimbusTags(existing, "large", "debian-12")

	wantSet := map[string]bool{
		"production":          true,
		"team-data":           true,
		"nimbus":              true,
		"nimbus-tier-large":   true,
		"nimbus-os-debian-12": true,
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

func TestSplitTags(t *testing.T) {
	t.Parallel()
	tests := []struct {
		raw  string
		want []string
	}{
		{"", nil},
		{"nimbus", []string{"nimbus"}},
		{"nimbus;nimbus-tier-medium", []string{"nimbus", "nimbus-tier-medium"}},
		{"nimbus,nimbus-tier-medium", []string{"nimbus", "nimbus-tier-medium"}},
		{"nimbus nimbus-tier-medium", []string{"nimbus", "nimbus-tier-medium"}},
		{"  nimbus ;; nimbus-os-debian-12  ", []string{"nimbus", "nimbus-os-debian-12"}},
	}
	for _, tt := range tests {
		got := proxmox.SplitTags(tt.raw)
		if !reflect.DeepEqual(got, tt.want) {
			t.Errorf("SplitTags(%q) = %v, want %v", tt.raw, got, tt.want)
		}
	}
}
