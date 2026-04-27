package utils

import (
	"fmt"
	"regexp"
	"strconv"
	"time"
)

// unitPattern matches one or more segments like "1y", "2m", "3w", "4d", "5h".
// Supports additive combination: "1m2w3d" = 1 month + 2 weeks + 3 days.
var unitPattern = regexp.MustCompile(`(\d+)\s*(y|m|w|d|h)`)

// ParseHumanDuration parses a human-friendly duration string into time.Duration.
// Supported units: y (365 days), m (30 days), w (7 days), d (1 day), h (1 hour).
// Segments are additive: "1m2w" = 30d + 14d = 44 days.
// Returns an error if the input is empty or contains no valid segments.
func ParseHumanDuration(s string) (time.Duration, error) {
	matches := unitPattern.FindAllStringSubmatch(s, -1)
	if len(matches) == 0 {
		return 0, fmt.Errorf("invalid duration %q: expected format like 7d, 1m2w, 2w3d4h", s)
	}

	var total time.Duration
	for _, match := range matches {
		n, err := strconv.Atoi(match[1])
		if err != nil {
			return 0, fmt.Errorf("invalid number in duration %q: %w", s, err)
		}
		switch match[2] {
		case "h":
			total += time.Duration(n) * time.Hour
		case "d":
			total += time.Duration(n) * 24 * time.Hour
		case "w":
			total += time.Duration(n) * 7 * 24 * time.Hour
		case "m":
			total += time.Duration(n) * 30 * 24 * time.Hour
		case "y":
			total += time.Duration(n) * 365 * 24 * time.Hour
		}
	}

	if total <= 0 {
		return 0, fmt.Errorf("duration %q resolves to zero", s)
	}
	return total, nil
}
