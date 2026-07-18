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
	// StatusUnknown (RQ-74): the submission STARTED but its outcome was lost
	// (connection dropped mid-submit, submit succeeded but the external id
	// could not be parsed/persisted). runq openly does not know whether a
	// cluster job exists, so it neither retries (double-submit risk) nor
	// fails (may be running fine). Not terminal: reconcile settles it from
	// facts (status.json marker / scheduler probe); until then the task
	// carries its error message in failure_detail.
	StatusUnknown TaskStatus = "unknown"
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
	MaxRetry    int // -1 = unlimited, 0 = no retries (zero value is safe)
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

	// B1: task timeout (seconds); 0 = no timeout
	Timeout int `json:"timeout,omitempty"`

	// L2 multi-user support (populated when Linux user/group model is enabled)
	UID   int    `json:"uid,omitempty"`
	User  string `json:"user,omitempty"`
	Group string `json:"group,omitempty"`

	// L3 checkpoint support (directory organized by project, inherited on requeue)
	CheckpointDir string `json:"checkpoint_dir,omitempty"`

	// L2-C: per-task workspace at <root>/<job_id>/<task_id>.
	// Mirrors store.TaskRow.TaskDir; populated by service.JobService.SubmitJob.
	// Used to inject RUNQ_TASK_DIR / RUNQ_PARAMS_FILE / etc. into the task env,
	// and to locate metrics.jsonl during reap.
	TaskDir string `json:"task_dir,omitempty"`

	// P6: W&B integration — sweep key names and job note flow through to
	// workspace.Identity so BaseEnv() can emit WANDB_RUN_GROUP / WANDB_TAGS.
	SweepKeys []string `json:"sweep_keys,omitempty"`
	JobNote   string   `json:"job_note,omitempty"`

	// ExternalID is the remote scheduler's job id for the current attempt
	// (empty before launch; cleared by requeue and manual retry). Purely
	// informational — restore/display/kill plumbing. Verdict staleness is
	// prevented by serializing all verdict producers on the target's
	// lifecycle lock (remote.Backend.lifecycleMu), NOT by comparing ids.
	ExternalID string `json:"external_id,omitempty"`
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

// Restore inserts a task preserving its existing Status and EnqueuedAt.
// Used only at daemon restart to rebuild the queue from DB without resetting wait times.
// The caller is responsible for setting all fields correctly before calling Restore.
func (q *Queue) Restore(task *Task) {
	q.mu.Lock()
	defer q.mu.Unlock()
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

// MarkUnknown transitions an in-flight (running) task to unknown: the
// dispatch started but its outcome was lost (RQ-74). The task keeps its
// submission slot — a cluster job may exist — and leaves unknown only
// through Complete (reconcile verdict / user kill) or Requeue (manual retry).
func (q *Queue) MarkUnknown(taskID string) error {
	q.mu.Lock()
	defer q.mu.Unlock()

	t := q.findLocked(taskID)
	if t == nil {
		return fmt.Errorf("task %q not found in queue", taskID)
	}
	if t.Status != StatusRunning {
		return fmt.Errorf("task %q is %s, expected running", taskID, t.Status)
	}
	t.Status = StatusUnknown
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
	// New attempt: the old external id belongs to a dead cluster job, and
	// clearing it is what invalidates any in-flight stale verdicts.
	t.ExternalID = ""
	return nil
}

// RequeueTransient returns a task to pending WITHOUT consuming retry budget:
// the launch never happened (scheduler unreachable), so this is not an
// attempt — no count, no external id (it was never assigned).
func (q *Queue) RequeueTransient(taskID string) error {
	q.mu.Lock()
	defer q.mu.Unlock()

	t := q.findLocked(taskID)
	if t == nil {
		return fmt.Errorf("task %q not found in queue", taskID)
	}
	t.Status = StatusPending
	t.GPUs = nil
	t.StartedAt = nil
	t.FinishedAt = nil
	t.ExternalID = ""
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
	existing.TaskDir = task.TaskDir
	existing.SweepKeys = task.SweepKeys
	existing.JobNote = task.JobNote
	// Manual retry reads the row AFTER external_id was cleared, so this
	// resets the attempt identity just like Requeue does.
	existing.ExternalID = task.ExternalID
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

// ListPending returns all pending tasks. Used by Prioritizer.
func (q *Queue) ListPending() []*Task { return q.ListByStatus(StatusPending) }

// ListRunning returns all running tasks. Used by Prioritizer.
func (q *Queue) ListRunning() []*Task { return q.ListByStatus(StatusRunning) }

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
