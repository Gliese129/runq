package scheduler

import (
	"context"
	"log/slog"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/gliese129/runq/internal/executor"
	"github.com/gliese129/runq/internal/resource"
	"github.com/gliese129/runq/internal/store"
)

// Config holds scheduler tuning parameters.
type Config struct {
	AgingThreshold  time.Duration // how long head-of-queue waits before reservation mode
	BackfillEnabled bool
	TickInterval    time.Duration // how often the scheduler loop runs
}

// DefaultConfig returns sensible defaults for a research lab.
func DefaultConfig() Config {
	return Config{
		AgingThreshold:  15 * time.Minute,
		BackfillEnabled: true,
		TickInterval:    1 * time.Second,
	}
}

// Scheduler is the core scheduling loop.
// It pulls tasks from the queue, allocates GPUs, and dispatches to the executor.
// All state transitions are persisted to store BEFORE updating the in-memory queue.
type Scheduler struct {
	cfg    Config
	queue  *Queue
	pool   resource.Allocator
	exec   *executor.Executor
	store  *store.Store
	logger *slog.Logger

	// pausedJobs tracks which jobs are paused. Scheduler skips pending tasks
	// belonging to paused jobs. Synced via PauseJob/ResumeJob API calls.
	pausedJobs map[string]bool
	pauseMu    sync.RWMutex

	// killRequested tracks tasks that were explicitly killed by the user.
	// When runTask sees a non-zero exit, it checks this set before deciding
	// retry vs killed. Prevents user-killed tasks from being auto-retried.
	killRequested map[string]bool
	killMu        sync.Mutex

	ctx    context.Context
	cancel context.CancelFunc
	wg     sync.WaitGroup
}

// New creates a Scheduler with all its dependencies.
func New(cfg Config, queue *Queue, pool resource.Allocator, exec *executor.Executor, store *store.Store, logger *slog.Logger) *Scheduler {
	ctx, cancel := context.WithCancel(context.Background())
	return &Scheduler{
		cfg:           cfg,
		queue:         queue,
		pool:          pool,
		exec:          exec,
		store:         store,
		logger:        logger,
		pausedJobs:    make(map[string]bool),
		killRequested: make(map[string]bool),
		ctx:           ctx,
		cancel:        cancel,
	}
}

// Start begins the scheduling loop in a background goroutine.
func (s *Scheduler) Start() {
	s.wg.Add(1)
	go func() {
		defer s.wg.Done()
		s.loop()
	}()
	s.logger.Info("scheduler started",
		"aging_threshold", s.cfg.AgingThreshold.String(),
		"backfill", s.cfg.BackfillEnabled,
		"tick", s.cfg.TickInterval.String(),
	)
}

// Shutdown stops the scheduling loop and waits for all running tasks to finish.
func (s *Scheduler) Shutdown() {
	s.cancel()
	s.wg.Wait()
	s.logger.Info("scheduler stopped")
}

// loop runs the scheduling tick on a fixed interval until ctx is cancelled.
func (s *Scheduler) loop() {
	ticker := time.NewTicker(s.cfg.TickInterval)
	defer ticker.Stop()

	for {
		select {
		case <-s.ctx.Done():
			return
		case <-ticker.C:
			s.tick()
		}
	}
}

// tick is one iteration of the scheduling loop.
//
// Algorithm (Reservation + Aging):
//  1. Peek the head pending task.
//  2. If it fits → dispatch it.
//  3. If it doesn't fit:
//     - Waited > AgingThreshold → reservation mode (block all, wait for GPUs).
//     - Otherwise + backfill enabled → find a smaller task that fits.
func (s *Scheduler) tick() {
	head := s.queue.Peek()
	if head == nil {
		return
	}
	// Skip paused jobs — treat as if no pending task exists for this job.
	if s.isJobPaused(head.JobID) {
		// Try backfill with a non-paused task instead.
		if s.cfg.BackfillEnabled {
			if task := s.peekSchedulableUnpaused(s.pool.FreeCount()); task != nil {
				s.dispatch(task)
			}
		}
		return
	}

	if head.GPUsNeeded <= s.pool.FreeCount() {
		s.dispatch(head)
		return
	}

	if s.shouldReserve(head) {
		s.logger.Debug("reservation mode",
			"task", head.ID, "need", head.GPUsNeeded,
			"free", s.pool.FreeCount(),
			"wait", time.Since(head.EnqueuedAt).Round(time.Second),
		)
		return
	}

	if s.cfg.BackfillEnabled {
		if task := s.peekSchedulableUnpaused(s.pool.FreeCount()); task != nil {
			s.logger.Debug("backfill", "task", task.ID, "gpus", task.GPUsNeeded, "blocked_by", head.ID)
			s.dispatch(task)
		}
	}
}

// peekSchedulableUnpaused returns the first pending task that fits in freeGPUs
// and whose parent job is not paused.
func (s *Scheduler) peekSchedulableUnpaused(freeGPUs int) *Task {
	s.pauseMu.RLock()
	paused := make(map[string]bool, len(s.pausedJobs))
	for k, v := range s.pausedJobs {
		paused[k] = v
	}
	s.pauseMu.RUnlock()
	return s.queue.PeekSchedulable(freeGPUs, paused)
}

// dispatch allocates GPUs, persists the running state, then launches the task.
// Order: allocate GPU → write DB → update queue → start process.
// If DB write fails, GPU is released and the task stays pending for next tick.
func (s *Scheduler) dispatch(task *Task) {
	gpus, err := s.pool.Allocate(task.GPUsNeeded, task.ID)
	if err != nil {
		s.logger.Warn("GPU allocation failed", "task", task.ID, "need", task.GPUsNeeded, "free", s.pool.FreeCount())
		return
	}
	task.GPUs = gpus

	// Persist to DB before updating queue.
	now := time.Now()
	dbCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := s.store.UpdateTaskStatus(dbCtx, task.ID, "running", map[string]any{
		"gpus":       gpuString(gpus),
		"started_at": now.Unix(),
	}); err != nil {
		s.logger.Error("persist running state failed, releasing GPU", "task", task.ID, "error", err)
		s.pool.Release(task.ID)
		return
	}

	if err := s.queue.MarkRunning(task.ID); err != nil {
		s.logger.Error("mark running in queue failed", "task", task.ID, "error", err)
		s.pool.Release(task.ID)
		return
	}

	s.logger.Info("task dispatched", "task", task.ID, "job", task.JobID, "gpus", gpus)
	s.wg.Add(1)
	go func() {
		defer s.wg.Done()
		s.runTask(task)
	}()
}

// runTask executes a single task and handles the result.
// GPU is always released on exit via defer.
func (s *Scheduler) runTask(task *Task) {
	defer s.pool.Release(task.ID)

	spec := executor.RunSpec{
		TaskID:     task.ID,
		Command:    task.Command,
		WorkingDir: task.WorkingDir,
		Env:        task.Env,
		GPUs:       task.GPUs,
		LogPath:    task.LogPath,
	}

	result, err := s.exec.Start(s.ctx, spec)
	if err != nil {
		s.logger.Error("task start failed", "task", task.ID, "error", err)
		s.handleFailure(task)
		return
	}

	task.PID = result.PID
	task.StartTime = result.StartTime

	// Persist PID (available only after process starts).
	s.persistFields(task.ID, map[string]any{"pid": result.PID, "start_time": result.StartTime.Unix()})

	// Check user-kill flag FIRST — even exit 0 after kill is treated as killed.
	if s.consumeKillRequest(task.ID) {
		s.completeTask(task, StatusKilled)
		s.refreshJobStatus(task.JobID)
		s.logger.Info("task killed by user", "task", task.ID)
		return
	}

	if result.ExitCode == 0 {
		s.completeTask(task, StatusSuccess)
		s.refreshJobStatus(task.JobID)
		s.logger.Info("task completed", "task", task.ID, "job", task.JobID)
		return
	}

	// Global shutdown — mark remaining running tasks as killed.
	if s.ctx.Err() != nil {
		s.completeTask(task, StatusKilled)
		s.refreshJobStatus(task.JobID)
		s.logger.Warn("task killed by shutdown", "task", task.ID)
		return
	}

	s.logger.Warn("task failed", "task", task.ID, "exit_code", result.ExitCode,
		"retry", task.RetryCount, "max_retry", task.MaxRetry)
	s.handleFailure(task)
}

// completeTask persists a terminal status to DB, then updates the queue.
func (s *Scheduler) completeTask(task *Task, status TaskStatus) {
	dbCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := s.store.UpdateTaskStatus(dbCtx, task.ID, string(status), map[string]any{
		"finished_at": time.Now().Unix(),
	}); err != nil {
		s.logger.Error("persist task completion failed", "task", task.ID, "status", status, "error", err)
	}
	if err := s.queue.Complete(task.ID, status); err != nil {
		s.logger.Error("complete in queue failed", "task", task.ID, "error", err)
	}
}

// handleFailure decides whether to retry or permanently fail a task.
// MaxRetry == 0 means unlimited retries.
func (s *Scheduler) handleFailure(task *Task) {
	canRetry := task.MaxRetry == 0 || task.RetryCount < task.MaxRetry
	if canRetry {
		nextRetry := task.RetryCount + 1
		dbCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := s.store.UpdateTaskStatus(dbCtx, task.ID, "pending", map[string]any{
			"retry_count": nextRetry,
			"gpus":        nil,
			"started_at":  nil,
			"finished_at": nil,
		}); err != nil {
			s.logger.Error("persist requeue failed", "task", task.ID, "error", err)
		}
		if err := s.queue.Requeue(task.ID); err != nil {
			s.logger.Error("requeue failed", "task", task.ID, "error", err)
		} else {
			s.logger.Info("task re-queued", "task", task.ID, "retry", nextRetry, "max_retry", task.MaxRetry)
		}
		return
	}

	s.completeTask(task, StatusFailed)
	s.refreshJobStatus(task.JobID)
	s.logger.Warn("task failed permanently", "task", task.ID, "retry", task.RetryCount, "max_retry", task.MaxRetry)
}

// persistFields updates arbitrary columns in DB. Non-critical — logs on error.
func (s *Scheduler) persistFields(taskID string, fields map[string]any) {
	dbCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	// UpdateTaskStatus with same status = only update the extra fields.
	// Use empty status trick: we read current status to avoid overwriting.
	// Simpler approach: just call a lightweight update.
	if err := s.store.UpdateTaskStatus(dbCtx, taskID, "running", fields); err != nil {
		s.logger.Warn("persist fields failed", "task", taskID, "error", err)
	}
}

// shouldReserve returns true if the head-of-queue task has waited
// longer than the aging threshold.
func (s *Scheduler) shouldReserve(head *Task) bool {
	return time.Since(head.EnqueuedAt) > s.cfg.AgingThreshold
}

// ── Job pause/resume ──

// PauseJob marks a job as paused in the scheduler's in-memory set.
// Paused jobs' pending tasks are skipped during scheduling.
// Running tasks are NOT affected (killing GPU processes doesn't free VRAM).
func (s *Scheduler) PauseJob(jobID string) {
	s.pauseMu.Lock()
	defer s.pauseMu.Unlock()
	s.pausedJobs[jobID] = true
	s.logger.Info("job paused", "job", jobID)
}

// ResumeJob removes a job from the paused set. Its pending tasks rejoin scheduling.
func (s *Scheduler) ResumeJob(jobID string) {
	s.pauseMu.Lock()
	defer s.pauseMu.Unlock()
	delete(s.pausedJobs, jobID)
	s.logger.Info("job resumed", "job", jobID)
}

// isJobPaused returns true if the given job is currently paused.
func (s *Scheduler) isJobPaused(jobID string) bool {
	s.pauseMu.RLock()
	defer s.pauseMu.RUnlock()
	return s.pausedJobs[jobID]
}

// RequestKill marks a task as user-killed. Call before Executor.Stop().
// runTask checks this flag to decide killed vs retry.
func (s *Scheduler) RequestKill(taskID string) {
	s.killMu.Lock()
	defer s.killMu.Unlock()
	s.killRequested[taskID] = true
}

// consumeKillRequest checks and clears the kill flag for a task.
func (s *Scheduler) consumeKillRequest(taskID string) bool {
	s.killMu.Lock()
	defer s.killMu.Unlock()
	if s.killRequested[taskID] {
		delete(s.killRequested, taskID)
		return true
	}
	return false
}

// refreshJobStatus checks all tasks of a job and updates the job status in DB.
// Called after every task state transition (complete, fail, requeue).
//
// Rules:
//   - any task running → job = "running"
//   - all tasks terminal (success/failed/killed) → job = "done"
//   - otherwise (some pending, none running) → keep current status
func (s *Scheduler) refreshJobStatus(jobID string) {
	ctx := context.Background()
	tasks, err := s.store.ListTasks(ctx, store.TaskFilter{JobID: jobID})
	if err != nil {
		s.logger.Error("refresh job status: list tasks failed", "job", jobID, "error", err)
		return
	}

	counts := map[string]int{"running": 0, "pending": 0, "done": 0}
	for _, t := range tasks {
		switch t.Status {
		case "running":
			counts["running"]++
		case "pending":
			counts["pending"]++
		case "success", "failed", "killed":
			counts["done"]++
		}
	}

	isStarted := (counts["running"] + counts["done"]) > 0
	isEnded := (counts["pending"] + counts["running"]) == 0

	var newStatus string
	if isEnded {
		newStatus = "done"
	} else if isStarted {
		newStatus = "running"
	} else {
		newStatus = "pending"
	}

	if err := s.store.UpdateJobStatus(ctx, jobID, newStatus); err != nil {
		s.logger.Error("refresh job status: update failed", "job", jobID, "status", newStatus, "error", err)
	}
}

// ── Helpers ──

// gpuString converts a GPU index slice to a comma-separated string (e.g. "0,1,3").
func gpuString(gpus []int) string {
	s := make([]string, len(gpus))
	for i, g := range gpus {
		s[i] = strconv.Itoa(g)
	}
	return strings.Join(s, ",")
}
