// Package scheduler implements the runq scheduling loop, task lifecycle,
// and freeze-state machine.
//
// File layout:
//
//	scheduler.go        Scheduler struct, New, Start, Shutdown, loop, autoThawLoop
//	config.go           Config + DiskConfig + DefaultConfig
//	tick.go             tick + dispatch + shouldReserve (scheduling decisions)
//	task_lifecycle.go   runTask + completeTask + handleFailure + reap + reattach
//	controls.go         PauseJob / ResumeJob / RequestKill / RefreshJobStatus
//	gpu.go              checkGPUResidual + gpuRefreshLoop + gpuString
//	freeze.go           FreezeState (disk-freeze state machine; SDK-driven)
//	queue.go            Task + Queue + TaskStatus
//	task_lifecycle.go   lifecycle + shared ingest adapter
//	prioritizer.go      Prioritizer interface + FIFO / FairShare strategies
package scheduler

import (
	"context"
	"log/slog"
	"sync"
	"sync/atomic"
	"time"

	"github.com/gliese129/runq/internal/resource"
	"github.com/gliese129/runq/internal/store"
	"github.com/shirou/gopsutil/v4/disk"
)

// Scheduler is the core scheduling loop.
// It pulls tasks from the queue, allocates resources, and dispatches to the
// launcher (local process fork or remote submit — see Launcher).
// All state transitions are persisted to store BEFORE updating the in-memory queue.
type Scheduler struct {
	cfg         Config
	queue       *Queue
	pool        resource.Allocator
	launcher    Launcher
	store       *store.Store
	logger      *slog.Logger
	prioritizer Prioritizer

	// pausedJobs tracks which jobs are paused. Scheduler skips pending tasks
	// belonging to paused jobs. Synced via PauseJob/ResumeJob API calls.
	pausedJobs map[string]bool
	pauseMu    sync.RWMutex

	// killRequested tracks tasks that were explicitly killed by the user.
	// When runTask sees a non-zero exit, it checks this set before deciding
	// retry vs killed. Prevents user-killed tasks from being auto-retried.
	killRequested map[string]bool
	killMu        sync.Mutex

	// finishMu serializes FinishTask on the unsupervised lane, where multiple
	// sensors (marker scan, probe align, kill paths) can deliver verdicts for
	// the same task concurrently — and a STALE verdict (read before a retry
	// requeued the task) must not clobber the new attempt.
	finishMu sync.Mutex

	// transientNote deduplicates the visible "submit failed, retrying" note
	// (RQ-74): taskID → last failure_detail text persisted for a transient
	// launch failure. SSH being down makes every tick fail with the same
	// message; without this the DB would take one UPDATE per task per tick.
	transientNote sync.Map

	// L2-C: optional disk-freeze state machine. nil means freeze disabled.
	// When set, tick() filters on FrozenJobs/FrozenMounts and runTask calls
	// freeze.RemoveTask on every exit so auto-thaw fires when the last
	// frozen task dies.
	freeze *FreezeState

	// L2-C: daemon socket path injected as RUNQ_SOCKET_PATH into task env.
	// Reserved for stage 2+ SDK control-plane calls.
	socketPath string

	ctx    context.Context
	cancel context.CancelFunc
	wg     sync.WaitGroup

	// RQ-75 lane rotation: quiesced turns tick into a no-op (no NEW
	// dispatches) without cancelling in-flight work; inflight counts the
	// dispatch goroutines (launchAsync/runTask) so DrainLaunches can wait
	// for their outcomes to settle into the DB before a replacement lane
	// restores its queue from that DB.
	quiesced atomic.Bool
	inflight sync.WaitGroup
}

// New creates a Scheduler with all its dependencies.
// If prioritizer is nil, defaults to FIFO.
//
// socketPath is injected into each task's env as RUNQ_SOCKET_PATH (empty
// string disables injection). freeze may be nil to disable the disk-freeze
// feature; when set, the same instance should be wired into api.Deps.Freeze
// so the thaw endpoint operates on it.
func New(
	cfg Config,
	queue *Queue,
	pool resource.Allocator,
	launcher Launcher,
	st *store.Store,
	logger *slog.Logger,
	prioritizer Prioritizer,
	socketPath string,
	freeze *FreezeState,
) *Scheduler {
	if prioritizer == nil {
		prioritizer = FIFOPrioritizer{}
	}
	ctx, cancel := context.WithCancel(context.Background())
	return &Scheduler{
		cfg:           cfg,
		queue:         queue,
		pool:          pool,
		launcher:      launcher,
		store:         st,
		logger:        logger,
		prioritizer:   prioritizer,
		pausedJobs:    make(map[string]bool),
		killRequested: make(map[string]bool),
		freeze:        freeze,
		socketPath:    socketPath,
		ctx:           ctx,
		cancel:        cancel,
	}
}

// FreezeState returns the configured FreezeState, or nil if none was wired.
// Exposed for the API thaw handler and tests.
func (s *Scheduler) FreezeState() *FreezeState {
	return s.freeze
}

// Start begins the scheduling loop in a background goroutine.
func (s *Scheduler) Start() {
	s.wg.Add(1)
	go func() {
		defer s.wg.Done()
		s.loop()
	}()
	if s.cfg.GPURefreshInterval > 0 {
		s.wg.Add(1)
		go func() {
			defer s.wg.Done()
			s.gpuRefreshLoop()
		}()
	}
	// autoThawLoop is part of the freeze feature; launch whenever freeze
	// is wired. No separate "enable" knob — if you have freeze you need
	// auto-thaw to ever recover without manual intervention.
	if s.freeze != nil {
		s.wg.Add(1)
		go func() {
			defer s.wg.Done()
			s.autoThawLoop()
		}()
	}
	s.logger.Info("scheduler started",
		"strategy", s.prioritizer.Name(),
		"aging_threshold", s.cfg.AgingThreshold.String(),
		"backfill", s.cfg.BackfillEnabled,
		"tick", s.cfg.TickInterval.String(),
		"gpu_refresh", s.cfg.GPURefreshInterval.String(),
	)
}

// Shutdown stops the scheduling loop and waits for all running tasks to finish.
func (s *Scheduler) Shutdown() {
	s.cancel()
	s.wg.Wait()
	s.logger.Info("scheduler stopped")
}

// Quiesce stops the scheduler from dispatching NEW tasks (tick becomes a
// no-op) while leaving in-flight launches and running tasks untouched —
// the K8s-termination / nginx-reload "stop accepting, finish what you
// have" phase of a lane swap (RQ-75). There is deliberately no resume:
// a quiesced scheduler's lane is on its way out.
func (s *Scheduler) Quiesce() {
	if !s.quiesced.Swap(true) {
		s.logger.Info("scheduler quiesced — no new dispatches")
	}
}

// DrainLaunches waits until every in-flight dispatch goroutine has settled
// its outcome (external id, transient requeue, unknown, or rejection — all
// DB-visible states), or ctx expires. Returns true when fully drained.
// Call after Quiesce; without it new dispatches keep the count busy.
func (s *Scheduler) DrainLaunches(ctx context.Context) bool {
	done := make(chan struct{})
	go func() {
		s.inflight.Wait()
		close(done)
	}()
	select {
	case <-done:
		return true
	case <-ctx.Done():
		return false
	}
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

// autoThawLoop periodically tries to release frozen tasks whose mount has
// recovered enough free space for that task's NeededBytes. Per-task
// threshold is set by the SDK at freeze time (FrozenTask.NeededBytes), so
// freeze and thaw decisions are physically symmetric.
//
// ThawTasks handles per-mount aggregation internally — we just pass it
// all frozen IDs and let it figure out which mounts to query.
func (s *Scheduler) autoThawLoop() {
	interval := s.cfg.Disk.AutoThawCheckInterval
	if interval <= 0 {
		interval = 60 * time.Second
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	// Wrap disk.Usage once; reused on every tick.
	freeFn := func(mount string) (uint64, error) {
		u, err := disk.Usage(mount)
		if err != nil {
			return 0, err
		}
		return u.Free, nil
	}

	for {
		select {
		case <-s.ctx.Done():
			return
		case <-ticker.C:
			if s.freeze == nil || !s.freeze.IsFrozen() {
				continue
			}
			result := s.freeze.ThawTasks(s.freeze.FrozenTaskIDs(), freeFn)
			if len(result.Thawed) > 0 {
				s.logger.Info("autoThaw released",
					"tasks", result.Thawed,
					"still_blocked", len(result.Blocked))
			}
		}
	}
}
