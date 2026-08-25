package provision

import "testing"

// cpuTypeFor is an unexported helper, so this file uses the internal
// test package (same split ippool uses for its internal helpers).
func TestCPUTypeFor(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		base string
		tags []string
		want string
	}{
		{"no tags passes the default through", "x86-64-v2-AES", nil, "x86-64-v2-AES"},
		{"unrelated tags don't upgrade", "x86-64-v2-AES", []string{"ssd", "gpu"}, "x86-64-v2-AES"},
		{"avx2 raises the portable default", "x86-64-v2-AES", []string{"avx2"}, "x86-64-v3"},
		{"avx2 alongside other tags", "x86-64-v2-AES", []string{"nvme", "avx2"}, "x86-64-v3"},
		{"avx2 raises kvm64 too", "kvm64", []string{"avx2"}, "x86-64-v3"},
		// Empty base means "leave the template's model", which on stock
		// Proxmox is kvm64/v2-AES — no AVX2. An explicit request has to
		// override it or the guest silently never sees the feature.
		{"empty base is overridden", "", []string{"avx2"}, "x86-64-v3"},
		{"empty base without avx2 stays empty", "", nil, ""},
		// Never downgrade: these already include AVX2, and rewriting
		// them would strip capability the operator deliberately chose.
		{"v4 is not downgraded", "x86-64-v4", []string{"avx2"}, "x86-64-v4"},
		{"host is not rewritten", "host", []string{"avx2"}, "host"},
		{"named model is not rewritten", "Skylake-Server", []string{"avx2"}, "Skylake-Server"},
		{"v3 is already correct", "x86-64-v3", []string{"avx2"}, "x86-64-v3"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			if got := cpuTypeFor(c.base, c.tags); got != c.want {
				t.Errorf("cpuTypeFor(%q, %v) = %q, want %q", c.base, c.tags, got, c.want)
			}
		})
	}
}
