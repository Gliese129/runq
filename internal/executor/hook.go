package executor

import (
	"context"
	"time"
)

// TaskEvent carries information about a lifecycle event from a running task.
// Used by Hook implementations (e.g. L3-A CUDA metrics hook).
type TaskEvent struct {
	TaskID    string
	Timestamp time.Time
	Type      string         // "start", "exit", "metric", "checkpoint"
	Data      map[string]any // event-specific payload
}

// Hook is called at key points in a task's lifecycle.
// Implementations can collect metrics, trigger checkpoints, etc.
// Register hooks via Executor configuration (not yet wired — L3-A).
type Hook interface {
	OnTaskStart(ctx context.Context, task *RunSpec) error
	OnTaskExit(ctx context.Context, task *RunSpec, exitCode int) error
	OnEvent(ctx context.Context, event TaskEvent)
}
