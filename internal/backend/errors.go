package backend

import (
	"errors"
	"strings"
)

var (
	ErrNotSupported       = errors.New("not supported")
	ErrNotFound           = errors.New("not found")
	ErrNoTargetConfigured = errors.New("no target configured")
)

// IsNotFound reports whether err represents a "not found" condition.
// Prefers errors.Is for sentinel-based checks; falls back to string
// matching for errors originating outside this package.
func IsNotFound(err error) bool {
	if errors.Is(err, ErrNotFound) {
		return true
	}
	return err != nil && strings.Contains(strings.ToLower(err.Error()), "not found")
}
