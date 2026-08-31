package scheduler

import (
	"context"
	"time"
)

// tick is one iteration of the scheduling loop.
//
// If a submission slot is free, dispatch the oldest pending task whose job
// is not paused. Execution-resource policy belongs to runqd or the external
// cluster scheduler, not this handoff queue.
func (s *Scheduler) tick() {
	pending := s.queue.ListPending()
	if len(pending) == 0 || s.slots.FreeCount() <= 0 {
		return
	}

	for _, task := range pending {
		if s.isJobPaused(task.JobID) {
			continue
		}
		s.dispatch(task)
		return
	}
}

// dispatch acquires one remote-submission slot, persists pre-effect intent,
// publishes in-memory ownership, then launches the external command.
func (s *Scheduler) dispatch(task *Task) {
	// Serialize the pending→submitting publication with kill/retry/verdict
	// transitions. Without this gate, an API kill could observe pending while
	// dispatch concurrently started an external effect, then falsely settle
	// killed without cancelling that effect.
	s.finishMu.Lock()
	defer s.finishMu.Unlock()
	current := s.queue.Get(task.ID)
	if current == nil || current.Status != StatusPending {
		return
	}
	task = current

	// Hold the read side through the external handoff. PauseJob takes the
	// write side, so once it returns every launch admitted before the pause
	// has already returned from Launcher.Launch, while later dispatches see
	// the published pause flag. RWMutex keeps unrelated handoffs parallel.
	s.dispatchMu.RLock()
	if s.isJobPaused(task.JobID) {
		s.dispatchMu.RUnlock()
		return
	}
	launchLeaseHeld := true
	defer func() {
		if launchLeaseHeld {
			s.dispatchMu.RUnlock()
		}
	}()

	err := s.slots.Acquire(task.ID)
	if err != nil {
		s.logger.Debug("submission slot unavailable", "task", task.ID, "free", s.slots.FreeCount())
		return
	}

	// Persist the pre-effect submitting state before starting the external
	// command. A daemon crash after this point
	// is outcome-unknown, never safe-to-resubmit. Once the launcher records
	// the external id, the durable row moves back to pending (externally
	// queued) and reconcile owns later transitions.
	now := time.Now()
	dbCtx, cancel := context.WithTimeout(s.ctx, 5*time.Second)
	err = s.store.UpdateTaskStatus(dbCtx, task.ID, string(StatusSubmitting), map[string]any{
		"external_id":    nil,
		"status_source":  "submit",
		"failure_detail": nil,
		"started_at":     now.Unix(),
	})
	cancel()
	if err != nil {
		s.logger.Error("persist submitting intent failed", "task", task.ID, "error", err)
		s.slots.Release(task.ID)
		return
	}

	if err := s.queue.MarkRunning(task.ID); err != nil {
		dbCtx, cancel := context.WithTimeout(s.ctx, 5*time.Second)
		rollbackErr := s.store.UpdateTaskStatus(dbCtx, task.ID, string(StatusPending), map[string]any{
			"started_at": nil,
		})
		cancel()
		if rollbackErr != nil {
			s.logger.Error("queue publication and submitting rollback failed; slot retained",
				"task", task.ID, "queue_error", err, "rollback_error", rollbackErr)
			return
		}
		s.logger.Error("mark running in queue failed; durable intent rolled back", "task", task.ID, "error", err)
		s.slots.Release(task.ID)
		return
	}
	s.RefreshJobStatus(task.JobID)
	s.logger.Info("task handed to remote scheduler", "task", task.ID, "job", task.JobID)
	s.wg.Add(1)
	launchLeaseHeld = false // ownership transfers to the launch goroutine
	go func() {
		defer s.wg.Done()
		s.launchUnderDispatchLease(task)
	}()
}
