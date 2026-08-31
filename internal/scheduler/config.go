package scheduler

import "time"

// Config holds scheduler tuning parameters.
type Config struct {
	TickInterval time.Duration // how often the scheduler loop runs
}

// DefaultConfig returns sensible defaults for a research lab.
func DefaultConfig() Config {
	return Config{
		TickInterval: time.Second,
	}
}
