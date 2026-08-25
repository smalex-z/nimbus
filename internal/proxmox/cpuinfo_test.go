package proxmox_test

import (
	"testing"

	"nimbus/internal/proxmox"
)

// Flag fixtures captured from live PVE 9 hosts (trimmed to the flags the
// x86-64 level check cares about, order preserved as reported).
const (
	// Xeon E5-2620 v2, Ivy Bridge — has AVX and AES but no AVX2/BMI/FMA.
	flagsIvyBridge = "fpu nx lm pni ssse3 cx16 sse4_1 sse4_2 popcnt aes xsave avx f16c rdrand lahf_lm"
	// Xeon E5-2603 v3, Haswell — the full x86-64-v3 set.
	flagsHaswell = "fpu nx lm pni ssse3 fma cx16 sse4_1 sse4_2 movbe popcnt aes xsave avx f16c rdrand lahf_lm abm bmi1 avx2 bmi2"
)

func TestCPUInfoSupportsAVX2(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name  string
		flags string
		want  bool
	}{
		{"ivy bridge has avx but not avx2", flagsIvyBridge, false},
		{"haswell has the full v3 set", flagsHaswell, true},
		{"empty flags is not a promise", "", false},
		// The alias that makes this whole function necessary. Linux
		// reports LZCNT as "abm" on Intel; no host in a real cluster
		// advertises a literal "lzcnt", so a psABI-literal check
		// returns false everywhere. Guard against a regression that
		// drops the alias.
		{"lzcnt spelled abm still counts", flagsHaswell, true},
		{"lzcnt spelled lzcnt also counts",
			"pni ssse3 sse4_1 sse4_2 popcnt avx avx2 bmi1 bmi2 f16c fma movbe lzcnt", true},
		{"v3 set minus lzcnt/abm is not v3",
			"pni ssse3 sse4_1 sse4_2 popcnt avx avx2 bmi1 bmi2 f16c fma movbe", false},
		{"avx2 alone is not the v3 set", "avx avx2", false},
		// A substring of a longer flag must not satisfy the check —
		// fields are split on whitespace, not matched by Contains.
		{"substring of another flag doesn't match", "avx512f avx2_fake bmi1x", false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			ci := &proxmox.CPUInfo{Flags: c.flags}
			if got := ci.SupportsAVX2(); got != c.want {
				t.Errorf("SupportsAVX2(%q) = %v, want %v", c.flags, got, c.want)
			}
		})
	}
}

// A nil CPUInfo is what a failed /status call leaves behind; the
// nodemgr fanout guards it, but the method must not panic on its own.
func TestCPUInfoSupportsAVX2_NilReceiver(t *testing.T) {
	t.Parallel()
	var ci *proxmox.CPUInfo
	if ci.SupportsAVX2() {
		t.Error("nil CPUInfo should not report AVX2 support")
	}
}
