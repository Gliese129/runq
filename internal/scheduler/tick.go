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

// shouldReserve returns true if the head-of-queue task has waited
// longer than the aging threshold.
func (s *Scheduler) shouldReserve(head *Task) bool {
	return time.Since(head.EnqueuedAt) > s.cfg.AgingThreshold
}
