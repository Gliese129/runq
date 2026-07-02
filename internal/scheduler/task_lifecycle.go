package scheduler

import (
	"context"
	"errors"
	"time"

	"github.com/gliese129/runq/internal/executor"
	"github.com/gliese129/runq/internal/ingest"
	"github.com/gliese129/runq/internal/runqenv"
)

// buildTaskEnv merges task.Env with the RUNQ_* environment variables the SDK
// expects to find. The shared RUNQ_* contract comes from runqenv.Base (the
// single source of truth, also used by the HPC wrapper); the daemon then layers
// on RUNQ_SOCKET_PATH. RUNQ_* are written last so they win against any user-set
// keys with the same name (RUNQ_* are an internal contract; user overrides of
// e.g. RUNQ_TASK_ID would silently break the SDK).
//
// Non-RUNQ user env (e.g. WANDB_API_KEY) is preserved as-is.
func (s *Scheduler) buildTaskEnv(task *Task) map[string]string {
	env := make(map[string]string, len(task.Env)+10)
	for k, v := range task.Env {
		env[k] = v
	}
	for k, v := range runqenv.Base(
		runqenv.Identity{
			TaskID:    task.ID,
			JobID:     task.JobID,
			Project:   task.ProjectName,
			TaskDir:   task.TaskDir,
			SweepKeys: task.SweepKeys,
			JobNote:   task.JobNote,
		},
		runqenv.Safety{
			FactorPercent: s.cfg.Disk.SafetyFactorPercent,
			ExtraGB:       s.cfg.Disk.SafetyExtraGB,
		},
	) {
		env[k] = v
	}
	// RUNQ_SOCKET_PATH is daemon-only (the SDK uses it to reach the daemon for
	// self-freeze). The HPC wrapper omits it and sets RUNQ_NO_DAEMON instead.
	if s.socketPath != "" {
		env["RUNQ_SOCKET_PATH"] = s.socketPath
	}

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

	ctx := s.ctx
	var cancel context.CancelFunc
	if task.Timeout > 0 {
		ctx, cancel = context.WithTimeout(s.ctx, time.Second*time.Duration(task.Timeout))
		defer cancel()
	}
	result, err := s.launcher.Launch(ctx, task, s.buildTaskEnv(task), func(si StartInfo) {
		task.PID = si.PID
		task.StartTime = si.StartTime
		fields := map[string]any{"pid": si.PID}
		if si.StartTime.IsZero() {
			fields["start_time"] = nil
		} else {
			fields["start_time"] = si.StartTime.Unix()
		}
		s.persistFields(task.ID, fields)
	})
	if err != nil {
		s.logger.Error("task start failed", "task", task.ID, "error", err)
		s.FinishTask(task, StatusFailed, nil)
		return
	}

	// Verdict order matters: user-kill flag FIRST — even exit 0 after kill is
	// treated as killed.
	switch {
	case s.consumeKillRequest(task.ID):
		s.FinishTask(task, StatusKilled, nil)
		s.logger.Info("task killed by user", "task", task.ID)
	case result.ExitCode == 0:
		s.FinishTask(task, StatusSuccess, nil)
		s.logger.Info("task completed", "task", task.ID, "job", task.JobID)
	case task.Timeout > 0 && errors.Is(ctx.Err(), context.DeadlineExceeded):
		s.FinishTask(task, StatusKilled, nil)
		s.logger.Warn("task timed out", "task", task.ID, "timeout", task.Timeout)
	case s.ctx.Err() != nil:
		// Global shutdown — mark remaining running tasks as killed.
		s.FinishTask(task, StatusKilled, nil)
		s.logger.Warn("task killed by shutdown", "task", task.ID)
	default:
		s.logger.Warn("task failed", "task", task.ID, "exit_code", result.ExitCode,
			"retry", task.RetryCount, "max_retry", task.MaxRetry)
		s.FinishTask(task, StatusFailed, nil)
	}
}

// launchAsync hands a task to an unsupervised launcher and returns. There is
// no verdict to evaluate here — the terminal state arrives later through
// FinishTask, fed by reconcile (.done marker / scheduler probe).
func (s *Scheduler) launchAsync(task *Task) {
	_, err := s.launcher.Launch(s.ctx, task, nil, nil)
	if err != nil {
		if errors.Is(err, ErrLaunchUntracked) {
			// Handed off but the external id is unknown: the remote job may be
			// running untracked. Leave the task in flight — reconcile heals it
			// via status.json if it runs. Do NOT retry (double submission).
			s.logger.Error("task submitted but untracked", "task", task.ID, "error", err)
			return
		}
		s.logger.Error("remote submit failed", "task", task.ID, "error", err)
		s.FinishTask(task, StatusFailed, map[string]any{"status_source": "submit"})
		return
	}
	s.logger.Info("task submitted to remote scheduler", "task", task.ID, "job", task.JobID)
}

// FinishTask drives a task to a terminal state through the shared lifecycle.
// StatusSuccess / StatusKilled persist immediately; StatusFailed goes through
// handleFailure, which requeues (retry) or permanently fails the task.
//
// extra carries additional DB fields to persist with the terminal transition
// (e.g. status_source, native_state, a wrapper-reported finished_at); nil is
// fine. Keys in extra override the defaults.
//
// This is the single funnel for task completion: supervised launchers reach it
// from runTask after the process exits; unsupervised launchers reach it from
// reconcile when a terminal signal arrives from the remote scheduler. For the
// unsupervised lane this is also where the submission slot is released.
func (s *Scheduler) FinishTask(task *Task, status TaskStatus, extra map[string]any) {
	if !s.launcher.Supervised() {
		defer s.pool.Release(task.ID)
	}
	switch status {
	case StatusSuccess, StatusKilled:
		s.completeTask(task, status, extra)
		s.RefreshJobStatus(task.JobID)
	default:
		s.handleFailure(task, extra)
	}
}

// completeTask persists a terminal status to DB, then updates the queue.
// extra fields are merged over the defaults (so a wrapper-reported
// finished_at wins over the wall clock).
func (s *Scheduler) completeTask(task *Task, status TaskStatus, extra map[string]any) {
	dbCtx, cancel := context.WithTimeout(s.ctx, 5*time.Second)
	defer cancel()
	fields := map[string]any{"finished_at": time.Now().Unix()}
	for k, v := range extra {
		fields[k] = v
	}
	if err := s.store.UpdateTaskStatus(dbCtx, task.ID, string(status), fields); err != nil {
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
			s.completeTask(task, StatusKilled, nil)
		} else {
			// Signal 0 polling can't get real exit code.
			// Treat non-killed exit as failed; user can inspect logs and retry.
			s.completeTask(task, StatusFailed, nil)
		}
		s.RefreshJobStatus(task.JobID)
		s.logger.Info("reattached task exited", "task", task.ID, "status", task.Status)
	}()
}

// handleFailure decides whether to retry or permanently fail a task.
// MaxRetry == 0 means unlimited retries. extra fields (may be nil) are
// persisted only with the permanent-failure transition; a requeue resets
// state instead.
func (s *Scheduler) handleFailure(task *Task, extra map[string]any) {
	canRetry := task.MaxRetry == 0 || task.RetryCount < task.MaxRetry
	if canRetry {
		nextRetry := task.RetryCount + 1
		dbCtx, cancel := context.WithTimeout(s.ctx, 5*time.Second)
		defer cancel()
		if err := s.store.UpdateTaskStatus(dbCtx, task.ID, "pending", map[string]any{
			"retry_count": nextRetry,
			"gpus":        nil,
			"started_at":  nil,
			"finished_at": nil,
			// Clear the external scheduler id: on the remote lane a requeue
			// means a NEW submission — probing the old id would track a dead
			// cluster job. Harmless no-op for local tasks (column unused).
			"external_id": nil,
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

	s.completeTask(task, StatusFailed, extra)
	s.RefreshJobStatus(task.JobID)
	s.logger.Warn("task failed permanently", "task", task.ID, "retry", task.RetryCount, "max_retry", task.MaxRetry)
}

// persistFields updates arbitrary columns on a running task in DB. Non-critical — logs on error.
// Reuses UpdateTaskStatus with status="running" so only the extra fields change.
func (s *Scheduler) persistFields(taskID string, fields map[string]any) {
	dbCtx, cancel := context.WithTimeout(s.ctx, 5*time.Second)
	defer cancel()
	if err := s.store.UpdateTaskStatus(dbCtx, taskID, "running", fields); err != nil {
		s.logger.Warn("persist fields failed", "task", taskID, "error", err)
	}
}
