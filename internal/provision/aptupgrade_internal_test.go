package provision

import "testing"

// aptUpgradeFor is unexported; internal test package, same split the
// cpuTypeFor table uses.
func TestAptUpgradeFor(t *testing.T) {
	t.Parallel()
	tr, fa := true, false
	cases := []struct {
		name           string
		clusterDefault bool
		override       *bool
		want           bool
	}{
		// The case that matters: an API client omitting the field must
		// NOT silently override an operator who turned upgrades on.
		{"nil falls through to the cluster default (on)", true, nil, true},
		{"nil falls through to the cluster default (off)", false, nil, false},
		{"explicit true wins over an off default", false, &tr, true},
		{"explicit false wins over an on default", true, &fa, false},
		{"explicit value matching the default is a no-op", false, &fa, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			if got := aptUpgradeFor(c.clusterDefault, c.override); got != c.want {
				t.Errorf("aptUpgradeFor(%v, %v) = %v, want %v", c.clusterDefault, c.override, got, c.want)
			}
		})
	}
}
