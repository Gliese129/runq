package scheduler

import (
	"context"
	"log/slog"
	"sync"
	"time"

	"github.com/gliese129/runq/internal/executor"
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
		AgingThreshold:  1 * time.Hour,
		BackfillEnabled: true,
		TickInterval:    1 * time.Second,
	}
}

// Scheduler is the core scheduling loop.
// It pulls tasks from the queue, allocates GPUs, and dispatches to the executor.
type Scheduler struct {
	cfg    Config
	queue  *Queue
	pool   *GPUPool
	exec   *executor.Executor
	logger *slog.Logger

	ctx    context.Context
	cancel context.CancelFunc
	wg     sync.WaitGroup
}

// New creates a Scheduler with all its dependencies.
func New(cfg Config, queue *Queue, pool *GPUPool, exec *executor.Executor, logger *slog.Logger) *Scheduler {
	ctx, cancel := context.WithCancel(context.Background())
	return &Scheduler{
		cfg:    cfg,
		queue:  queue,
		pool:   pool,
		exec:   exec,
		logger: logger,
		ctx:    ctx,
		cancel: cancel,
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
		"aging_threshold", s.cfg.AgingThreshold,
		"backfill", s.cfg.BackfillEnabled,
		"tick", s.cfg.TickInterval,
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

	// Head fits → dispatch.
	if head.GPUsNeeded <= s.pool.FreeCount() {
		s.dispatch(head)
		return
	}

	// Head doesn't fit → check reservation.
	if s.shouldReserve(head) {
		s.logger.Debug("reservation mode: waiting for GPUs",
			"task_id", head.ID,
			"gpus_needed", head.GPUsNeeded,
			"gpus_free", s.pool.FreeCount(),
			"waiting", time.Since(head.EnqueuedAt).Round(time.Second),
		)
		return
	}

	// Backfill: find a smaller pending task that fits.
	if s.cfg.BackfillEnabled {
		task := s.queue.PeekSchedulable(s.pool.FreeCount())
		if task != nil {
			s.logger.Debug("backfilling",
				"task_id", task.ID,
				"gpus_needed", task.GPUsNeeded,
				"head_blocked", head.ID,
			)
			s.dispatch(task)
		}
	}
}

// dispatch allocates GPUs for a task, marks it running, and launches it.
func (s *Scheduler) dispatch(task *Task) {
	gpus, err := s.pool.Allocate(task.GPUsNeeded, task.ID)
	if err != nil {
		s.logger.Warn("GPU allocation failed",
			"task_id", task.ID,
			"gpus_needed", task.GPUsNeeded,
			"gpus_free", s.pool.FreeCount(),
		)
		return
	}
	task.GPUs = gpus

	if err := s.queue.MarkRunning(task.ID); err != nil {
		s.logger.Error("failed to mark task running", "task_id", task.ID, "error", err)
		s.pool.Release(task.ID)
		return
	}

	s.logger.Info("task dispatched", "task_id", task.ID, "job_id", task.JobID, "gpus", gpus)
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
		s.logger.Error("task start failed", "task_id", task.ID, "error", err)
		s.handleFailure(task)
		return
	}

	task.PID = result.PID
	task.StartTime = result.StartTime

	// Success.
	if result.ExitCode == 0 {
		if err := s.queue.Complete(task.ID, StatusSuccess); err != nil {
			s.logger.Error("failed to mark task success", "task_id", task.ID, "error", err)
		} else {
			s.logger.Info("task completed", "task_id", task.ID, "job_id", task.JobID)
		}
		return
	}

	// Killed (context cancelled by Stop or Shutdown).
	if s.ctx.Err() != nil {
		if err := s.queue.Complete(task.ID, StatusKilled); err != nil {
			s.logger.Error("failed to mark task killed", "task_id", task.ID, "error", err)
		} else {
			s.logger.Warn("task killed", "task_id", task.ID)
		}
		return
	}

	// Non-zero exit → retry or fail.
	s.logger.Warn("task exited with error",
		"task_id", task.ID,
		"exit_code", result.ExitCode,
		"retry_count", task.RetryCount,
		"max_retry", task.MaxRetry,
	)
	s.handleFailure(task)
}

// handleFailure decides whether to retry or permanently fail a task.
// MaxRetry == 0 means unlimited retries.
func (s *Scheduler) handleFailure(task *Task) {
	canRetry := task.MaxRetry == 0 || task.RetryCount < task.MaxRetry
	if canRetry {
		if err := s.queue.Requeue(task.ID); err != nil {
			s.logger.Error("failed to requeue task", "task_id", task.ID, "error", err)
		} else {
			s.logger.Info("task requeued for retry",
				"task_id", task.ID,
				"retry", task.RetryCount,
				"max_retry", task.MaxRetry,
			)
		}
		return
	}

	if err := s.queue.Complete(task.ID, StatusFailed); err != nil {
		s.logger.Error("failed to mark task failed", "task_id", task.ID, "error", err)
	} else {
		s.logger.Warn("task failed permanently",
			"task_id", task.ID,
			"retry", task.RetryCount,
			"max_retry", task.MaxRetry,
		)
	}
}

// shouldReserve returns true if the head-of-queue task has been waiting
// longer than the aging threshold.
func (s *Scheduler) shouldReserve(head *Task) bool {
	return time.Since(head.EnqueuedAt) > s.cfg.AgingThreshold
}
