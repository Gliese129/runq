package utils

import "testing"

func TestParseHumanSize(t *testing.T) {
	cases := []struct {
		in   string
		want int64
		err  bool
	}{
		{"0", 0, false},
		{"500", 500, false},
		{"500K", 500 << 10, false},
		{"500M", 500 << 20, false},
		{"50G", 50 << 30, false},
		{"50GB", 50 << 30, false},
		{"1T", 1 << 40, false},
		{"1.5G", int64(1.5 * float64(1<<30)), false},
		{"1gb", 1 << 30, false},
		{"  50g  ", 50 << 30, false}, // whitespace tolerated
		{"", 0, true},
		{"abc", 0, true},
		{"-5G", 0, true},
		{"G", 0, true},
	}
	for _, tc := range cases {
		got, err := ParseHumanSize(tc.in)
		if tc.err {
			if err == nil {
				t.Errorf("ParseHumanSize(%q) expected error, got %d", tc.in, got)
			}
			continue
		}
		if err != nil {
			t.Errorf("ParseHumanSize(%q) unexpected error: %v", tc.in, err)
			continue
		}
		if got != tc.want {
			t.Errorf("ParseHumanSize(%q) = %d, want %d", tc.in, got, tc.want)
		}
	}
}
