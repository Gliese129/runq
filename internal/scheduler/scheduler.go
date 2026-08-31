// Package scheduler implements the runq-lab remote scheduling loop and task
// lifecycle.
//
// File layout:
//
//	scheduler.go        Scheduler struct, New, Start, Shutdown, loop
//	config.go           Config + DefaultConfig
//	tick.go             tick + dispatch (scheduling decisions)
//	task_lifecycle.go   launch/finish transitions and retries
//	controls.go         PauseJob / ResumeJob / RequestKill / RefreshJobStatus
//	queue.go            Task + Queue + TaskStatus
package scheduler

import (
	"context"
	"log/slog"
	"sync"
	"time"

	"github.com/gliese129/runq-lab/internal/resource"
	"github.com/gliese129/runq-lab/internal/store"
)

// Scheduler is the core scheduling loop.
// It pulls tasks from the queue, acquires submission slots, and dispatches to
// the configured remote launcher.
// All state transitions are persisted to store BEFORE updating the in-memory queue.
type Scheduler struct {
	cfg      Config
	queue    *Queue
	slots    *resource.SlotAllocator
	launcher Launcher
	store    *store.Store
	logger   *slog.Logger

	// pausedJobs tracks which jobs are paused. Scheduler skips pending tasks
	// belonging to paused jobs. Synced via PauseJob/ResumeJob API calls.
	pausedJobs map[string]bool
	pauseMu    sync.RWMutex
	// dispatchMu makes pause linearizable with external handoff. Dispatchers
	// hold a read lease from their final pause check until Launcher.Launch
	// returns; PauseJob takes the write side before publishing the pause flag.
	// Acknowledging a pause therefore means no admitted launch can begin later.
	dispatchMu sync.RWMutex

	// killRequested tracks tasks that were explicitly killed by the user.
	// A failed remote verdict checks this set before deciding retry vs killed.
	// Prevents user-killed tasks from being auto-retried.
	killRequested map[string]bool
	killMu        sync.Mutex

	// finishMu serializes FinishTask, where multiple
	// sensors (marker scan, probe align, kill paths) can deliver verdicts for
	// the same task concurrently — and a STALE verdict (read before a retry
	// requeued the task) must not clobber the new attempt.
	finishMu sync.Mutex

	// transientNote deduplicates the visible "submit failed, retrying" note
	// (RQ-74): taskID → last failure_detail text persisted for a transient
	// launch failure. SSH being down makes every tick fail with the same
	// message; without this the DB would take one UPDATE per task per tick.
	transientNote sync.Map

	ctx    context.Context
	cancel context.CancelFunc
	wg     sync.WaitGroup
}

// New creates a Scheduler with all its dependencies.
func New(
	cfg Config,
	queue *Queue,
	slots *resource.SlotAllocator,
	launcher Launcher,
	st *store.Store,
	logger *slog.Logger,
) *Scheduler {
	ctx, cancel := context.WithCancel(context.Background())
	return &Scheduler{
		cfg:           cfg,
		queue:         queue,
		slots:         slots,
		launcher:      launcher,
		store:         st,
		logger:        logger,
		pausedJobs:    make(map[string]bool),
		killRequested: make(map[string]bool),
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
	s.logger.Info("scheduler started", "tick", s.cfg.TickInterval.String())
}

// Shutdown stops scheduling and waits for in-progress handoff calls to return.
// Remote tasks continue under their execution service and are recovered by
// reconciliation after runq-lab restarts.
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
