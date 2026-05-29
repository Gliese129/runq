package scheduler

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"time"

	"github.com/gliese129/runq/internal/executor"
	"github.com/gliese129/runq/internal/ingest"
)

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
		OnStart: func(result executor.Result) {
			task.PID = result.PID
			task.StartTime = result.StartTime
			fields := map[string]any{"pid": result.PID}
			if result.StartTime.IsZero() {
				fields["start_time"] = nil
			} else {
				fields["start_time"] = result.StartTime.Unix()
			}
			s.persistFields(task.ID, fields)
		},
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
	result, err := ingest.ReapOutputs(s.ctx, s.store, ingest.Target{
		TaskID: task.ID,
		JobID:  task.JobID,
		Dir:    task.TaskDir,
	})
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
