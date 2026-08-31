package scheduler

import (
	"context"
	"fmt"
	"time"

	"github.com/gliese129/runq-lab/internal/store"
)

// ── Job pause/resume ──────────────────────────────────────────────────────

// PauseJob marks a job as paused in the scheduler's in-memory set.
// Paused jobs' pending tasks are skipped during scheduling.
// In-flight tasks are not affected; pause only gates new submissions.
func (s *Scheduler) PauseJob(jobID string) {
	s.dispatchMu.Lock()
	defer s.dispatchMu.Unlock()
	s.pauseMu.Lock()
	defer s.pauseMu.Unlock()
	s.pausedJobs[jobID] = true
	s.logger.Info("job paused", "job", jobID)
}

// ResumeJob removes a job from the paused set. Its pending tasks rejoin scheduling.
func (s *Scheduler) ResumeJob(jobID string) {
	s.dispatchMu.Lock()
	defer s.dispatchMu.Unlock()
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

// IsJobPaused is the exported view of the in-memory pause set. "paused" is a
// user control state that lives in the scheduler, separate from the derived
// lifecycle status in the DB; the service-layer aggregator consults this so it
// doesn't clobber a pause with a recomputed lifecycle status.
func (s *Scheduler) IsJobPaused(jobID string) bool {
	return s.isJobPaused(jobID)
}

// ClearPause drops the pause flag WITHOUT resume semantics (no scheduling
// rejoin implied, no "resumed" log). Kill is the other human intent that
// overrides pause: a killed job must be free to reach its terminal aggregate
// instead of parking at "paused" forever. Call before the final
// RefreshJobStatus of a kill path.
func (s *Scheduler) ClearPause(jobID string) {
	s.pauseMu.Lock()
	defer s.pauseMu.Unlock()
	delete(s.pausedJobs, jobID)
}

// ── Kill request tracking ─────────────────────────────────────────────────

// RequestKill persists user intent before publishing it in memory. It shares
// finishMu with terminal transitions, so a racing completion either clears the
// durable flag atomically with terminal persistence or wins before this write.
func (s *Scheduler) RequestKill(taskID string) error {
	s.finishMu.Lock()
	defer s.finishMu.Unlock()
	dbCtx, cancel := context.WithTimeout(s.ctx, 5*time.Second)
	defer cancel()
	recorded, err := s.store.SetTaskKillRequested(dbCtx, taskID, true)
	if err != nil {
		return fmt.Errorf("persist kill intent for %s: %w", taskID, err)
	}
	if !recorded {
		return nil // a terminal transition won the race
	}
	s.killMu.Lock()
	defer s.killMu.Unlock()
	s.killRequested[taskID] = true
	return nil
}

// RestoreKillRequest republishes already-durable intent during lane recovery.
func (s *Scheduler) RestoreKillRequest(taskID string) {
	s.killMu.Lock()
	defer s.killMu.Unlock()
	s.killRequested[taskID] = true
}

// TryKillPending atomically settles a task only if it is still waiting for
// handoff. A concurrent dispatch uses the same finishMu; if dispatch wins,
// this returns false and the caller must persist intent then cancel remotely.
func (s *Scheduler) TryKillPending(taskID, source string) (bool, error) {
	s.finishMu.Lock()
	defer s.finishMu.Unlock()
	task := s.queue.Get(taskID)
	if task == nil || task.Status != StatusPending {
		return false, nil
	}
	if !s.completeTask(task, StatusKilled, map[string]any{"status_source": source}) {
		return false, fmt.Errorf("task %s pending kill was not persisted", taskID)
	}
	s.RefreshJobStatus(task.JobID)
	return true, nil
}

// KillTask is the single entry point for user-initiated kills on
// scheduler-managed tasks: flag first (so the exit verdict reads "killed",
// not "failed"→retry), then best-effort termination via the launcher
// (runqd cancellation or scheduler-specific scancel/qdel).
func (s *Scheduler) KillTask(taskID string) error {
	if err := s.RequestKill(taskID); err != nil {
		return err
	}
	if err := s.launcher.Kill(taskID); err != nil {
		s.logger.Warn("remote cancel failed; kill intent retained", "task", taskID, "error", err)
		return err
	}
	return nil
}

// killPending reports whether a kill flag is set WITHOUT consuming it.
// Use when the settle decision depends on a fallible action (late remote
// cancel): consume only after the action succeeded, otherwise the intent
// must survive for the next lifecycle event.
func (s *Scheduler) killPending(taskID string) bool {
	s.killMu.Lock()
	defer s.killMu.Unlock()
	return s.killRequested[taskID]
}

// ClearKillRequest drops a pending kill flag without acting on it. Manual
// retry MUST call this: a stale flag from an earlier refused kill would
// otherwise assassinate the fresh attempt at its first lifecycle event
// (the flag survives refusals by design — see RQ-69 ownership protocol).
// completeTask also clears it on every terminal transition, so a flag can
// never outlive the attempt it was aimed at.
func (s *Scheduler) ClearKillRequest(taskID string) {
	s.killMu.Lock()
	defer s.killMu.Unlock()
	delete(s.killRequested, taskID)
}

// ── Job aggregate status ──────────────────────────────────────────────────

// RefreshJobStatus checks all tasks of a job and updates the job status in DB.
// Called after every task state transition (complete, fail, requeue).
//
// Rules:
//   - any task running → job = "running"
//   - all tasks terminal (success/failed/killed) → terminal split via
//     store.TerminalJobStatus: done / killed / failed / partial
//   - otherwise (some pending, none running) → keep current status
func (s *Scheduler) RefreshJobStatus(jobID string) {
	tasks, err := s.store.ListTasks(s.ctx, store.TaskFilter{JobID: jobID})
	if err != nil {
		s.logger.Error("refresh job status: list tasks failed", "job", jobID, "error", err)
		return
	}

	// Preserve the "paused" control state — UNCONDITIONALLY. Pause is a human
	// intent (same grammar as the kill flag, RQ-69): it outlives mechanical
	// task terminality and is released only by another human action. Even when
	// every task has settled, the job stays "paused" until the user resumes
	// (ResumeJob clears the set and re-runs this aggregation, which then lands
	// the terminal split) or kills (killJob clears the pause via ClearPause).
	// Rewriting paused → done here would silently discard the control state a
	// user is still holding.
	if s.isJobPaused(jobID) {
		return
	}

	newStatus, err := store.ProjectJobStatus(tasks)
	if err != nil {
		s.logger.Error("refresh job status: invalid task state", "job", jobID, "error", err)
		return
	}

	if err := s.store.UpdateJobStatus(s.ctx, jobID, newStatus); err != nil {
		s.logger.Error("refresh job status: update failed", "job", jobID, "status", newStatus, "error", err)
	}
}
