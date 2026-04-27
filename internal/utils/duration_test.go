package utils

import (
	"testing"
	"time"
)

func TestParseHumanDuration(t *testing.T) {
	tests := []struct {
		input string
		want  time.Duration
	}{
		{"7d", 7 * 24 * time.Hour},
		{"24h", 24 * time.Hour},
		{"1w", 7 * 24 * time.Hour},
		{"1m", 30 * 24 * time.Hour},
		{"1y", 365 * 24 * time.Hour},
		{"1m2w", (30 + 14) * 24 * time.Hour},
		{"1m2w3d4h", (30+14+3)*24*time.Hour + 4*time.Hour},
		{"2w3d", (14 + 3) * 24 * time.Hour},
		{"1y6m", (365 + 180) * 24 * time.Hour},
	}
	for _, tt := range tests {
		got, err := ParseHumanDuration(tt.input)
		if err != nil {
			t.Errorf("ParseHumanDuration(%q) error: %v", tt.input, err)
			continue
		}
		if got != tt.want {
			t.Errorf("ParseHumanDuration(%q) = %v, want %v", tt.input, got, tt.want)
		}
	}
}

func TestParseHumanDurationErrors(t *testing.T) {
	invalid := []string{"", "abc", "123", "5x", "0d"}
	for _, s := range invalid {
		_, err := ParseHumanDuration(s)
		if err == nil {
			t.Errorf("ParseHumanDuration(%q) expected error, got nil", s)
		}
	}
}
