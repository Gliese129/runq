package scheduler

import (
	"fmt"
	"sync"
	"time"
)

// TaskStatus represents the lifecycle state of a task.
type TaskStatus string

const (
	StatusPending TaskStatus = "pending"
	StatusRunning TaskStatus = "running"
	StatusSuccess TaskStatus = "success"
	StatusFailed  TaskStatus = "failed"
	StatusKilled  TaskStatus = "killed"
)

// Task represents the smallest schedulable unit.
type Task struct {
	ID          string
	JobID       string
	ProjectName string
	Command     string         // fully instantiated command line
	Params      map[string]any // the parameter values for this task
	GPUsNeeded  int
	GPUs        []int // assigned GPU indices (filled after scheduling)
	Status      TaskStatus
	RetryCount  int
	MaxRetry    int // 0 = unlimited
	PID         int
	StartTime   time.Time // absolute process start time (for reclaim)
	LogPath     string
	WorkingDir  string
	Env         map[string]string
	EnqueuedAt  time.Time
	StartedAt   *time.Time
	FinishedAt  *time.Time

	// Resume support
	Resumable bool   // from project config
	ExtraArgs string // appended to command on resume restart

	// L2 multi-user support (populated when Linux user/group model is enabled)
	UID   int    `json:"uid,omitempty"`
	User  string `json:"user,omitempty"`
	Group string `json:"group,omitempty"`

	// L3 checkpoint support (directory organized by project, inherited on requeue)
	CheckpointDir string `json:"checkpoint_dir,omitempty"`
}

// Queue is a FIFO task queue with backfill + aging support. Thread-safe.
type Queue struct {
	mu    sync.Mutex
	tasks []*Task
}

// NewQueue creates an empty queue.
func NewQueue() *Queue {
	return &Queue{tasks: make([]*Task, 0)}
}

// Push adds a task to the back of the queue with status=pending.
func (q *Queue) Push(task *Task) {
	q.mu.Lock()
	defer q.mu.Unlock()

	task.Status = StatusPending
	task.EnqueuedAt = time.Now()
	q.tasks = append(q.tasks, task)
}

// PushBatch adds multiple tasks at once (e.g. from a job expansion).
func (q *Queue) PushBatch(tasks []*Task) {
	q.mu.Lock()
	defer q.mu.Unlock()

	now := time.Now()
	for _, t := range tasks {
		t.Status = StatusPending
		t.EnqueuedAt = now
		q.tasks = append(q.tasks, t)
	}
}

// Peek returns the first pending task without removing it. Returns nil if empty.
func (q *Queue) Peek() *Task {
	q.mu.Lock()
	defer q.mu.Unlock()

	for _, t := range q.tasks {
		if t.Status == StatusPending {
			return t
		}
	}
	return nil
}

// PeekSchedulable returns the first pending task that fits within freeGPUs.
// Used for backfill when the head task is too large.
func (q *Queue) PeekSchedulable(freeGPUs int, jobFilter map[string]bool) *Task {
	q.mu.Lock()
	defer q.mu.Unlock()

	for _, t := range q.tasks {
		filtered := jobFilter[t.JobID]
		if t.Status == StatusPending && t.GPUsNeeded <= freeGPUs && !filtered {
			return t
		}
	}
	return nil
}

// MarkRunning transitions a task from pending to running.
func (q *Queue) MarkRunning(taskID string) error {
	q.mu.Lock()
	defer q.mu.Unlock()

	t := q.findLocked(taskID)
	if t == nil {
		return fmt.Errorf("task %q not found in queue", taskID)
	}
	if t.Status != StatusPending {
		return fmt.Errorf("task %q is %s, expected pending", taskID, t.Status)
	}
	t.Status = StatusRunning
	now := time.Now()
	t.StartedAt = &now
	return nil
}

// Complete transitions a task to a terminal state (success/failed/killed).
func (q *Queue) Complete(taskID string, status TaskStatus) error {
	q.mu.Lock()
	defer q.mu.Unlock()

	t := q.findLocked(taskID)
	if t == nil {
		return fmt.Errorf("task %q not found in queue", taskID)
	}
	t.Status = status
	now := time.Now()
	t.FinishedAt = &now
	return nil
}

// Requeue moves a failed task back to pending for retry. Increments RetryCount.
func (q *Queue) Requeue(taskID string) error {
	q.mu.Lock()
	defer q.mu.Unlock()

	t := q.findLocked(taskID)
	if t == nil {
		return fmt.Errorf("task %q not found in queue", taskID)
	}
	t.Status = StatusPending
	t.RetryCount++
	t.GPUs = nil
	t.StartedAt = nil
	t.FinishedAt = nil
	return nil
}

// RetryExisting updates an existing terminal task in-place for manual retry.
// Returns false if the task is not currently in the queue.
func (q *Queue) RetryExisting(task *Task) bool {
	q.mu.Lock()
	defer q.mu.Unlock()

	existing := q.findLocked(task.ID)
	if existing == nil {
		return false
	}
	existing.JobID = task.JobID
	existing.ProjectName = task.ProjectName
	existing.Command = task.Command
	existing.Params = task.Params
	existing.GPUsNeeded = task.GPUsNeeded
	existing.GPUs = nil
	existing.Status = StatusPending
	existing.RetryCount = task.RetryCount
	existing.MaxRetry = task.MaxRetry
	existing.PID = 0
	existing.StartTime = time.Time{}
	existing.LogPath = task.LogPath
	existing.WorkingDir = task.WorkingDir
	existing.Env = task.Env
	existing.EnqueuedAt = time.Now()
	existing.StartedAt = nil
	existing.FinishedAt = nil
	existing.Resumable = task.Resumable
	existing.ExtraArgs = task.ExtraArgs
	existing.UID = task.UID
	existing.User = task.User
	existing.Group = task.Group
	existing.CheckpointDir = task.CheckpointDir
	return true
}

// Get returns a task by ID. Returns nil if not found.
func (q *Queue) Get(taskID string) *Task {
	q.mu.Lock()
	defer q.mu.Unlock()
	return q.findLocked(taskID)
}

// ListByStatus returns all tasks matching the given status.
func (q *Queue) ListByStatus(status TaskStatus) []*Task {
	q.mu.Lock()
	defer q.mu.Unlock()

	var result []*Task
	for _, t := range q.tasks {
		if t.Status == status {
			result = append(result, t)
		}
	}
	return result
}

// ListByJob returns all tasks belonging to a job.
func (q *Queue) ListByJob(jobID string) []*Task {
	q.mu.Lock()
	defer q.mu.Unlock()

	var result []*Task
	for _, t := range q.tasks {
		if t.JobID == jobID {
			result = append(result, t)
		}
	}
	return result
}

// Len returns the total number of tasks in the queue (all statuses).
func (q *Queue) Len() int {
	q.mu.Lock()
	defer q.mu.Unlock()
	return len(q.tasks)
}

// PendingCount returns the number of pending tasks.
func (q *Queue) PendingCount() int {
	q.mu.Lock()
	defer q.mu.Unlock()

	n := 0
	for _, t := range q.tasks {
		if t.Status == StatusPending {
			n++
		}
	}
	return n
}

// findLocked returns a task by ID. Caller must hold mu.
func (q *Queue) findLocked(taskID string) *Task {
	for _, t := range q.tasks {
		if t.ID == taskID {
			return t
		}
	}
	return nil
}

// FetchAll returns all tasks in the queue
func (q *Queue) FetchAll() []Task {
	q.mu.Lock()
	defer q.mu.Unlock()

	results := make([]Task, 0, len(q.tasks))
	for _, task := range q.tasks {
		results = append(results, Task{
			ID:          task.ID,
			ProjectName: task.ProjectName,
			JobID:       task.JobID,
			EnqueuedAt:  task.EnqueuedAt,
			Status:      task.Status,
		})
	}
	return results
}
