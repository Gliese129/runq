package scheduler

import (
	"context"
	"time"
)

// ScheduleContext is the snapshot passed to Prioritizer each tick.
// Contains everything needed to make scheduling decisions.
type ScheduleContext struct {
	Ctx      context.Context // scheduler lifecycle context — cancels on shutdown
	Pending  []*Task         // tasks waiting for GPUs
	Running  []*Task         // tasks currently executing (L3: will carry Progress)
	FreeGPUs int             // number of currently unallocated GPUs
	Now      time.Time       // current wall clock time
}

// Priority is the output per task — score + optional metadata.
type Priority struct {
	TaskID string
	Score  float64        // higher = schedule first
	Meta   map[string]any // L3: predicted_remaining, confidence, etc.
}

// Prioritizer ranks pending tasks for scheduling.
// The scheduler dispatches tasks in the order returned by Prioritize.
type Prioritizer interface {
	// Prioritize returns pending tasks ranked by scheduling priority (highest first).
	Prioritize(ctx ScheduleContext) []Priority

	// Name returns the strategy name for logging/config ("fifo", "fair").
	Name() string
}
