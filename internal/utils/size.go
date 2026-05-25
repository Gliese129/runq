package utils

import (
	"fmt"
	"strconv"
	"strings"
)

// ParseHumanSize parses a human-friendly size string (e.g. "50GB", "1.5T", "500M")
// into a byte count. Supports k/K, m/M, g/G, t/T as multipliers (1024-based),
// with optional trailing "B" or "b". Case insensitive. "0" returns 0.
//
// Used by YAML config parsing so users can write "50GB" / "1.5T" instead of
// raw byte counts.
func ParseHumanSize(s string) (int64, error) {
	if s == "" {
		return 0, fmt.Errorf("empty size string")
	}
	raw := strings.ToLower(strings.TrimSpace(s))
	// Strip optional trailing "b" (e.g. "GB" → "G").
	raw = strings.TrimSuffix(raw, "b")

	multiplier := int64(1)
	if n := len(raw); n > 0 {
		switch raw[n-1] {
		case 'k':
			multiplier = 1 << 10
			raw = raw[:n-1]
		case 'm':
			multiplier = 1 << 20
			raw = raw[:n-1]
		case 'g':
			multiplier = 1 << 30
			raw = raw[:n-1]
		case 't':
			multiplier = 1 << 40
			raw = raw[:n-1]
		}
	}

	raw = strings.TrimSpace(raw)
	if raw == "" {
		return 0, fmt.Errorf("invalid size %q: no number", s)
	}

	// Try integer first to avoid float rounding when possible.
	if multiplier == 1 {
		n, err := strconv.ParseInt(raw, 10, 64)
		if err != nil {
			return 0, fmt.Errorf("invalid size %q: %w", s, err)
		}
		return n, nil
	}

	f, err := strconv.ParseFloat(raw, 64)
	if err != nil {
		return 0, fmt.Errorf("invalid size %q: %w", s, err)
	}
	if f < 0 {
		return 0, fmt.Errorf("invalid size %q: negative", s)
	}
	return int64(f * float64(multiplier)), nil
}
