package scheduler

import (
	"context"
	"errors"
	"log/slog"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/gliese129/runq/internal/executor"
	"github.com/gliese129/runq/internal/resource"
	"github.com/gliese129/runq/internal/store"
	"github.com/gliese129/runq/internal/utils"
	"github.com/shirou/gopsutil/v4/disk"
)

// DiskConfig groups disk-freeze related tuning. These values are exposed to
// the SDK via RUNQ_* env vars — the daemon itself doesn't enforce them; the
// SDK uses them to compute NeededBytes for self-freeze decisions.
//
// `AutoThawCheckInterval` is the only purely-daemon-side field — it controls
// how often the scheduler polls disk usage on frozen mounts to issue
// SIGCONTs for tasks that can safely resume.
type DiskConfig struct {
	// SafetyFactorPercent is multiplied into upcoming-ckpt-size to compute
	// the free-bytes threshold needed for a save. 110 = 10% headroom over
	// the actual ckpt size.
	SafetyFactorPercent int `yaml:"safety_factor_percent"`

	// SafetyExtraGB is an additional absolute buffer (gigabytes) added on
	// top of the percentage. Useful for filesystems where small writes
	// (logs, tmp files) might land alongside the ckpt and starve it.
	SafetyExtraGB int `yaml:"safety_extra_gb"`

	// AutoThawCheckInterval is the auto-thaw poll cadence.
	AutoThawCheckInterval time.Duration `yaml:"auto_thaw_check_interval"`
}

// Config holds scheduler tuning parameters.
type Config struct {
	AgingThreshold     time.Duration // how long head-of-queue waits before reservation mode
	BackfillEnabled    bool
	TickInterval       time.Duration // how often the scheduler loop runs
	GPURefreshInterval time.Duration // how often to scan for external GPU usage (0 = disabled)

	// L2-C: disk-freeze tuning. autoThawLoop runs when freeze != nil (no
	// separate enable flag — the auto-thaw goroutine is part of the
	// freeze feature, not an opt-in extra).
	Disk DiskConfig
}

// DefaultConfig returns sensible defaults for a research lab.
func DefaultConfig() Config {
	return Config{
		AgingThreshold:     15 * time.Minute,
		BackfillEnabled:    true,
		TickInterval:       1 * time.Second,
		GPURefreshInterval: 30 * time.Second,
		Disk: DiskConfig{
			SafetyFactorPercent:   110, // 10% headroom over raw ckpt size
			SafetyExtraGB:         0,   // no extra absolute buffer by default
			AutoThawCheckInterval: 60 * time.Second,
		},
	}
}

// Scheduler is the core scheduling loop.
// It pulls tasks from the queue, allocates GPUs, and dispatches to the executor.
// All state transitions are persisted to store BEFORE updating the in-memory queue.
type Scheduler struct {
	cfg         Config
	queue       *Queue
	pool        resource.Allocator
	exec        *executor.Executor
	store       *store.Store
	logger      *slog.Logger
	prioritizer Prioritizer

	// pausedJobs tracks which jobs are paused. Scheduler skips pending tasks
	// belonging to paused jobs. Synced via PauseJob/ResumeJob API calls.
	pausedJobs map[string]bool
	pauseMu    sync.RWMutex

	// killRequested tracks tasks that were explicitly killed by the user.
	// When runTask sees a non-zero exit, it checks this set before deciding
	// retry vs killed. Prevents user-killed tasks from being auto-retried.
	killRequested map[string]bool
	killMu        sync.Mutex

	// L2-C: optional disk-freeze state machine. nil means freeze disabled.
	// When set, tick() should short-circuit while freeze.IsFrozen(), and
	// runTask should call freeze.RemoveTask on every exit so auto-thaw fires
	// when the last frozen task dies.
	freeze *FreezeState

	// L2-C: daemon socket path injected as RUNQ_SOCKET_PATH into task env.
	// Reserved for stage 2+ SDK control-plane calls.
	socketPath string

	ctx    context.Context
	cancel context.CancelFunc
	wg     sync.WaitGroup
}

// FreezeState returns the configured FreezeState, or nil if none was wired.
// Exposed for the API thaw handler and tests.
func (s *Scheduler) FreezeState() *FreezeState {
	return s.freeze
}

// buildTaskEnv merges task.Env with the RUNQ_* environment variables the SDK
// expects to find. RUNQ_* are written last so they win against any user-set
// keys with the same name (RUNQ_* are an internal contract; user overrides
// of e.g. RUNQ_TASK_ID would silently break the SDK).
//
// Non-RUNQ user env (e.g. WANDB_API_KEY) is preserved as-is.
//
// RUNQ_WANDB_CONFIG_FILE is only injected when the file exists, so that the
// SDK can use the env's presence/absence as a binary signal for "wandb is
// configured for this task" without needing to stat the path itself.
func (s *Scheduler) buildTaskEnv(task *Task) map[string]string {
	env := make(map[string]string, len(task.Env)+10)
	for k, v := range task.Env {
		env[k] = v
	}
	env["RUNQ_TASK_ID"] = task.ID
	env["RUNQ_JOB_ID"] = task.JobID
	env["RUNQ_PROJECT_NAME"] = task.ProjectName
	if task.TaskDir != "" {
		env["RUNQ_TASK_DIR"] = task.TaskDir
		env["RUNQ_PARAMS_FILE"] = filepath.Join(task.TaskDir, "params.json")
		env["RUNQ_METRICS_FILE"] = filepath.Join(task.TaskDir, "metrics.jsonl")
		env["RUNQ_CHECKPOINT_DIR"] = filepath.Join(task.TaskDir, "checkpoints")

		wandbCfg := filepath.Join(task.TaskDir, "wandb_config.json")
		if _, err := os.Stat(wandbCfg); err == nil {
			env["RUNQ_WANDB_CONFIG_FILE"] = wandbCfg
		}
	}
	if s.socketPath != "" {
		env["RUNQ_SOCKET_PATH"] = s.socketPath
	}

	// SDK contract for self-freeze: SDK reads these and computes
	//   needed = upcoming_ckpt_size × percent / 100 + extra_gb × 1GiB
	// Always injected (never optional) so the SDK doesn't need fallback
	// defaults — if the field is missing it's a daemon-side bug.
	env["RUNQ_SAFETY_FACTOR_PERCENT"] = strconv.Itoa(s.cfg.Disk.SafetyFactorPercent)
	env["RUNQ_SAFETY_EXTRA_GB"] = strconv.Itoa(s.cfg.Disk.SafetyExtraGB)

	return env
}

// New creates a Scheduler with all its dependencies.
// If prioritizer is nil, defaults to FIFO.
//
// socketPath is injected into each task's env as RUNQ_SOCKET_PATH (empty
// string disables injection). freeze may be nil to disable the disk-freeze
// feature; when set, the same instance should be wired into api.Deps.Freeze
// so the thaw endpoint operates on it.
func New(
	cfg Config,
	queue *Queue,
	pool resource.Allocator,
	exec *executor.Executor,
	st *store.Store,
	logger *slog.Logger,
	prioritizer Prioritizer,
	socketPath string,
	freeze *FreezeState,
) *Scheduler {
	if prioritizer == nil {
		prioritizer = FIFOPrioritizer{}
	}
	ctx, cancel := context.WithCancel(context.Background())
	return &Scheduler{
		cfg:           cfg,
		queue:         queue,
		pool:          pool,
		exec:          exec,
		store:         st,
		logger:        logger,
		prioritizer:   prioritizer,
		pausedJobs:    make(map[string]bool),
		killRequested: make(map[string]bool),
		freeze:        freeze,
		socketPath:    socketPath,
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
	if s.cfg.GPURefreshInterval > 0 {
		s.wg.Add(1)
		go func() {
			defer s.wg.Done()
			s.gpuRefreshLoop()
		}()
	}
	// autoThawLoop is part of the freeze feature; launch whenever freeze
	// is wired. No separate "enable" knob — if you have freeze you need
	// auto-thaw to ever recover without manual intervention.
	if s.freeze != nil {
		s.wg.Add(1)
		go func() {
			defer s.wg.Done()
			s.autoThawLoop()
		}()
	}
	s.logger.Info("scheduler started",
		"strategy", s.prioritizer.Name(),
		"aging_threshold", s.cfg.AgingThreshold.String(),
		"backfill", s.cfg.BackfillEnabled,
		"tick", s.cfg.TickInterval.String(),
		"gpu_refresh", s.cfg.GPURefreshInterval.String(),
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

// autoThawLoop periodically tries to release frozen tasks whose mount has
// recovered enough free space for that task's NeededBytes. Per-task
// threshold is set by the SDK at freeze time (FrozenTask.NeededBytes), so
// freeze and thaw decisions are physically symmetric.
//
// ThawTasks handles per-mount aggregation internally — we just pass it
// all frozen IDs and let it figure out which mounts to query.
func (s *Scheduler) autoThawLoop() {
	interval := s.cfg.Disk.AutoThawCheckInterval
	if interval <= 0 {
		interval = 60 * time.Second
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	// Wrap disk.Usage once; reused on every tick.
	freeFn := func(mount string) (uint64, error) {
		u, err := disk.Usage(mount)
		if err != nil {
			return 0, err
		}
		return u.Free, nil
	}

	for {
		select {
		case <-s.ctx.Done():
			return
		case <-ticker.C:
			if s.freeze == nil || !s.freeze.IsFrozen() {
				continue
			}
			result := s.freeze.ThawTasks(s.freeze.FrozenTaskIDs(), freeFn)
			if len(result.Thawed) > 0 {
				s.logger.Info("autoThaw released",
					"tasks", result.Thawed,
					"still_blocked", len(result.Blocked))
			}
		}
	}
}

// tick is one iteration of the scheduling loop.
//
// Algorithm (Prioritizer + Reservation + Backfill):
//  1. Ask Prioritizer to rank all pending tasks.
//  2. Find head = highest-priority non-paused task.
//  3. Head fits → dispatch it immediately.
//  4. Head doesn't fit + waited > AgingThreshold → reservation mode (block all).
//  5. Head doesn't fit + backfill enabled → dispatch the first smaller task that fits.
func (s *Scheduler) tick() {
	pending := s.queue.ListPending()
	if len(pending) == 0 {
		return
	}

	running := s.queue.ListRunning()
	sctx := ScheduleContext{
		Pending:  pending,
		Running:  running,
		FreeGPUs: s.pool.FreeCount(),
		Now:      time.Now(),
	}
	ranked := s.prioritizer.Prioritize(sctx)

	// check if tasks need to freeze
	var frozenJobs, frozenMounts map[string]struct{}
	if s.freeze != nil {
		frozenJobs = s.freeze.FrozenJobs()
		frozenMounts = s.freeze.FrozenMounts()
	}
	parts, _ := utils.LoadMountTable()

	// Build a quick task lookup from pending list.
	taskMap := make(map[string]*Task, len(pending))
	for _, t := range pending {
		taskMap[t.ID] = t
	}

	// Find head: the highest-priority non-paused, non-frozen-sibling,
	// non-frozen-mount task. `ranked` is in priority-desc order; first
	// match wins.
	var head *Task
	for _, p := range ranked {
		t := taskMap[p.TaskID]
		if t == nil || s.isJobPaused(t.JobID) {
			continue
		}
		// Sibling task already frozen on this job → skip; the pending
		// task would just self-freeze on its own first save.
		if _, ok := frozenJobs[t.JobID]; ok {
			continue
		}
		// Mount has a frozen task on it → skip; dispatching to the same
		// stressed disk just compounds the problem.
		if mount := utils.MountOf(t.CheckpointDir, parts); mount != "" {
			if _, ok := frozenMounts[mount]; ok {
				continue
			}
		}
		head = t
		break
	}
	if head == nil {
		return // all pending tasks are paused
	}

	// Head fits → dispatch it.
	if head.GPUsNeeded <= sctx.FreeGPUs {
		s.dispatch(head)
		return
	}

	// Head doesn't fit — check aging / reservation.
	if s.shouldReserve(head) {
		s.logger.Debug("reservation mode",
			"task", head.ID, "need", head.GPUsNeeded,
			"free", sctx.FreeGPUs,
			"wait", time.Since(head.EnqueuedAt).Round(time.Second),
		)
		return
	}

	// Backfill: find a smaller task that fits.
	if s.cfg.BackfillEnabled {
		for _, p := range ranked {
			t := taskMap[p.TaskID]
			if t == nil || t.ID == head.ID || s.isJobPaused(t.JobID) {
				continue
			}
			if t.GPUsNeeded <= sctx.FreeGPUs {
				s.logger.Debug("backfill", "task", t.ID, "gpus", t.GPUsNeeded, "blocked_by", head.ID)
				s.dispatch(t)
				return
			}
		}
	}
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

	// Set wall-clock start time AFTER successful DB write.
	// FairShare reads task.StartTime from the queue to compute running occupation;
	// waiting until Executor.Start() returns (blocking) would leave it as zero value.
	// Setting it before DB write would leave stale state if the write fails.
	task.StartTime = now

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
	defer func() {
		if s.freeze != nil {
			s.freeze.RemoveTask(task.ID)
		}
		s.reapMetrics(task)
		s.checkGPUResidual(task)
		s.pool.Release(task.ID)
	}()

	spec := executor.RunSpec{
		TaskID:     task.ID,
		Command:    task.Command,
		WorkingDir: task.WorkingDir,
		Env:        s.buildTaskEnv(task),
		GPUs:       task.GPUs,
		LogPath:    task.LogPath,
	}

	ctx := s.ctx
	var cancel context.CancelFunc
	if task.Timeout > 0 {
		ctx, cancel = context.WithTimeout(s.ctx, time.Second*time.Duration(task.Timeout))
		defer cancel()
	}
	result, err := s.exec.Start(ctx, spec)
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
		s.RefreshJobStatus(task.JobID)
		s.logger.Info("task killed by user", "task", task.ID)
		return
	}

	if result.ExitCode == 0 {
		s.completeTask(task, StatusSuccess)
		s.RefreshJobStatus(task.JobID)
		s.logger.Info("task completed", "task", task.ID, "job", task.JobID)
		return
	}

	if task.Timeout > 0 && errors.Is(ctx.Err(), context.DeadlineExceeded) {
		s.completeTask(task, StatusKilled)
		s.RefreshJobStatus(task.JobID)
		s.logger.Warn("task timed out", "task", task.ID, "timeout", task.Timeout)
		return
	}

	// Global shutdown — mark remaining running tasks as killed.
	if s.ctx.Err() != nil {
		s.completeTask(task, StatusKilled)
		s.RefreshJobStatus(task.JobID)
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

// reapMetrics ingests a finished task's metrics.jsonl into the store
// (metric and checkpoint rows). No freeze logic — freeze is SDK-driven
// via /api/internal/freeze-self; daemon never decides based on reap.
//
// Best-effort: errors are logged, never propagated. Called from runTask's
// defer and from MonitorReattached's defer, so any panic here would
// orphan a task. Keep this function dumb.
func (s *Scheduler) reapMetrics(task *Task) {
	result, err := ReapTaskOutputs(s.ctx, s.store, task)
	if err != nil {
		s.logger.Warn("reap failed", "task", task.ID, "error", err)
		return
	}
	s.logger.Info("reap data",
		"task", task.ID,
		"metric", result.MetricCount,
		"ckpt", result.CheckpointCount)
}

// MonitorReattached monitors a reattached (daemon-restart) task until it exits.
// Follows the same lifecycle as runTask: GPU residual check → release GPU →
// complete task → refresh job status. Called from daemon.go after Reclaim.
func (s *Scheduler) MonitorReattached(task *Task, ch <-chan executor.ReattachResult) {
	s.wg.Add(1)
	go func() {
		defer s.wg.Done()
		defer func() {
			if s.freeze != nil {
				s.freeze.RemoveTask(task.ID)
			}
			s.reapMetrics(task)
			s.checkGPUResidual(task)
			s.pool.Release(task.ID)
		}()

		res, ok := <-ch
		if !ok {
			return // channel closed without result (shouldn't happen)
		}

		// Check user-kill flag first, same as runTask.
		if s.consumeKillRequest(task.ID) || res.Killed {
			s.completeTask(task, StatusKilled)
		} else {
			// Signal 0 polling can't get real exit code.
			// Treat non-killed exit as failed; user can inspect logs and retry.
			s.completeTask(task, StatusFailed)
		}
		s.RefreshJobStatus(task.JobID)
		s.logger.Info("reattached task exited", "task", task.ID, "status", task.Status)
	}()
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
	s.RefreshJobStatus(task.JobID)
	s.logger.Warn("task failed permanently", "task", task.ID, "retry", task.RetryCount, "max_retry", task.MaxRetry)
}

// persistFields updates arbitrary columns on a running task in DB. Non-critical — logs on error.
// Reuses UpdateTaskStatus with status="running" so only the extra fields change.
func (s *Scheduler) persistFields(taskID string, fields map[string]any) {
	dbCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
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

// RefreshJobStatus checks all tasks of a job and updates the job status in DB.
// Called after every task state transition (complete, fail, requeue).
//
// Rules:
//   - any task running → job = "running"
//   - all tasks terminal (success/failed/killed) → job = "done"
//   - otherwise (some pending, none running) → keep current status
func (s *Scheduler) RefreshJobStatus(jobID string) {
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

// checkGPUResidual uses nvidia-smi pmon to detect processes still occupying
// the GPUs that were assigned to a task after it exited. Logs a warning for
// each residual process found. Best-effort: errors are logged, never fatal.
func (s *Scheduler) checkGPUResidual(task *Task) {
	if len(task.GPUs) == 0 {
		return
	}
	procs, err := resource.CheckResidualProcesses(task.GPUs)
	if err != nil {
		s.logger.Warn("GPU residual check failed", "task", task.ID, "error", err)
		return
	}
	for _, p := range procs {
		s.logger.Warn("GPU residual process detected after task exit",
			"task", task.ID, "gpu", p.GPUIndex,
			"residual_pid", p.PID, "mem_mb", p.MemMB, "command", p.Command,
		)
	}
}

// gpuRefreshLoop periodically scans for external GPU usage and updates the pool.
func (s *Scheduler) gpuRefreshLoop() {
	ticker := time.NewTicker(s.cfg.GPURefreshInterval)
	defer ticker.Stop()

	for {
		select {
		case <-s.ctx.Done():
			return
		case <-ticker.C:
			s.refreshGPUUsage()
		}
	}
}

// refreshGPUUsage scans all GPUs via pmon and marks externally-occupied ones.
func (s *Scheduler) refreshGPUUsage() {
	pool, ok := s.pool.(*resource.GPUPool)
	if !ok {
		return // MockAllocator — skip
	}

	status := pool.Status()
	gpuIndices := make([]int, len(status))
	for i, g := range status {
		gpuIndices[i] = g.Index
	}

	procs, err := resource.CheckResidualProcesses(gpuIndices)
	if err != nil {
		s.logger.Debug("GPU refresh: pmon failed", "error", err)
		return
	}

	// Build set of task IDs managed by runq.
	managedIDs := make(map[string]bool)
	for _, t := range s.queue.ListRunning() {
		managedIDs[t.ID] = true
	}

	blocked, unblocked := pool.RefreshExternalUsage(procs, managedIDs)
	if len(blocked) > 0 {
		s.logger.Warn("GPUs blocked by external processes", "gpus", blocked)
	}
	if len(unblocked) > 0 {
		s.logger.Info("GPUs unblocked (external processes gone)", "gpus", unblocked)
	}
}

// gpuString converts a GPU index slice to a comma-separated string (e.g. "0,1,3").
func gpuString(gpus []int) string {
	s := make([]string, len(gpus))
	for i, g := range gpus {
		s[i] = strconv.Itoa(g)
	}
	return strings.Join(s, ",")
}
