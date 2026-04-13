package scheduler

import "time"

// Task represents the smallest schedulable unit.
type Task struct {
	ID          string
	JobID       string
	ProjectName string
	Command     string            // fully instantiated command line
	Params      map[string]any    // the parameter values for this task
	GPUsNeeded  int
	GPUs        []int             // assigned GPU indices (filled after scheduling)
	Status      TaskStatus
	RetryCount  int
	MaxRetry    int               // 0 = unlimited
	PID         int               // OS process ID (for reclaim)
	StartTime   int64             // /proc/<pid>/stat starttime (for reclaim validation)
	LogPath     string
	EnqueuedAt  time.Time
	StartedAt   *time.Time
	FinishedAt  *time.Time
}

// TaskStatus represents the lifecycle state of a task.
type TaskStatus string

const (
	StatusPending TaskStatus = "pending"
	StatusRunning TaskStatus = "running"
	StatusSuccess TaskStatus = "success"
	StatusFailed  TaskStatus = "failed"
	StatusKilled  TaskStatus = "killed"
)

// Queue is a FIFO task queue with backfill + aging support.
//
// TODO: implement
type Queue struct {
	// TODO: implement
}
