package scheduler

import (
	"context"
	"errors"
	"time"

	"github.com/gliese129/runq/internal/executor"
	"github.com/gliese129/runq/internal/ingest"
	"github.com/gliese129/runq/internal/workspace"
)

// buildTaskEnv merges task.Env with the RUNQ_* environment variables the SDK
// expects to find. The shared RUNQ_* contract comes from workspace.BaseEnv (the
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
	for k, v := range workspace.BaseEnv(
		workspace.Identity{
			TaskID:    task.ID,
			JobID:     task.JobID,
			Project:   task.ProjectName,
			TaskDir:   task.TaskDir,
			SweepKeys: task.SweepKeys,
			JobNote:   task.JobNote,
		},
		workspace.Safety{
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
//
// Launch failures split three ways (the taxonomy matters):
//   - TRANSIENT (scheduler unreachable): not the task's failure — back to
//     pending, no retry budget consumed, next tick retries.
//   - UNTRACKED (submitted, id lost): leave in flight; reconcile heals via
//     status.json. Never retry (double submission).
//   - REJECTED (scheduler said no, exit != 0): deterministic — permanent
//     failure with the scheduler's own words in the error.
func (s *Scheduler) launchAsync(task *Task) {
	_, err := s.launcher.Launch(s.ctx, task, nil, nil)
	switch {
	case err == nil:
		s.logger.Info("task submitted to remote scheduler", "task", task.ID, "job", task.JobID)
	case errors.Is(err, ErrLaunchTransient):
		s.logger.Warn("scheduler unreachable, task returned to queue",
			"task", task.ID, "error", err)
		s.requeueTransient(task)
	case errors.Is(err, ErrLaunchUntracked):
		s.logger.Error("task submitted but untracked", "task", task.ID, "error", err)
	default:
		s.logger.Error("submission rejected", "task", task.ID, "error", err)
		s.FinishTaskNoRetry(task, map[string]any{"status_source": "submit"})
	}
}

// requeueTransient puts an unsupervised task back to pending after a launch
// that never reached the scheduler. Under finishMu so it can't interleave
// with a verdict delivery; releases the submission slot; the DB row stayed
// "pending" throughout (unsupervised dispatch doesn't write DB), so there is
// nothing to reset there.
func (s *Scheduler) requeueTransient(task *Task) {
	s.finishMu.Lock()
	defer s.finishMu.Unlock()
	if err := s.queue.RequeueTransient(task.ID); err != nil {
		s.logger.Error("transient requeue failed", "task", task.ID, "error", err)
	}
	s.pool.Release(task.ID)
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
//
// Unsupervised concurrency contract: sensors (marker scan, probe align, kill
// paths) may deliver verdicts concurrently, and a verdict computed from data
// read BEFORE a retry requeued the task is stale. finishMu serializes the
// check-then-act, and the queue's in-memory state is the authority on which
// attempt is current: verdicts are accepted only for tasks whose queue entry
// is running (in flight), plus the pending→killed case (user cancelling a
// task that was never handed off). Everything else is dropped.
func (s *Scheduler) FinishTask(task *Task, status TaskStatus, extra map[string]any) {
	s.finishTaskInner(task, status, extra, true)
}

// FinishTaskNoRetry records a permanent failure that must NOT enter the
// retry policy — deterministic scheduler rejections (submission ran, exit
// non-zero): retrying replays the same answer and, with max_retry=0
// (unlimited), becomes a log storm.
func (s *Scheduler) FinishTaskNoRetry(task *Task, extra map[string]any) {
	s.finishTaskInner(task, StatusFailed, extra, false)
}

func (s *Scheduler) finishTaskInner(task *Task, status TaskStatus, extra map[string]any, allowRetry bool) {
	if !s.launcher.Supervised() {
		s.finishMu.Lock()
		defer s.finishMu.Unlock()

		qt := s.queue.Get(task.ID)
		switch {
		case qt == nil:
			// Unmanaged row: not in this lane's queue (should not happen
			// while the daemon runs — restore covers all non-terminal rows —
			// but a legacy writer or edge restore gap must not lose wrapper
			// facts). Persist the terminal state directly, WITHOUT retry (no
			// queue entry to relaunch) and without slot bookkeeping (never
			// held one). Failed maps to a permanent failure.
			s.logger.Warn("finish verdict for unmanaged task: persisting without lifecycle",
				"task", task.ID, "status", string(status))
			if status != StatusSuccess && status != StatusKilled {
				status = StatusFailed
			}
			s.completeTask(task, status, extra)
			s.RefreshJobStatus(task.JobID)
			return
		case qt.Status == StatusRunning:
			// In flight — accept. Attempt-level staleness ("verdict computed
			// against attempt N delivered after attempt N+1 re-entered
			// running") is prevented BY CONSTRUCTION, not detected here: all
			// verdict producers and manual retry serialize on the target's
			// lifecycle lock (remote.Backend.lifecycleMu), and a requeue only
			// happens inside a verdict delivery — so no verdict can ever be
			// computed before a requeue and delivered after it. This gate
			// remains as a cheap backstop for queue-state mismatches only.
		case qt.Status == StatusPending && status == StatusKilled:
			// Never handed off, user cancelled — accept.
		default:
			// Stale verdict: the task was requeued (new attempt pending) or
			// already terminal. A previous attempt's failure must not
			// clobber the new attempt or double-count a retry.
			s.logger.Debug("finish verdict dropped: stale",
				"task", task.ID, "queue_status", string(qt.Status), "verdict", string(status))
			return
		}
		defer s.pool.Release(task.ID)
	}
	switch {
	case status == StatusSuccess || status == StatusKilled:
		s.completeTask(task, status, extra)
		s.RefreshJobStatus(task.JobID)
	case allowRetry:
		s.handleFailure(task, extra)
	default:
		s.completeTask(task, StatusFailed, extra)
		s.RefreshJobStatus(task.JobID)
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
	// final=false on purpose: this runs at each ATTEMPT's end, and a retry
	// rewrites metrics.jsonl — the (size,offset) mark handles both the
	// incremental append and the shrink-rebuild; freezing would block the
	// next attempt's ingest.
	result, err := ingest.ReapIncremental(s.ctx, s.store, ingest.Target{
		TaskID: task.ID,
		JobID:  task.JobID,
		Dir:    task.TaskDir,
	}, false)
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
