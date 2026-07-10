package version

import "testing"

func TestCompare(t *testing.T) {
	cases := []struct {
		a, b string
		cmp  int
		ok   bool
	}{
		{"v1.2.3", "v1.2.3", 0, true},
		{"1.2.3", "v1.2.3", 0, true},
		{"v0.4.0", "v0.10.0", -1, true},
		{"v1.0.0", "v0.99.99", 1, true},
		{"v1.2", "v1.2.0", 0, true},
		{"v1", "v1.0.0", 0, true},
		{"v1.2.3-rc1", "v1.2.3", 0, true}, // pre-release suffix truncated
		{"dev", "v1.0.0", 0, false},
		{"v1.0.0", "dev", 0, false},
		{"", "v1.0.0", 0, false},
		{"vx.y", "v1.0.0", 0, false},
	}
	for _, c := range cases {
		cmp, ok := Compare(c.a, c.b)
		if ok != c.ok || (ok && cmp != c.cmp) {
			t.Errorf("Compare(%q, %q) = (%d, %v), want (%d, %v)", c.a, c.b, cmp, ok, c.cmp, c.ok)
		}
	}
}
