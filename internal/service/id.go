package service

import (
	"crypto/rand"
	"fmt"
	"time"
)

// GenerateID returns a short, human-readable unique ID (hex timestamp + random suffix).
func GenerateID() string {
	ts := time.Now().Unix()
	rd := make([]byte, 4)
	if _, err := rand.Read(rd); err != nil {
		panic("entropy source unavailable")
	}
	return fmt.Sprintf("%x%x", ts, rd)
}
