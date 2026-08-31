package scheduler

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/gliese129/runq-lab/internal/store"
)

// launchAsync hands a task to a remote launcher and returns. There is
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
//
// launchAsync submits a remote task and settles the outcome against
// the kill flag. The kill × launch-outcome semantics (RQ-69) are:
//
//	outcome     │ no flag              │ flag set
//	────────────┼──────────────────────┼────────────────────────────────────
//	OK          │ await verdict        │ cancel late job (fallible!);
//	            │                      │ consume+killed on success,
//	            │                      │ RETAIN flag on cancel failure
//	transient   │ requeue (no attempt) │ consume+killed (never reached
//	            │                      │ the scheduler — settle honestly)
//	rejected    │ permanent failure    │ consume+killed (same reasoning)
//	untracked   │ await reconcile      │ RETAIN flag (a cluster job may be
//	            │                      │ running — we will not lie; the
//	            │                      │ verdict path settles it)
//
// Principles: a user kill wins over every path that would resubmit; a task
// is marked killed ONLY when no unmanaged remote job can exist (never
// submitted, or cancel confirmed); a retained flag is honored by the next
// lifecycle event (handleFailure / verdict) and cleared on any terminal
// transition, so it cannot outlive the attempt.
func (s *Scheduler) launchAsync(task *Task) {
	err := s.launcher.Launch(s.ctx, task)
	s.handleLaunchOutcome(task, err)
}

// launchUnderDispatchLease is the dispatch-loop entry point. The caller
// acquired dispatchMu.RLock before publishing the launch; release it as soon
// as the external handoff call returns so PauseJob can acknowledge without
// waiting for local outcome bookkeeping.
func (s *Scheduler) launchUnderDispatchLease(task *Task) {
	err := func() error {
		defer s.dispatchMu.RUnlock()
		return s.launcher.Launch(s.ctx, task)
	}()
	s.handleLaunchOutcome(task, err)
}

func (s *Scheduler) handleLaunchOutcome(task *Task, err error) {
	switch {
	case err == nil:
		s.logger.Info("task submitted to remote scheduler", "task", task.ID, "job", task.JobID)
		// The handoff succeeded — the launcher cleared the DB-side transient
		// note together with the external-id write; drop the dedup entry so
		// a LATER transient failure (next attempt) is persisted again.
		s.transientNote.Delete(task.ID)
		if s.killPending(task.ID) {
			// Kill raced the submit: the external id exists only now.
			// Cancel is fallible — only a CONFIRMED cancel may settle
			// killed (the kill never lies); on failure the flag survives
			// for the next lifecycle event.
			if kerr := s.killViaLauncher(task.ID); kerr != nil {
				s.logger.Warn("late cancel failed — kill intent retained",
					"task", task.ID, "error", kerr)
				return
			}
			s.settleKilled(task, "late cancel confirmed")
		}
	case errors.Is(err, ErrLaunchTransient):
		if s.settleKilled(task, "transient launch — never reached the scheduler") {
			return
		}
		s.logger.Warn("scheduler unreachable, task returned to queue",
			"task", task.ID, "error", err)
		if !s.requeueTransient(task) {
			return
		}
		// RQ-74: the retry loop must not be silent — persist the transport
		// error on the (still pending) row so ps/dashboard can show WHY the
		// task keeps waiting. Deduplicated: SSH being down repeats the same
		// message every tick.
		s.noteTransientFailure(task, err)
	case errors.Is(err, ErrLaunchUntracked):
		// RQ-74: the submission started but its outcome was lost (mid-flight
		// disconnect, id parse/persist failure). A cluster job MAY exist, so
		// neither retry (double submit) nor fail (may be running) is honest —
		// the task becomes `unknown`, carrying the error verbatim, and waits
		// for reconcile to settle it from facts (status.json marker / probe).
		// If a kill is pending we keep the flag (no lying); the verdict path
		// settles it.
		s.logger.Error("task outcome unknown after launch", "task", task.ID, "error", err)
		s.markUnknown(task, err)
	default:
		if s.settleKilled(task, "rejected submission — no remote job") {
			return
		}
		s.logger.Error("submission rejected", "task", task.ID, "error", err)
		// RQ-74: the scheduler's own words (stderr + exit code + rendered
		// command, composed by the launcher) become the task's permanent
		// failure evidence — visible in task show / ps / dashboard, not
		// only in the daemon log.
		s.FinishTaskNoRetry(task, map[string]any{
			"status_source":  "submit",
			"failure_detail": failureDetail(err),
		})
	}
}

// markUnknown persists the `unknown` status (queue + DB) with the launch
// error as visible evidence. The submission slot stays held — a cluster job
// may exist. Reconcile is the only path out (verdict → FinishTask), plus
// user kill.
func (s *Scheduler) markUnknown(task *Task, cause error) {
	dbCtx, cancel := context.WithTimeout(s.ctx, 5*time.Second)
	defer cancel()
	if err := s.store.UpdateTaskStatus(dbCtx, task.ID, string(StatusUnknown), map[string]any{
		"status_source":  "submit",
		"failure_detail": failureDetail(cause),
	}); err != nil {
		s.logger.Error("persist unknown status failed; queue left running", "task", task.ID, "error", err)
		return
	}
	// The unknown evidence replaces any earlier transient note — drop the
	// dedup entry so a FUTURE transient failure with the same text (after a
	// manual retry) is not silently swallowed against a stale memory.
	s.transientNote.Delete(task.ID)
	if err := s.queue.MarkUnknown(task.ID); err != nil {
		s.logger.Error("mark unknown in queue failed", "task", task.ID, "error", err)
	}
}

// noteTransientFailure writes the transport error onto the still-pending row
// (failure_detail) so the wait is self-explaining, deduplicating identical
// messages across ticks. The note is cleared on a successful handoff
// (remote launcher persists failure_detail=nil with the external id) and on
// every terminal transition (completeTask defaults failure_detail to nil).
func (s *Scheduler) noteTransientFailure(task *Task, cause error) {
	detail := "submit not reaching the scheduler yet — will retry automatically:\n" + failureDetail(cause)
	if prev, ok := s.transientNote.Load(task.ID); ok && prev.(string) == detail {
		return
	}
	// Under finishMu, gated on the queue's CURRENT view: a user kill can
	// settle the task in the window between requeueTransient and this
	// write, and an unconditional UPDATE (status=pending) would resurrect
	// a killed row. The queue is the authority — only a still-pending
	// entry takes the note.
	s.finishMu.Lock()
	defer s.finishMu.Unlock()
	if qt := s.queue.Get(task.ID); qt == nil || qt.Status != StatusPending {
		return
	}
	dbCtx, cancel := context.WithTimeout(s.ctx, 5*time.Second)
	defer cancel()
	if err := s.store.UpdateTaskStatus(dbCtx, task.ID, string(StatusPending), map[string]any{
		"failure_detail": detail,
	}); err != nil {
		s.logger.Warn("persist transient note failed", "task", task.ID, "error", err)
		return
	}
	s.transientNote.Store(task.ID, detail)
}

// ResumeUnknown flips an unknown queue entry back to running: reconcile
// confirmed (wrapper facts) that a live cluster job exists behind an
// outcome-lost submission (RQ-74). Under finishMu so it cannot interleave
// with a concurrent verdict delivery; the DB write is the caller's
// (reconcile is the DB's writer on this lane — this only syncs the queue).
func (s *Scheduler) ResumeUnknown(taskID string) {
	s.finishMu.Lock()
	defer s.finishMu.Unlock()
	if err := s.queue.ResumeUnknown(taskID); err != nil {
		s.logger.Warn("resume unknown: queue sync failed", "task", taskID, "error", err)
	}
}

// settleKilled consumes a pending kill flag and drives the task to killed.
// Returns false (and does nothing) when no kill was requested. Callers
// must have established that recording killed is HONEST at this point: no
// unmanaged remote job can exist (never submitted, or cancel confirmed).
func (s *Scheduler) settleKilled(task *Task, reason string) bool {
	if !s.killPending(task.ID) {
		return false
	}
	s.logger.Info("kill honored", "task", task.ID, "at", reason)
	if err := s.FinishTaskChecked(task, StatusKilled, map[string]any{"status_source": "runq"}); err != nil {
		// Intent and ownership remain durable. In particular, do not requeue a
		// task whose user cancellation could not be persisted.
		s.logger.Error("persist confirmed kill failed; intent retained", "task", task.ID, "error", err)
	}
	return true
}

func (s *Scheduler) killViaLauncher(taskID string) error {
	return s.launcher.Kill(taskID)
}

// requeueTransient puts a remote task back to pending after a launch
// that provably never reached the scheduler. Persistence precedes queue
// publication and lease release; on a failed write the durable submitting
// intent remains active and restart will conservatively recover it as unknown.
func (s *Scheduler) requeueTransient(task *Task) bool {
	s.finishMu.Lock()
	defer s.finishMu.Unlock()
	dbCtx, cancel := context.WithTimeout(s.ctx, 5*time.Second)
	defer cancel()
	if err := s.store.UpdateTaskStatus(dbCtx, task.ID, string(StatusPending), map[string]any{
		"external_id": nil,
		"started_at":  nil,
	}); err != nil {
		s.logger.Error("persist transient requeue failed; queue and lease retained",
			"task", task.ID, "error", err)
		return false
	}
	if err := s.queue.RequeueTransient(task.ID); err != nil {
		s.logger.Error("transient queue requeue failed after durable rollback", "task", task.ID, "error", err)
		return false
	}
	s.slots.Release(task.ID)
	// No attempt reached the execution service. Recompute the aggregate so a
	// lone task does not leave its job stuck in running after returning to
	// pending; other active tasks still keep the aggregate running.
	s.RefreshJobStatus(task.JobID)
	return true
}

// FinishTask drives a task to a terminal state through the shared lifecycle.
// StatusSuccess / StatusKilled persist immediately; StatusFailed goes through
// handleFailure, which requeues (retry) or permanently fails the task.
//
// extra carries additional DB fields to persist with the terminal transition
// (e.g. status_source, native_state, a wrapper-reported finished_at); nil is
// fine. Keys in extra override the defaults.
//
// This is the single funnel for task completion. Reconcile calls it when a
// terminal signal arrives from the remote scheduler; it releases the held
// submission slot only after the durable transition succeeds.
//
// Concurrency contract: sensors (marker scan, probe align, kill paths) may
// deliver verdicts concurrently, and a verdict computed from data
// read BEFORE a retry requeued the task is stale. finishMu serializes the
// check-then-act, and the queue's in-memory state is the authority on which
// attempt is current: verdicts are accepted only for tasks whose queue entry
// is running (in flight), plus the pending→killed case (user cancelling a
// task that was never handed off). Everything else is dropped.
func (s *Scheduler) FinishTask(task *Task, status TaskStatus, extra map[string]any) {
	_ = s.FinishTaskChecked(task, status, extra)
}

// FinishTaskChecked is the error-reporting lifecycle funnel for API paths
// that must not acknowledge a kill/retry transition unless it became durable.
func (s *Scheduler) FinishTaskChecked(task *Task, status TaskStatus, extra map[string]any) error {
	if s.finishTaskInner(task, status, extra, true) {
		return nil
	}
	return fmt.Errorf("task %s transition to %s was not applied", task.ID, status)
}

// FinishTaskNoRetry records a permanent failure that must NOT enter the
// retry policy — deterministic scheduler rejections (submission ran, exit
// non-zero): retrying replays the same answer and, with max_retry=-1
// (unlimited), becomes a log storm.
func (s *Scheduler) FinishTaskNoRetry(task *Task, extra map[string]any) {
	s.finishTaskInner(task, StatusFailed, extra, false)
}

func (s *Scheduler) finishTaskInner(task *Task, status TaskStatus, extra map[string]any, allowRetry bool) bool {
	s.finishMu.Lock()
	defer s.finishMu.Unlock()

	qt := s.queue.Get(task.ID)
	switch {
	case qt == nil:
		// Unmanaged row: not in this lane's queue (should not happen while
		// the daemon runs), but an edge restore gap must not lose wrapper
		// facts. Persist without retry or slot bookkeeping.
		s.logger.Warn("finish verdict for unmanaged task: persisting without lifecycle",
			"task", task.ID, "status", string(status))
		if status != StatusSuccess && status != StatusKilled {
			status = StatusFailed
		}
		transitioned := s.completeTask(task, status, extra)
		if transitioned {
			s.RefreshJobStatus(task.JobID)
		}
		return transitioned
	case qt.Status == StatusRunning:
		// In flight — accept. Attempt-level staleness is prevented by the
		// target lifecycle lock; this remains a queue-state backstop.
	case qt.Status == StatusUnknown:
		// Reconcile is allowed to heal an outcome-unknown submission.
	case qt.Status == StatusPending && status == StatusKilled:
		// Never handed off, user cancelled — accept.
	default:
		s.logger.Debug("finish verdict dropped: stale",
			"task", task.ID, "queue_status", string(qt.Status), "verdict", string(status))
		return false
	}
	// Kill wins over retry — the ONE verdict-side checkpoint (the launch-
	// side table lives on launchAsync). A failed verdict with a pending
	// kill settles killed instead of re-entering the retry policy; success
	// stands (facts win) and completeTask clears any leftover flag. Placed
	// AFTER the staleness gate on purpose: a stale attempt's verdict must
	// not consume a flag aimed at the current attempt.
	if status == StatusFailed && s.killPending(task.ID) {
		s.logger.Info("kill honored on failure verdict", "task", task.ID, "retry", task.RetryCount)
		status = StatusKilled
		merged := make(map[string]any, len(extra)+1)
		for k, v := range extra {
			merged[k] = v
		}
		merged["status_source"] = "runq"
		extra = merged
	}
	var transitioned bool
	switch {
	case status == StatusSuccess || status == StatusKilled:
		transitioned = s.completeTask(task, status, extra)
		if transitioned {
			s.RefreshJobStatus(task.JobID)
		}
	case allowRetry:
		transitioned = s.handleFailure(task, extra)
	default:
		transitioned = s.completeTask(task, StatusFailed, extra)
		if transitioned {
			s.RefreshJobStatus(task.JobID)
		}
	}
	if transitioned {
		s.slots.Release(task.ID)
	}
	return transitioned
}

// completeTask persists a terminal status to DB, then updates the queue.
// extra fields are merged over the defaults (so a wrapper-reported
// finished_at wins over the wall clock).
func (s *Scheduler) completeTask(task *Task, status TaskStatus, extra map[string]any) bool {
	dbCtx, cancel := context.WithTimeout(s.ctx, 5*time.Second)
	defer cancel()
	fields := map[string]any{
		"finished_at":    time.Now().Unix(),
		"kill_requested": 0,
		// Stale scheduler-state tokens must not survive into a terminal row
		// ("Killed" + native_state "running" is a lie). A verdict that DOES
		// know the final native state passes it via extra and overrides.
		"native_state": nil,
		// Same honesty rule for pre-run failure evidence (RQ-74): a task
		// that reached a terminal through the run phase must not keep a
		// stale submit-era note ("retrying…" on a succeeded task). Verdicts
		// that carry real evidence (submit rejection) override via extra.
		"failure_detail": nil,
	}
	for k, v := range extra {
		fields[k] = v
	}
	if err := s.store.UpdateTaskStatus(dbCtx, task.ID, string(status), fields); err != nil {
		// DB is the truth, the queue a view — the view must never run
		// ahead (review round 5 #2): a memory-terminal/DB-non-terminal
		// split would strand the task with no mechanism left to retry it.
		// Leaving the queue untouched keeps the verdict retryable (marker
		// rescan / probe / next sweep delivers it again).
		s.logger.Error("persist task completion failed — queue left unchanged, verdict will be redelivered",
			"task", task.ID, "status", status, "error", err)
		return false
	}
	s.transientNote.Delete(task.ID)
	if err := s.queue.Complete(task.ID, status); err != nil {
		s.logger.Error("complete in queue failed", "task", task.ID, "error", err)
	}
	// A kill flag never outlives the attempt it was aimed at: whatever the
	// terminal state (success beats a late kill; killed already consumed
	// it), leftover intent must not leak into a future manual retry.
	s.ClearKillRequest(task.ID)
	return true
}

// handleFailure decides whether to retry or permanently fail a task.
// MaxRetry < 0 means unlimited retries; 0 means none. extra fields (may
// be nil) are persisted only with the permanent-failure transition; a
// requeue resets state instead.
func (s *Scheduler) handleFailure(task *Task, extra map[string]any) bool {
	// No kill handling here: the failed×kill decision is made ONCE in
	// finishTaskInner's funnel checkpoint before this is even reached.
	//
	// Retry budget semantics (changed with RQ-69): -1 = unlimited
	// (explicit opt-in), 0 = no retries. The zero value is now the SAFE
	// behavior — under the old "0 = unlimited" rule, a project that simply
	// omitted max_retry defaulted into an infinite resubmit loop.
	canRetry := task.MaxRetry < 0 || task.RetryCount < task.MaxRetry
	if canRetry {
		nextRetry := task.RetryCount + 1
		dbCtx, cancel := context.WithTimeout(s.ctx, 5*time.Second)
		defer cancel()
		fields := map[string]any{
			"retry_count": nextRetry,
			"gpus":        nil,
			"started_at":  nil,
			"finished_at": nil,
			// Clear the external scheduler id: on the remote lane a requeue
			// means a NEW submission — probing the old id would track a dead
			// cluster job.
			"external_id": nil,
			// A new attempt starts with a clean slate — stale failure
			// evidence from the previous attempt must not survive into it.
			"failure_detail": nil,
			"status_source":  nil,
			"native_state":   nil,
			"queue":          nil,
			"kill_requested": 0,
		}
		// Reconciliation attaches a complete durable attempt fence to extra.
		// Keep that predicate even though terminal evidence fields themselves
		// do not belong on the fresh pending epoch.
		store.CarryTaskStatusFence(fields, extra)
		if err := s.store.UpdateTaskStatus(dbCtx, task.ID, "pending", fields); err != nil {
			s.logger.Error("persist requeue failed; queue left unchanged", "task", task.ID, "error", err)
			return false
		}
		if err := s.queue.Requeue(task.ID); err != nil {
			s.logger.Error("requeue failed", "task", task.ID, "error", err)
			return true
		}
		s.logger.Info("task re-queued", "task", task.ID, "retry", nextRetry, "max_retry", task.MaxRetry)
		return true
	}

	if !s.completeTask(task, StatusFailed, extra) {
		return false
	}
	s.RefreshJobStatus(task.JobID)
	s.logger.Warn("task failed permanently", "task", task.ID, "retry", task.RetryCount, "max_retry", task.MaxRetry)
	return true
}

// maxFailureDetailLen caps the persisted failure evidence. Scheduler stderr is
// normally a few lines; the cap only guards against a pathological submit
// command spewing megabytes into the DB row.
const maxFailureDetailLen = 8 << 10

// failureDetail renders an error as the task's persisted failure evidence
// (tasks.failure_detail, RQ-74), truncated to maxFailureDetailLen.
func failureDetail(err error) string {
	msg := err.Error()
	if len(msg) > maxFailureDetailLen {
		msg = msg[:maxFailureDetailLen] + "\n… (truncated)"
	}
	return msg
}
