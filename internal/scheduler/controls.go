package scheduler

import (
	"github.com/gliese129/runq/internal/store"
)

// ── Job pause/resume ──────────────────────────────────────────────────────

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

// IsJobPaused is the exported view of the in-memory pause set. "paused" is a
// user control state that lives in the scheduler, separate from the derived
// lifecycle status in the DB; the service-layer aggregator consults this so it
// doesn't clobber a pause with a recomputed lifecycle status.
func (s *Scheduler) IsJobPaused(jobID string) bool {
	return s.isJobPaused(jobID)
}

// ── Kill request tracking ─────────────────────────────────────────────────

// RequestKill marks a task as user-killed. Call before Launcher.Kill()
// (currently executor.Stop at the call sites; consolidating both into one
// Scheduler.KillTask is planned for Step 3 of the scheduler unification).
// runTask checks this flag to decide killed vs retry.
func (s *Scheduler) RequestKill(taskID string) {
	s.killMu.Lock()
	defer s.killMu.Unlock()
	s.killRequested[taskID] = true
}

// KillTask is the single entry point for user-initiated kills on
// scheduler-managed tasks: flag first (so the exit verdict reads "killed",
// not "failed"→retry), then best-effort termination via the launcher
// (process-group kill locally; scancel/qdel on remote lanes).
func (s *Scheduler) KillTask(taskID string) {
	s.RequestKill(taskID)
	s.launcher.Kill(taskID)
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
//   - all tasks terminal (success/failed/killed) → job = "done"
//   - otherwise (some pending, none running) → keep current status
func (s *Scheduler) RefreshJobStatus(jobID string) {
	tasks, err := s.store.ListTasks(s.ctx, store.TaskFilter{JobID: jobID})
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

	// Preserve the "paused" control state. Lifecycle aggregation derives status
	// purely from task states and must not overwrite a user-initiated pause;
	// otherwise a running task completing flips the job to "running"/"pending"
	// and the later ResumeJob rejects ("not paused"). A paused job with work
	// still left stays paused so ResumeJob can find it; once every task is
	// terminal, "done" wins (resume is meaningless).
	if !isEnded && s.isJobPaused(jobID) {
		return
	}

	var newStatus string
	if isEnded {
		newStatus = "done"
	} else if isStarted {
		newStatus = "running"
	} else {
		newStatus = "pending"
	}

	if err := s.store.UpdateJobStatus(s.ctx, jobID, newStatus); err != nil {
		s.logger.Error("refresh job status: update failed", "job", jobID, "status", newStatus, "error", err)
	}
}
