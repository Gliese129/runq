package utils

import (
	"crypto/rand"
	"fmt"
)

// GenerateJobID / GenerateTaskID return typed ids ("jb"/"tk" + 8 hex):
// the prefix makes a job and a task distinguishable at a glance — in paths,
// qstat output and CLI arguments. The hex body keeps collision probability
// negligible at lab scale.
func GenerateJobID() string { return "jb" + GenerateID() }

func GenerateTaskID() string { return "tk" + GenerateID() }

// GenerateID returns a short, human-friendly 8-hex-char ID.
// Format: 4 bytes random -> "a3f1b20e". Collision probability is negligible
// at lab scale (< 10k tasks): ~1 in 4 billion per pair.
func GenerateID() string {
	rd := make([]byte, 4)
	if _, err := rand.Read(rd); err != nil {
		panic("entropy source unavailable")
	}
	return fmt.Sprintf("%x", rd)
}
