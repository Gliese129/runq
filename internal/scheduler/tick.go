package scheduler

import (
	"context"
	"time"

	"github.com/gliese129/runq/internal/utils"
)

// tick is one iteration of the scheduling loop.
//
// Algorithm (Prioritizer + Reservation + Backfill):
//  1. Ask Prioritizer to rank all pending tasks.
//  2. Find head = highest-priority non-blocked task (paused / frozen-job /
//     frozen-mount filters apply).
//  3. Head fits → dispatch it immediately.
//  4. Head doesn't fit + waited > AgingThreshold → reservation mode (block all).
//  5. Head doesn't fit + backfill enabled → dispatch the first smaller task
//     that fits AND passes the same blocked filters.
func (s *Scheduler) tick() {
	// The read lock spans the WHOLE tick (review #4 follow-up): dispatch
	// registers inflight.Add synchronously inside tick, so Quiesce (write
	// lock) returning guarantees no un-registered dispatch can follow.
	s.quiesceMu.RLock()
	defer s.quiesceMu.RUnlock()
	if s.quiesced {
		// Retiring lane (RQ-75): pending work is not frozen — it is
		// handed to the successor (or settled, for a removed target).
		// Late-arriving submits land here too and get forwarded next
		// tick, which is what makes an intake fence unnecessary.
		if fn := s.retireAction; fn != nil {
			for _, t := range s.queue.ListPending() {
				if !fn(t) {
					break // no destination yet — retry the whole set next tick
				}
			}
		}
		return
	}
	pending := s.queue.ListPending()
	if len(pending) == 0 {
		return
	}

	running := s.queue.ListRunning()
	sctx := ScheduleContext{
		Ctx:      s.ctx,
		Pending:  pending,
		Running:  running,
		FreeGPUs: s.pool.FreeCount(),
		Now:      time.Now(),
	}
	ranked := s.prioritizer.Prioritize(sctx)

	// Freeze-aware filtering data — load once per tick, not per candidate.
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

	// isDispatchBlocked encapsulates the "should I refuse to dispatch this
	// task right now?" check. Shared between head selection and backfill so
	// they can't drift apart — backfill ignoring freeze filters would let
	// small tasks land on a frozen mount even after head was correctly
	// skipped.
	isDispatchBlocked := func(t *Task) bool {
		if s.isJobPaused(t.JobID) {
			return true
		}
		// Sibling task already frozen on this job → skip; the pending task
		// would just self-freeze on its first save.
		if _, ok := frozenJobs[t.JobID]; ok {
			return true
		}
		// Mount has a frozen task on it → skip; dispatching to the same
		// stressed disk just compounds the problem.
		if mount := utils.MountOf(t.CheckpointDir, parts); mount != "" {
			if _, ok := frozenMounts[mount]; ok {
				return true
			}
		}
		return false
	}

	// Find head: the highest-priority non-blocked task. `ranked` is in
	// priority-desc order; first match wins.
	var head *Task
	for _, p := range ranked {
		t := taskMap[p.TaskID]
		if t == nil || isDispatchBlocked(t) {
			continue
		}
		head = t
		break
	}
	if head == nil {
		return // all pending tasks are paused/frozen
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

	// Backfill: find a smaller task that fits. Reuse isDispatchBlocked so
	// frozen-job / frozen-mount filters apply here too — otherwise small
	// tasks could slip onto a frozen mount after head was correctly skipped.
	if s.cfg.BackfillEnabled {
		for _, p := range ranked {
			t := taskMap[p.TaskID]
			if t == nil || t.ID == head.ID || isDispatchBlocked(t) {
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

// dispatch allocates resources, persists the running state, then launches the
// task. Order: allocate → write DB → update queue → start.
// If DB write fails, the allocation is released and the task stays pending
// for the next tick.
func (s *Scheduler) dispatch(task *Task) {
	gpus, err := s.pool.Allocate(task.GPUsNeeded, task.ID)
	if err != nil {
		s.logger.Warn("allocation failed", "task", task.ID, "need", task.GPUsNeeded, "free", s.pool.FreeCount())
		return
	}
	task.GPUs = gpus

	// Unsupervised (remote) lane: the task is handed to an external scheduler,
	// which may queue it for hours — so its DB status stays "pending" and
	// reconcile owns every subsequent transition. Queue-wise it occupies a
	// slot (MarkRunning) until FinishTask releases it.
	if !s.launcher.Supervised() {
		if err := s.queue.MarkRunning(task.ID); err != nil {
			s.logger.Error("mark running in queue failed", "task", task.ID, "error", err)
			s.pool.Release(task.ID)
			return
		}
		s.logger.Info("task handed to remote scheduler", "task", task.ID, "job", task.JobID)
		s.wg.Add(1)
		s.inflight.Add(1)
		go func() {
			defer s.wg.Done()
			defer s.inflight.Done()
			s.launchAsync(task)
		}()
		return
	}

	now := time.Now()
	dbCtx, cancel := context.WithTimeout(s.ctx, 5*time.Second)
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

	// Advance the job aggregate on the running transition too — not just on
	// completion. Without this a job stays "pending" in the DB while its task
	// is already running, so daemon CLI/WebUI show stale job status. Uses the
	// same aggregate helper as every terminal transition.
	s.RefreshJobStatus(task.JobID)

	s.logger.Info("task dispatched", "task", task.ID, "job", task.JobID, "gpus", gpus)
	s.wg.Add(1)
	s.inflight.Add(1)
	go func() {
		defer s.wg.Done()
		defer s.inflight.Done()
		s.runTask(task)
	}()
}

// shouldReserve returns true if the head-of-queue task has waited
// longer than the aging threshold.
func (s *Scheduler) shouldReserve(head *Task) bool {
	return time.Since(head.EnqueuedAt) > s.cfg.AgingThreshold
}
