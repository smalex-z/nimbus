package proxmox_test

import (
	"testing"

	"nimbus/internal/proxmox"
)

func TestParseTagStyle_Empty(t *testing.T) {
	t.Parallel()
	got := proxmox.ParseTagStyle("")
	if len(got.ColorMap) != 0 {
		t.Errorf("ColorMap = %v, want empty", got.ColorMap)
	}
	if got.CaseSensitive != nil {
		t.Errorf("CaseSensitive = %v, want nil", got.CaseSensitive)
	}
	if got.Shape != "" || got.Ordering != "" {
		t.Errorf("Shape/Ordering = %q/%q, want empty", got.Shape, got.Ordering)
	}
}

func TestParseTagStyle_FullRoundTrip(t *testing.T) {
	t.Parallel()
	input := "color-map=nimbus:e8e6e1:3a3543;production:ff0000,ordering=alphabetical,shape=dense"
	got := proxmox.ParseTagStyle(input)

	if c := got.ColorMap["nimbus"]; c.BG != "e8e6e1" || c.FG != "3a3543" {
		t.Errorf("nimbus color = %+v, want {e8e6e1 3a3543}", c)
	}
	if c := got.ColorMap["production"]; c.BG != "ff0000" || c.FG != "" {
		t.Errorf("production color = %+v, want {ff0000 \"\"}", c)
	}
	if got.Ordering != "alphabetical" {
		t.Errorf("Ordering = %q, want alphabetical", got.Ordering)
	}
	if got.Shape != "dense" {
		t.Errorf("Shape = %q, want dense", got.Shape)
	}

	// Re-encode and verify it still parses to the same fields.
	again := proxmox.ParseTagStyle(got.String())
	if again.ColorMap["nimbus"].BG != "e8e6e1" {
		t.Errorf("re-parsed nimbus BG = %q, want e8e6e1", again.ColorMap["nimbus"].BG)
	}
	if again.Shape != "dense" {
		t.Errorf("re-parsed Shape = %q, want dense", again.Shape)
	}
}

func TestEnsureNimbusColor(t *testing.T) {
	t.Parallel()

	t.Run("adds when missing", func(t *testing.T) {
		t.Parallel()
		s := proxmox.ParseTagStyle("")
		changed := s.EnsureNimbusColor("e8e6e1", "3a3543")
		if !changed {
			t.Error("changed = false on missing-marker case, want true")
		}
		if c := s.ColorMap["nimbus"]; c.BG != "e8e6e1" || c.FG != "3a3543" {
			t.Errorf("nimbus color = %+v, want {e8e6e1 3a3543}", c)
		}
	})

	t.Run("idempotent when already correct", func(t *testing.T) {
		t.Parallel()
		s := proxmox.ParseTagStyle("color-map=nimbus:e8e6e1:3a3543")
		changed := s.EnsureNimbusColor("e8e6e1", "3a3543")
		if changed {
			t.Error("changed = true on already-correct case, want false")
		}
	})

	t.Run("updates when wrong color", func(t *testing.T) {
		t.Parallel()
		s := proxmox.ParseTagStyle("color-map=nimbus:ff0000:000000")
		changed := s.EnsureNimbusColor("e8e6e1", "3a3543")
		if !changed {
			t.Error("changed = false on wrong-color case, want true")
		}
		if c := s.ColorMap["nimbus"]; c.BG != "e8e6e1" {
			t.Errorf("nimbus BG = %q, want e8e6e1", c.BG)
		}
	})

	t.Run("preserves other tags' colors", func(t *testing.T) {
		t.Parallel()
		s := proxmox.ParseTagStyle("color-map=production:ff0000;team-data:00aaff:ffffff,shape=dense")
		s.EnsureNimbusColor("e8e6e1", "3a3543")

		if c := s.ColorMap["production"]; c.BG != "ff0000" {
			t.Errorf("production color clobbered: %+v", c)
		}
		if c := s.ColorMap["team-data"]; c.BG != "00aaff" || c.FG != "ffffff" {
			t.Errorf("team-data color clobbered: %+v", c)
		}
		if s.Shape != "dense" {
			t.Errorf("Shape clobbered: %q", s.Shape)
		}
	})
}

func TestTagStyle_String_Deterministic(t *testing.T) {
	t.Parallel()
	// Two equivalent styles built in different orders should encode the same.
	a := proxmox.TagStyle{ColorMap: map[string]proxmox.TagColor{
		"nimbus":     {BG: "e8e6e1", FG: "3a3543"},
		"production": {BG: "ff0000"},
	}}
	b := proxmox.TagStyle{ColorMap: map[string]proxmox.TagColor{
		"production": {BG: "ff0000"},
		"nimbus":     {BG: "e8e6e1", FG: "3a3543"},
	}}
	if a.String() != b.String() {
		t.Errorf("non-deterministic output: %q vs %q", a.String(), b.String())
	}
}
