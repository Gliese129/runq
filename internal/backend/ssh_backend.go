package backend

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"sync"
	"sync/atomic"
	"time"

	"github.com/gliese129/runq-lab/internal/logfile"

	"github.com/gliese129/runq-lab/internal/config"
	"github.com/gliese129/runq-lab/internal/ingest"
	"github.com/gliese129/runq-lab/internal/job"
	"github.com/gliese129/runq-lab/internal/project"
	"github.com/gliese129/runq-lab/internal/remote"
	"github.com/gliese129/runq-lab/internal/resource"
	"github.com/gliese129/runq-lab/internal/rfs"
	"github.com/gliese129/runq-lab/internal/scheduler"
	"github.com/gliese129/runq-lab/internal/store"
	"github.com/gliese129/runq-lab/internal/workspace"
)

// Sensor cadences for the remote lane. Both loops are gated on HasInFlight —
// zero in-flight tasks means zero SSH traffic (the silence contract).
//
//   - markerScanInterval: one SFTP readdir of the done dir. Cheap, so it can
//     run often; this is the PRIMARY completion path.
//   - probeAlignInterval: a full SchedulerProbe pass (batch qstat). Expensive
//     and visible to cluster admins — deliberately infrequent; it exists to
//     catch tasks whose wrapper never wrote a marker (node fail, hard kill).
//     User-triggered refreshes are separate and unaffected.
const (
	markerScanInterval = 2 * time.Minute
	probeAlignInterval = 15 * time.Minute

	// hibernateAfter: with no user-driven interaction on this target for
	// this long, the sensor drops to a skeleton cadence — qstat alignment
	// stops ENTIRELY (a probe loop running all night is exactly what gets
	// accounts flagged by cluster admins) and marker scans slow down but
	// keep running (one readdir; it releases submission slots, so a big
	// overnight sweep keeps flowing). Any user action wakes it instantly.
	// Accepted cost: node-fail slot leaks wait until the user returns.
	hibernateAfter = 30 * time.Minute

	// hibernatedMarkerEvery: while hibernated, marker scan runs every Nth
	// tick (N × markerScanInterval = 10min).
	hibernatedMarkerEvery = 5

	// metricsIngestInterval: background incremental metrics pass (spec
	// §8.1.4) — long interval spreads the IO into a steady trickle; the
	// (size,offset) mark makes each pass one stat per idle running task.
	metricsIngestInterval = 5 * time.Minute

	// L4 freshness thresholds. Soft: a read older than this nudges the
	// sensor for a background pass (SWR) — the read itself never waits.
	// Hard: older than this (with tasks in flight) is reported stale=true.
	// Hard sits above the hibernated marker cadence (5×2min) so normal
	// hibernation never smears "stale" over healthy data.
	syncSoftTTL = 5 * time.Minute
	syncHardTTL = 30 * time.Minute

	// Two-tier forced-refresh floors (D22/D23) — SSH and qstat cost
	// DIFFERENT amounts and get different brakes:
	//
	//   refreshMinInterval (10s): floor on the whole forced pass. What it
	//   throttles is SSH-level work (marker readdir + status.json reads) —
	//   cheap, a double-clicked button just shouldn't stampede.
	//
	//   schedProbeFloor (5min): floor on the SCHEDULER PROBE inside the
	//   pass. qstat/squeue on shared login nodes is the thing cluster
	//   admins actually watch; a forced pass whose last probe is younger
	//   than this reuses it (SchedulerProbe's floor gate) and still
	//   resyncs markers/status — the primary completion path.
	refreshMinInterval = 10 * time.Second
	schedProbeFloor    = 5 * time.Minute

	// taskSilenceAfter: a running task whose output streams (log,
	// activity.tsv, metrics.jsonl) haven't advanced in this long is
	// SUSPECTED dead-without-marker (node crash, OOM-killed wrapper) and
	// earns an early scheduler consultation. Generous on purpose: long
	// validation phases legitimately go quiet for minutes.
	taskSilenceAfter = 15 * time.Minute
)

// SSHBackend implements Backend for a remote HPC cluster accessed via SSH.
// Each SSHBackend holds its own rfs.SSHFS (persistent SSH connection) and
// per-target scheduler templates — no shared hpc: config section needed.
//
// Reconcile strategy (push from run.sh + poll):
//   - Background marker polling: daemon periodically ls .done/<task_id>
//     to detect completed tasks (lightweight, 30-60s interval).
//   - On-demand qstat: user clicks Refresh in the dashboard, triggering
//     a full scheduler probe for the job.
//   - status.json: written by run.sh on compute nodes, read by daemon via SSH.
type SSHBackend struct {
	storeQueries    // embeds store, reg, and shared project/clean/thaw/dryrun methods
	backend         *remote.Backend
	transportCloser io.Closer        // SSHFS, or an injected closeable transport
	targetName      string           // this lane's target name (tasks.target scope)
	scope           *store.LaneScope // RQ-75: generation-ownership predicate, SHARED with remote.Backend

	// Per-target scheduler lane (RQ-46): queue + submission slots + scheduler
	// instance + remote launcher.
	queue    *scheduler.Queue
	pool     *resource.SlotAllocator
	sched    *scheduler.Scheduler
	launcher *remote.Launcher
	logger   *slog.Logger

	loopMu     sync.Mutex
	loopCancel context.CancelFunc
	loopWG     sync.WaitGroup

	// A returned log follower outlives the routing call that created it.
	// Close quiesces immediately but defers transport teardown until every
	// follower releases its lease, so config rotation never severs a stream.
	artifactMu        sync.Mutex
	artifactRefs      int
	closeRequested    bool
	transportClosed   bool
	transportCloseErr error

	// GPU view cache (gpu_template targets): dashboard panels poll at
	// human-refresh frequency; one SSH exec per gpuCacheTTL is plenty.
	gpuMu      sync.Mutex
	gpuCache   []GPUSlot
	gpuCacheAt time.Time

	// lastActivity is the unix time of the last user-driven interaction with
	// this target (list/get/submit/kill/retry/refresh — dashboard polling
	// counts: the user is watching). The sensor loop hibernates when it goes
	// stale; see hibernateAfter.
	lastActivity atomic.Int64

	// ── L4 sync-cycle state (process-scoped; the DATA's freshness lives
	// in the sync_state table) ──
	//
	// syncGen counts completed FULL passes (marker scan + forced probe).
	// A ForceRefresh waiter records g0 at kick time and waits for
	// gen > g0 — waiting for "any pass" would let a pass that STARTED
	// before the kick satisfy the wait and hand back the old photo.
	syncMu     sync.Mutex
	syncGen    uint64
	syncDone   chan struct{} // closed+replaced at each full-pass completion
	lastForced time.Time     // min_interval throttle (memory: restart resets, fine)

	// nudgeCh wakes the sensor loop for one forced full pass. Capacity 1 +
	// non-blocking send = concurrent nudges coalesce by construction.
	nudgeCh chan struct{}

	// admissionsInFlight counts SubmitJob and manual RetryTask calls between
	// entry and return. It is incremented BEFORE the retired-gate check, so
	// the retirement sweep cannot close a lane while new intent, reset, or
	// queue publication is still in flight on a previously resolved pointer.
	admissionsInFlight atomic.Int64
}

// HasInFlightAdmissions reports whether a submit or manual retry is currently
// executing against this lane. The retirement sweep refuses to close while
// true, even before the operation has published a non-terminal task row.
func (b *SSHBackend) HasInFlightAdmissions() bool { return b.admissionsInFlight.Load() > 0 }

// touchActivity records a user-driven interaction (wakes a hibernated sensor
// on its next tick).
func (b *SSHBackend) touchActivity() { b.lastActivity.Store(time.Now().Unix()) }

// userActive reports whether the target saw user activity within
// hibernateAfter.
func (b *SSHBackend) userActive() bool {
	return time.Since(time.Unix(b.lastActivity.Load(), 0)) <= hibernateAfter
}

// SSHBackendConfig bundles everything needed to build an SSHBackend.
type SSHBackendConfig struct {
	Target    config.TargetConfig
	Store     *store.Store
	GlobalCfg *config.GlobalConfig
	Logger    *slog.Logger // nil → slog.Default()

	// FS overrides the filesystem/exec transport. When set, the ssh:
	// section is not required — this is how the client's synthesized
	// localhost-runqd lane runs the exact same remote machinery over
	// rfs.LocalFS (plumbing commands default to runqd.sock, so templates
	// are verbatim identical to a remote runq target).
	FS rfs.FS
}

// NewSSHBackend creates a Backend for a remote HPC target. The SSH
// connection is lazy — no dial happens until the first operation.
//
// The caller must call Close() on shutdown to release the SSH connection.
func NewSSHBackend(cfg SSHBackendConfig) (*SSHBackend, error) {
	t := cfg.Target
	if t.SubmitTemplate == "" {
		return nil, fmt.Errorf("target %q: submit_template is required", t.Name)
	}

	var laneFS rfs.FS
	var transportCloser io.Closer
	if cfg.FS != nil {
		// Injected transport (localhost-runqd lane over LocalFS).
		laneFS = cfg.FS
		if closer, ok := cfg.FS.(io.Closer); ok {
			transportCloser = closer
		}
	} else {
		if t.SSH == nil {
			return nil, fmt.Errorf("target %q: ssh section is required for scheduler targets", t.Name)
		}
		// Build SSH config from target. The host may be an ~/.ssh/config
		// alias ("tsubame") — resolve it the way OpenSSH would; explicit
		// target fields win over ssh_config values.
		sshHost, sshPort, sshUser, sshKey := rfs.ResolveSSHConfigDefaults(
			t.SSH.Host, t.SSH.Port, t.SSH.User, t.SSH.Key)
		host := sshHost
		if sshPort > 0 {
			host = fmt.Sprintf("%s:%d", sshHost, sshPort)
		}

		auth, err := rfs.ResolveAuthMethods(sshKey)
		if err != nil {
			return nil, fmt.Errorf("target %q: ssh auth: %w", t.Name, err)
		}

		sshCfg := rfs.SSHConfig{
			Host:        host,
			User:        sshUser,
			AuthMethods: auth,
			// RQ-74: honor the user's own `StrictHostKeyChecking
			// accept-new` for this alias — runq is never more ceremonious
			// than their ssh. Mismatch still hard-fails.
			HostKeyPolicy: rfs.ResolveHostKeyPolicy(t.SSH.Host),
			// Idle disconnect: with the sensor loops running every ~2min
			// while tasks are in flight, the connection stays warm during
			// activity and closes ~10min after the queue drains — a normal
			// SSH user's shape.
			IdleTimeout: 10 * time.Minute,
		}
		sshFS := rfs.NewSSHFS(sshCfg)
		laneFS = sshFS
		transportCloser = sshFS
	}
	hpcBe := remote.NewWithFS(&cfg.Target, cfg.Store, cfg.GlobalCfg, laneFS)

	logger := cfg.Logger
	if logger == nil {
		logger = slog.Default()
	}
	logger = logger.With("target", t.Name)

	// Assemble the lane: FIFO prioritizer, submission slots, and remote
	// launcher. Local execution and resource ownership live in runqd.
	q := scheduler.NewQueue()
	pool := resource.NewSlotAllocator(t.MaxInflight)
	launcher := remote.NewLauncher(hpcBe)
	sched := scheduler.New(scheduler.DefaultConfig(), q, pool, launcher, cfg.Store, logger)

	// Terminal transitions found by reconcile/markers flow through the
	// scheduler's FinishTask funnel (retry policy, slot release).
	hpcBe.Finisher = sched

	be := &SSHBackend{
		storeQueries: storeQueries{
			store: cfg.Store,
			reg:   project.NewRegistry(cfg.Store.DB()),
		},
		backend:         hpcBe,
		transportCloser: transportCloser,
		queue:           q,
		pool:            pool,
		sched:           sched,
		launcher:        launcher,
		logger:          logger,
		targetName:      t.Name,
		scope:           store.NewLaneScope(t.Name, t.SemanticGeneration()),
		syncDone:        make(chan struct{}),
		nudgeCh:         make(chan struct{}, 1),
	}
	// One scope, every query surface (RQ-75): the remote backend's probe/
	// marker/heartbeat/orphan reads filter by the same ownership predicate.
	hpcBe.Scope = be.scope
	// Boot counts as activity: after a daemon restart the sensor gets one
	// active window to re-align restored in-flight state before it may
	// hibernate (zero value would mean "hibernated since 1970").
	be.touchActivity()
	return be, nil
}

// Start restores this target's tasks into the lane, starts the scheduler
// loop, and launches the sensor loop. Recovery is a readiness barrier: a
// durable task must never be omitted while the lane reports itself started.
func (b *SSHBackend) Start(ctx context.Context) error {
	if err := b.restoreLane(ctx); err != nil {
		return fmt.Errorf("restore target %s: %w", b.targetName, err)
	}
	b.loopMu.Lock()
	defer b.loopMu.Unlock()
	if b.loopCancel != nil {
		return fmt.Errorf("target %s already started", b.targetName)
	}
	b.sched.Start()
	loopCtx, cancel := context.WithCancel(ctx)
	b.loopCancel = cancel
	b.loopWG.Add(1)
	go b.sensorLoop(loopCtx)
	return nil
}

// restoreLane rebuilds the in-memory queue/slots from the DB on startup:
// tasks with an external id are in flight on the cluster (occupy a slot,
// restored as running); tasks without one are waiting to be (re)launched.
func (b *SSHBackend) restoreLane(ctx context.Context) error {
	rows, err := b.store.ListTasks(ctx, store.TaskFilter{Target: b.backend.Cfg.Name, Scope: b.scope})
	if err != nil {
		return fmt.Errorf("list owned tasks: %w", err)
	}
	jobs, err := b.store.ListJobs(ctx, "", b.targetName)
	if err != nil {
		return fmt.Errorf("list target jobs: %w", err)
	}

	// Recovery is two-phase. First make every required durable repair; only
	// after the whole set succeeds may any queue entry or slot be published.
	// A partial in-memory restore would make the lane appear ready while some
	// durable work had no owner until another restart.
	var repairErrs []error
	repairedJobs := make(map[string]struct{})
	// Ownership filtering happens in SQL via the lane scope (RQ-75); what
	// arrives here is OURS. The active lane additionally ADOPTS what it
	// received from foreign orphan generations ('' legacy rows, hash
	// shifts, crash windows) by RESTAMPING them — ownership is written,
	// never re-inferred, so unfinished counts always add up.
	for i := range rows {
		row := &rows[i]
		switch row.Status {
		case "success", "failed", "killed":
			continue
		}
		if !b.scope.IsRetiring() && row.TargetGeneration != b.scope.Generation {
			previousGeneration := row.TargetGeneration
			if rerr := b.store.RestampTask(ctx, row.ID, b.scope.Generation); rerr != nil {
				repairErrs = append(repairErrs, fmt.Errorf("adopt task %s: %w", row.ID, rerr))
				continue
			}
			row.TargetGeneration = b.scope.Generation
			b.logger.Info("adopted task from an unrecorded generation",
				"task", row.ID, "was", previousGeneration)
		}
		if row.Status == "submitting" && row.StatusSource == "retry" {
			// A manual retry persisted intent before resetting wrapper evidence.
			// The reset is idempotent, so recovery completes it before making
			// the task launchable. Recovery uses the same cross-generation task
			// lock and durable identity fence as the interactive retry path.
			if rerr := b.store.WithTaskAttemptLock(row.ID, func() error {
				current, err := b.store.GetTask(ctx, row.ID)
				if err != nil {
					return err
				}
				if current == nil || current.Status != "submitting" || current.StatusSource != "retry" ||
					current.RetryCount != row.RetryCount || current.TargetGeneration != row.TargetGeneration ||
					current.ExternalID != "" {
					return fmt.Errorf("durable retry intent changed during recovery")
				}
				if err := b.backend.ResetWrapperState(ctx, current.TaskDir, current.ID); err != nil {
					return err
				}
				fields := map[string]any{
					"status_source":  nil,
					"failure_detail": nil,
					"kill_requested": 0,
				}
				store.FenceTaskStatusUpdate(fields, *current)
				return b.store.UpdateTaskStatus(ctx, current.ID, "pending", fields)
			}); rerr != nil {
				repairErrs = append(repairErrs, fmt.Errorf("resume retry reset %s: %w", row.ID, rerr))
				continue
			}
			row.Status = "pending"
			row.StatusSource = ""
			row.FailureDetail = ""
			row.KillRequested = false
		} else if row.Status == "submitting" {
			// The daemon stopped after persisting submit intent but before a
			// durable external-id verdict. The command may have run, so a
			// restart must conservatively retain ownership and must not launch
			// the attempt again.
			if uerr := b.store.UpdateTaskStatus(ctx, row.ID, "unknown", map[string]any{
				"status_source":  "submit",
				"failure_detail": "runq-lab restarted while submission was in flight; remote outcome is unknown",
			}); uerr != nil {
				repairErrs = append(repairErrs, fmt.Errorf("settle interrupted submission %s: %w", row.ID, uerr))
				continue
			}
			row.Status = "unknown"
			row.StatusSource = "submit"
			row.FailureDetail = "runq-lab restarted while submission was in flight; remote outcome is unknown"
		}
		if row.KillRequested && row.Status == "pending" && row.ExternalID == "" {
			if uerr := b.store.UpdateTaskStatus(ctx, row.ID, "killed", map[string]any{
				"status_source":  "runq",
				"finished_at":    time.Now().Unix(),
				"kill_requested": 0,
			}); uerr != nil {
				repairErrs = append(repairErrs, fmt.Errorf("settle recovered pending kill %s: %w", row.ID, uerr))
				continue
			}
			row.Status = "killed"
			row.StatusSource = "runq"
			row.KillRequested = false
			repairedJobs[row.JobID] = struct{}{}
		}
	}
	for jobID := range repairedJobs {
		tasks, lerr := b.store.ListTasks(ctx, store.TaskFilter{JobID: jobID})
		if lerr != nil {
			repairErrs = append(repairErrs, fmt.Errorf("project recovered job %s: %w", jobID, lerr))
			continue
		}
		status, perr := store.ProjectJobStatus(tasks)
		if perr != nil {
			repairErrs = append(repairErrs, perr)
			continue
		}
		if uerr := b.store.UpdateJobStatus(ctx, jobID, status); uerr != nil {
			repairErrs = append(repairErrs, fmt.Errorf("persist recovered job %s: %w", jobID, uerr))
		}
	}
	if err := errors.Join(repairErrs...); err != nil {
		return err
	}
	for _, job := range jobs {
		_, killSettled := repairedJobs[job.ID]
		if job.Status == "paused" && !killSettled {
			b.sched.PauseJob(job.ID)
		}
	}

	restored, queued := 0, 0
	for i := range rows {
		row := &rows[i]
		switch row.Status {
		case "success", "failed", "killed":
			continue
		}
		t := TaskRowToSchedulerTask(row)
		switch {
		case row.Status == "unknown":
			b.pool.Reserve(row.ID)
			t.Status = scheduler.StatusUnknown
			b.queue.Restore(t)
			restored++
		case row.ExternalID != "":
			b.pool.Reserve(row.ID)
			t.Status = scheduler.StatusRunning
			b.queue.Restore(t)
			restored++
		default:
			b.queue.Push(t)
			queued++
		}
		if row.KillRequested {
			b.sched.RestoreKillRequest(row.ID)
		}
	}
	if restored+queued > 0 {
		b.logger.Info("remote lane restored", "in_flight", restored, "queued", queued)
	}
	return nil
}

// sensorLoop is the SINGLE background sensor goroutine for this target —
// one ticker, sequential work, no sensor-vs-sensor concurrency by shape:
//
//   - every tick (markerScanInterval): done-marker scan (one readdir, cheap;
//     the primary completion path)
//   - every probeAlignInterval: batch scheduler probe (catches tasks whose
//     wrapper never wrote a marker — node fail, hard kill — so their slots
//     don't leak) + background orphan detection (two-strike hysteresis)
//
// Both gated on HasInFlight: zero in-flight tasks = zero SSH traffic.
// Request-driven verdict paths (kill, manual retry, user-triggered refresh)
// still arrive on API goroutines; the target's lifecycleMu serializes them
// with this loop.
func (b *SSHBackend) sensorLoop(ctx context.Context) {
	defer b.loopWG.Done()
	ticker := time.NewTicker(markerScanInterval)
	defer ticker.Stop()
	lastAlign := time.Now()  // warm start: no immediate qstat on daemon boot
	lastIngest := time.Now() // metrics ingest rides the same loop, own cadence
	tick := 0
	for {
		select {
		case <-ctx.Done():
			return
		case <-b.nudgeCh:
			// Forced full pass (SWR nudge or explicit refresh): runs even
			// with zero in-flight — the user asked, and a no-op pass is a
			// cheap honest answer. Completes a sync generation so
			// ForceRefresh waiters wake.
			b.forcedSync(ctx)
		case <-ticker.C:
			tick++
			if inflight, err := b.backend.HasInFlight(ctx); err != nil || !inflight {
				continue
			}
			b.replayKillIntents(ctx)
			active := b.userActive()

			// Marker scan: every tick while active, every Nth while
			// hibernated — cheap enough to keep an overnight sweep moving.
			if !active && tick%hibernatedMarkerEvery != 0 {
				continue
			}
			if err := b.backend.ScanDoneMarkers(ctx); errors.Is(err, remote.ErrMarkerDetectionDisabled) {
				// No marker source is configured. This is not an observation and
				// must not overwrite scheduler-derived freshness every tick.
			} else if err != nil {
				b.logger.Warn("marker scan failed", "error", err)
				b.recordTasksSync(err)
			} else {
				// A successful marker scan IS an observation of the remote
				// world (the primary completion path) — it advances the
				// photo's timestamp.
				b.recordTasksSync(nil)
			}

			// Metrics ingest (spec §8.1.4): long-interval background pass so
			// running tasks' metrics land in SQLite as a steady trickle —
			// reads are pure SQL, no cold-start parse spike. Runs while
			// hibernated too (data collection needs no observer; each pass
			// is a stat per running task + delta-only reads), at the
			// hibernated marker cadence it already inherits from the gate
			// above.
			if time.Since(lastIngest) >= metricsIngestInterval {
				lastIngest = time.Now()
				b.ingestRunningMetrics(ctx)

				// Heartbeat (file-stream liveness) rides the same cadence:
				// ≤3 stats per running task. Silence is only a SUSPICION —
				// the escalation asks the scheduler, and the etiquette
				// floor still applies even to suspicions.
				if silent, herr := b.backend.HeartbeatProbe(ctx, taskSilenceAfter); herr == nil && len(silent) > 0 {
					b.logger.Info("heartbeat: silent tasks, consulting scheduler", "tasks", silent)
					if perr := b.backend.SchedulerProbe(ctx, schedProbeFloor); perr != nil {
						b.logger.Warn("heartbeat escalation probe failed", "error", perr)
						b.recordTasksSync(perr)
					} else {
						b.recordTasksSync(nil)
					}
				}
			}

			// qstat alignment: user-present periods only.
			if !active || time.Since(lastAlign) < probeAlignInterval {
				continue
			}
			lastAlign = time.Now()
			if err := b.backend.SchedulerProbe(ctx, DefaultReadTTL); err != nil {
				b.logger.Warn("probe align failed", "error", err)
				b.recordTasksSync(err)
			} else {
				b.recordTasksSync(nil)
			}
			if err := b.backend.DetectOrphans(ctx, false); err != nil {
				b.logger.Warn("orphan detection failed", "error", err)
			}
		}
	}
}

// replayKillIntents retries durable cancellations recovered after restart or
// left pending by a transient scheduler error. Intents without an external id
// remain owned and are settled by later evidence (or the explicit unknown
// escape hatch); they are never converted into a false cancellation.
func (b *SSHBackend) replayKillIntents(ctx context.Context) {
	rows, err := b.store.ListTasks(ctx, store.TaskFilter{Target: b.targetName, Scope: b.scope})
	if err != nil {
		b.logger.Warn("list durable kill intents failed", "error", err)
		return
	}
	for i := range rows {
		row := &rows[i]
		terminal := row.Status == "success" || row.Status == "failed" || row.Status == "killed"
		if !row.KillRequested || row.ExternalID == "" || terminal {
			continue
		}
		if _, err := b.backend.Kill(ctx, row.ID); err != nil {
			b.logger.Warn("replay durable kill intent failed", "task", row.ID, "error", err)
		}
	}
}

// ingestRunningMetrics runs one incremental metrics pass over this lane's
// running tasks. Each task costs one stat when idle, delta-only reads when
// growing — the spread-out替代 of a read-time parse storm.
func (b *SSHBackend) ingestRunningMetrics(ctx context.Context) {
	rows, err := b.store.ListTasks(ctx, store.TaskFilter{Status: "running", Target: b.targetName, Scope: b.scope})
	if err != nil {
		return
	}
	for i := range rows {
		loaded := rows[i]
		if loaded.TaskDir == "" {
			continue
		}
		err := b.store.WithTaskAttemptLock(loaded.ID, func() error {
			current, err := b.store.GetTask(ctx, loaded.ID)
			if err != nil {
				return err
			}
			if current == nil || current.Status != "running" ||
				current.RetryCount != loaded.RetryCount ||
				current.TargetGeneration != loaded.TargetGeneration ||
				current.ExternalID != loaded.ExternalID {
				return nil
			}
			_, err = ingest.ReapIncremental(ctx, b.store, ingest.Target{
				TaskID: current.ID, JobID: current.JobID, Dir: current.TaskDir, FS: b.backend.FS,
			}, false)
			return err
		})
		if err != nil {
			b.logger.Debug("metrics ingest failed", "task", loaded.ID, "error", err)
		}
	}
}

// DetectOrphansNow runs an immediate (no-hysteresis) orphan scan. Called by
// the interactive clean path, where the user visually confirms every entry.
func (b *SSHBackend) DetectOrphansNow(ctx context.Context) error {
	return b.backend.DetectOrphans(ctx, true)
}

// Generation is the semantic hash of the target config this lane was
// built from (RQ-75) — the ownership stamp on its tasks.
func (b *SSHBackend) Generation() string { return b.scope.Generation }

// MarkRetiring flips this lane into retiring mode (RQ-75): the launch
// path is permanently quiesced, and restoreLane confines itself to the
// lane's own generation (never queueing pending work — that belongs to
// the active generation). Must be called BEFORE Start when rebuilding a
// retiring lane after a restart.
// MarkRetiring narrows this lane's ownership scope to its own generation
// (RQ-75 round 5, user design: snapshot isolation — NO forwarding, NO
// kill). A retiring lane keeps its FULL scheduling loop: pending tasks
// submitted under this config run under this config; runq does not
// second-guess the user. The lane closes once its non-terminal count
// stays at zero. Only rerun resolves to the newest generation
// (explicit, confirmed cross-generation retry).
func (b *SSHBackend) MarkRetiring() { b.scope.MarkRetiring() }

// PromoteActive reverses MarkRetiring (round 6 #4): the config changed
// BACK to this lane's generation (A→B→A, or a removed target re-added
// unchanged), so instead of building a twin lane with the SAME content
// hash — two lanes would co-own every row — the retiring lane is
// promoted back to active. Same object, same queue, scope widened.
func (b *SSHBackend) PromoteActive() { b.scope.ResumeActive() }

// QuiesceForHistory stops control/reconcile loops while deliberately keeping
// the filesystem usable for terminal logs and metric artifacts. Retirement
// calls it only after the generation has no unfinished tasks.
func (b *SSHBackend) QuiesceForHistory() {
	b.loopMu.Lock()
	defer b.loopMu.Unlock()
	if b.loopCancel != nil {
		b.loopCancel()
		b.loopWG.Wait()
		b.sched.Shutdown()
		b.loopCancel = nil
	}
}

// acquireArtifactLease pins the transport for a long-lived artifact reader.
// The routing barrier protects lease acquisition against a concurrent Close;
// after acquisition, Close may return without severing the reader.
func (b *SSHBackend) acquireArtifactLease() (func(), error) {
	b.artifactMu.Lock()
	defer b.artifactMu.Unlock()
	if b.closeRequested {
		return nil, fmt.Errorf("target %s lane is closing", b.targetName)
	}
	b.artifactRefs++
	var once sync.Once
	return func() {
		once.Do(func() {
			b.artifactMu.Lock()
			b.artifactRefs--
			shouldClose := b.closeRequested && b.artifactRefs == 0 && !b.transportClosed
			b.artifactMu.Unlock()
			if shouldClose {
				if err := b.closeTransportIfIdle(); err != nil {
					b.logger.Warn("deferred lane transport close failed", "error", err)
				}
			}
		})
	}, nil
}

func (b *SSHBackend) closeTransportIfIdle() error {
	b.artifactMu.Lock()
	if !b.closeRequested || b.artifactRefs != 0 || b.transportClosed {
		err := b.transportCloseErr
		b.artifactMu.Unlock()
		return err
	}
	b.transportClosed = true
	closer := b.transportCloser
	b.artifactMu.Unlock()

	var err error
	if closer != nil {
		err = closer.Close()
	}
	b.artifactMu.Lock()
	b.transportCloseErr = err
	b.artifactMu.Unlock()
	return err
}

// Close quiesces the lane immediately, then releases its transport once all
// returned log followers have closed. Must be called on daemon shutdown.
func (b *SSHBackend) Close() error {
	b.QuiesceForHistory()
	b.artifactMu.Lock()
	b.closeRequested = true
	b.artifactMu.Unlock()
	return b.closeTransportIfIdle()
}

// ── Capabilities ──────────────────────────────────────────────────────────

func (b *SSHBackend) Capabilities() Capabilities {
	return Capabilities{
		GPUMap:        b.backend.Cfg.GPUTemplate != "", // gpu_template-driven (runq preset)
		PauseResume:   true,                            // runq-level dispatch gate (in-flight work continues)
		LiveLog:       true,                            // logs readable via SSH
		Retry:         true,                            // scheduler lane re-runs submit.cmd (RQ-46)
		StateModel:    "poll",                          // best-effort projection; staleness surfaced
		KillAsync:     true,                            // qdel/scancel forwarded
		SubmitPreview: true,                            // zero-disk dry-run via submit code path
	}
}

// DryRun overrides storeQueries: the workspace-root preview must come from
// THIS lane's decision point (target workspace), not the client's global
// config (RQ-65 — dry-run confirming /tmp/.runq while submit writes to the
// cluster's runq-workspaces).
func (b *SSHBackend) DryRun(ctx context.Context, cfg job.JobConfig) (*DryRunResult, error) {
	return BuildDryRunResult(cfg, func(name string) (*project.Config, error) {
		return b.reg.Get(ctx, name)
	}, func(proj *project.Config) string {
		root, _ := b.backend.WorkspaceRoot(proj, false)
		return root
	})
}

// ── Reconcile ─────────────────────────────────────────────────────────────

func (b *SSHBackend) RefreshJob(ctx context.Context, jobID string) error {
	b.touchActivity()
	// Local reconcile (status.json, markers) always runs; the scheduler
	// probe respects the qstat etiquette floor — same split as forcedSync.
	err := b.backend.EnsureFresh(ctx, jobID, schedProbeFloor)
	b.recordTasksSync(err)
	return err
}

// ReconcileAll runs a full reconcile pass over all active jobs. Called by
// the dashboard's background ticker — never by the list endpoint itself.
func (b *SSHBackend) ReconcileAll(ctx context.Context) error {
	err := b.backend.SchedulerProbe(ctx, DefaultReadTTL)
	b.recordTasksSync(err)
	return err
}

// ── Job operations ────────────────────────────────────────────────────────

func (b *SSHBackend) ListJobs(ctx context.Context, projectScope string) ([]JobSummary, error) {
	b.touchActivity()
	target := b.backend.Cfg.Name
	jobs, err := b.store.ListJobsVisible(ctx, projectScope, target)
	if err != nil {
		return nil, err
	}
	var observationErrs []error
	for _, j := range jobs {
		if !store.IsTerminalJobStatus(j.Status) {
			if err := b.backend.EnsureFresh(ctx, j.ID, DefaultReadTTL); err != nil {
				observationErrs = append(observationErrs, fmt.Errorf("refresh job %s: %w", j.ID, err))
			}
		}
	}
	if err := errors.Join(observationErrs...); err != nil {
		// Listing remains best-effort, but the envelope must not stamp the
		// returned snapshot fresh after a synchronous observation failed.
		b.recordTasksSync(err)
	}
	// Re-query after reconcile.
	jobs, err = b.store.ListJobsVisible(ctx, projectScope, target)
	if err != nil {
		return nil, err
	}
	jobIDs := make([]string, len(jobs))
	for i, j := range jobs {
		jobIDs[i] = j.ID
	}
	allTasks, err := b.store.ListTasksForJobs(ctx, jobIDs)
	if err != nil {
		return nil, err
	}
	byJob := make(map[string][]store.TaskRow, len(jobs))
	for _, t := range allTasks {
		byJob[t.JobID] = append(byJob[t.JobID], t)
	}
	out := make([]JobSummary, 0, len(jobs))
	for _, j := range jobs {
		out = append(out, BuildJobSummary(j, byJob[j.ID]))
	}
	return out, nil
}

func (b *SSHBackend) GetJob(ctx context.Context, jobID string) (*JobDetail, error) {
	b.touchActivity()
	if err := b.backend.EnsureFresh(ctx, jobID, DefaultReadTTL); err != nil {
		return nil, err
	}
	j, err := b.store.GetJob(ctx, jobID)
	if err != nil {
		return nil, err
	}
	if j == nil {
		return nil, fmt.Errorf("job %q: %w", jobID, ErrNotFound)
	}
	// UNscoped: GetJob is a USER VIEW — it must show the whole job,
	// including tasks a retiring generation still tracks (review round 3).
	tasks, err := b.store.ListTasks(ctx, store.TaskFilter{JobID: jobID})
	if err != nil {
		return nil, err
	}
	keys, _ := b.store.MetricKeys(ctx, jobID) // summaries: exact, remote-safe
	detail := BuildJobDetail(*j, tasks, keys)
	if cfg, err := b.reg.Get(ctx, j.ProjectName); err == nil && cfg.Wandb != nil {
		detail.Wandb = &WandbInfo{
			Entity:  cfg.Wandb.Entity,
			Project: cfg.Wandb.Project,
			BaseURL: WandbBaseURL(cfg.Wandb.Entity, cfg.Wandb.Project),
		}
	}
	return &detail, nil
}

// CompareMetrics ranks tasks by their best ingested value — pure SQL over
// the metrics table (spec §8.1.4); EnsureFresh keeps statuses honest and
// its verdict path runs the final ingest for finished tasks.
func (b *SSHBackend) CompareMetrics(ctx context.Context, jobID, key string, desc bool) ([]CompareRow, error) {
	if err := b.backend.EnsureFresh(ctx, jobID, DefaultReadTTL); err != nil {
		return nil, err
	}
	return compareRowsFromDB(ctx, b.store, jobID, key, desc)
}

// MetricKeys is the discovery half of the metrics dual-mode (spec §5.4):
// SELECT DISTINCT over ingested rows.
func (b *SSHBackend) MetricKeys(ctx context.Context, jobID string) ([]string, error) {
	b.touchActivity()
	return b.store.MetricKeys(ctx, jobID)
}

// gpuCacheTTL bounds how often dashboard GPU-panel polling can turn into an
// actual SSH exec on this target.
const gpuCacheTTL = 10 * time.Second

// GPUStatus reports this target's GPUs through its optional gpu_template
// (runq preset: `runq gpu --json`). No template = no visibility = empty.
// Observation failures are empty-not-error: the aggregated panel must not
// go dark because one cluster hiccuped.
func (b *SSHBackend) GPUStatus(ctx context.Context) ([]GPUSlot, error) {
	tmpl := b.backend.Cfg.GPUTemplate
	if tmpl == "" {
		return []GPUSlot{}, nil
	}
	b.touchActivity()

	b.gpuMu.Lock()
	defer b.gpuMu.Unlock()
	if b.gpuCache != nil && time.Since(b.gpuCacheAt) < gpuCacheTTL {
		return b.gpuCache, nil
	}

	// stdout only — stderr noise (motd, activation chatter) must not reach
	// the JSON parser.
	stdout, _, code, err := b.backend.FS.Exec(ctx, "sh", "-c", tmpl)
	if err != nil || code != 0 {
		return []GPUSlot{}, nil
	}
	var slots []GPUSlot
	if uerr := json.Unmarshal(bytes.TrimSpace(stdout), &slots); uerr != nil {
		return []GPUSlot{}, nil
	}
	for i := range slots {
		slots[i].Target = b.backend.Cfg.Name
	}
	b.gpuCache = slots
	b.gpuCacheAt = time.Now()
	return slots, nil
}

// ── Task operations ───────────────────────────────────────────────────────

func (b *SSHBackend) loadOwnedTask(ctx context.Context, taskID, operation string) (*store.TaskRow, error) {
	task, err := b.store.GetTask(ctx, taskID)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", operation, err)
	}
	if task == nil {
		return nil, fmt.Errorf("task %q: %w", taskID, ErrNotFound)
	}
	if !b.scope.Owns(task.Target, task.TargetGeneration) {
		return nil, fmt.Errorf("task %q moved to target generation %q; lane %q no longer owns it",
			taskID, task.TargetGeneration, b.scope.Generation)
	}
	return task, nil
}

func (b *SSHBackend) GetTask(ctx context.Context, taskID string) (*TaskView, error) {
	b.touchActivity()
	task, err := b.store.GetTask(ctx, taskID)
	if err != nil {
		return nil, err
	}
	if task == nil {
		return nil, fmt.Errorf("task %q: %w", taskID, ErrNotFound)
	}
	b.reconcileTask(ctx, taskID, task)
	var view TaskView
	err = b.store.WithTaskAttemptLock(taskID, func() error {
		owned, err := b.loadOwnedTask(ctx, taskID, "get task")
		if err != nil {
			return err
		}
		view = BuildTaskView(*owned)
		applyTaskDetail(&view, *owned)
		return nil
	})
	if err != nil {
		return nil, err
	}
	return &view, nil
}

// FS exposes this lane's filesystem (LocalFS or SSHFS — the lane doesn't
// know the difference, and neither should callers).
func (b *SSHBackend) FS() rfs.FS { return b.backend.FS }

// RecordContactOK forwards a daemon-observed reachability proof to the
// lane's passive contact record (RQ-74) — e.g. the remote CLI forward
// session coming up after `runq connect`.
func (b *SSHBackend) RecordContactOK() { b.backend.RecordContactOK() }

// TargetHealth is this lane's passive reachability row for /health (D6):
// a snapshot of the most recent transport outcome — never a probe.
func (b *SSHBackend) TargetHealth() TargetHealth {
	at, ok, lastErr := b.backend.LastContact()
	h := TargetHealth{Name: b.targetName, Reachable: ok, LastError: lastErr}
	if !at.IsZero() {
		h.LastChecked = at.Unix()
	} else {
		h.Reachable = false // no contact since boot: unknown ≠ reachable
	}
	return h
}

// ── L4: freshness, SWR, forced refresh ────────────────────────────────────

// recordTasksSync persists one 'tasks' sync outcome. Background context:
// the recording must not die with the (possibly canceled) pass it records.
func (b *SSHBackend) recordTasksSync(outcome error) {
	if err := b.store.RecordSyncOutcome(context.Background(), b.targetName, b.tasksSyncResource(), outcome); err != nil {
		b.logger.Debug("sync_state write failed", "error", err)
	}
}

func (b *SSHBackend) tasksSyncResource() string {
	if generation := b.Generation(); generation != "" {
		return "tasks:" + generation
	}
	return "tasks"
}

// completeSyncCycle bumps the generation and wakes every waiter.
func (b *SSHBackend) completeSyncCycle() {
	b.syncMu.Lock()
	b.syncGen++
	close(b.syncDone)
	b.syncDone = make(chan struct{})
	b.syncMu.Unlock()
}

// forcedSync runs one full pass NOW (marker scan always; SchedulerProbe
// respects schedProbeFloor — a forced refresh bypasses the 15min ALIGNMENT
// cadence, never the qstat etiquette floor),
// records the outcome, and completes a generation. Runs on the sensor
// goroutine — serialized with regular ticks by construction.
func (b *SSHBackend) forcedSync(ctx context.Context) {
	markerErr := b.backend.ScanDoneMarkers(ctx)
	probeErr := b.backend.SchedulerProbe(ctx, schedProbeFloor)
	err := markerErr
	if markerErr == nil || errors.Is(markerErr, remote.ErrMarkerDetectionDisabled) {
		err = probeErr
	}
	b.recordTasksSync(err)
	b.completeSyncCycle()
}

// SyncInfo reports this lane's data freshness (envelope refreshed_at/stale)
// and doubles as the SWR trigger: a read that finds soft-stale data nudges
// the sensor and returns immediately — the CALLER never waits on SSH.
//
// stale semantics: with zero in-flight tasks the store is authoritative
// (nothing remote can change it), so data is never stale no matter how old
// the last sync — age only starts to matter when there is something to
// observe.
func (b *SSHBackend) SyncInfo(ctx context.Context) (refreshedAt int64, stale bool) {
	row, err := b.store.GetSyncState(ctx, b.targetName, b.tasksSyncResource())
	if err != nil || row == nil {
		return 0, false
	}
	refreshedAt = row.LastSuccess
	inflight, ierr := b.backend.HasInFlight(ctx)
	if ierr != nil || !inflight {
		return refreshedAt, false
	}
	age := time.Since(time.Unix(row.LastSuccess, 0))
	if age > syncSoftTTL {
		select {
		case b.nudgeCh <- struct{}{}:
		default: // a pass is already queued — coalesce
		}
	}
	return refreshedAt, age > syncHardTTL || row.LastError != ""
}

// ForceRefresh kicks one full pass and blocks until the pass that STARTED
// after the kick completes (generation fence), the min_interval floor
// rejects it, or ctx times out. Every exit returns an honest receipt.
func (b *SSHBackend) ForceRefresh(ctx context.Context) (*RefreshReceipt, error) {
	b.touchActivity()

	b.syncMu.Lock()
	if since := time.Since(b.lastForced); since < refreshMinInterval {
		b.syncMu.Unlock()
		receipt := b.currentReceipt(ctx, false, "min_interval")
		receipt.RetryAfterSeconds = int64((refreshMinInterval - since).Seconds()) + 1
		return receipt, nil
	}
	b.lastForced = time.Now()
	g0 := b.syncGen
	done := b.syncDone
	b.syncMu.Unlock()

	select {
	case b.nudgeCh <- struct{}{}:
	default:
	}

	for {
		select {
		case <-ctx.Done():
			return b.currentReceipt(context.Background(), false, "timeout"), nil
		case <-done:
		}
		b.syncMu.Lock()
		if b.syncGen > g0 {
			b.syncMu.Unlock()
			break
		}
		done = b.syncDone
		b.syncMu.Unlock()
	}

	row, _ := b.store.GetSyncState(ctx, b.targetName, b.tasksSyncResource())
	if row != nil && row.LastError != "" {
		r := b.currentReceipt(ctx, false, row.LastError)
		return r, nil
	}
	return b.currentReceipt(ctx, true, ""), nil
}

// currentReceipt assembles a receipt from the persisted freshness row.
func (b *SSHBackend) currentReceipt(ctx context.Context, refreshed bool, reason string) *RefreshReceipt {
	receipt := &RefreshReceipt{Refreshed: refreshed, Reason: reason}
	if row, err := b.store.GetSyncState(ctx, b.targetName, b.tasksSyncResource()); err == nil && row != nil {
		receipt.RefreshedAt = row.LastSuccess
	}
	return receipt
}

// ListTasks — the lane defaults the target scope to itself (its slice of
// the shared store); an explicit opts.Target overrides.
func (b *SSHBackend) ListTasks(ctx context.Context, opts TaskListOptions) ([]TaskView, int, error) {
	b.touchActivity()
	if opts.Target == "" {
		opts.Target = b.targetName
	}
	return listTasksFromStore(ctx, b.store, opts)
}

// TaskMetrics serves the chart from the task-lifetime RAW TAIL WINDOW of
// metrics.jsonl (one ranged read — recent data is small by construction).
// It is deliberately side-effect free: background/reconcile paths own SQL
// ingestion, while this read shares the task-attempt lock with retry reset
// and ingestion so it cannot observe a torn attempt boundary. afterTS > 0
// is the dashboard's incremental pull cursor.
func (b *SSHBackend) TaskMetrics(ctx context.Context, taskID string, afterTS int64) ([]MetricPoint, error) {
	b.touchActivity()
	var points []MetricPoint
	err := b.store.WithTaskAttemptLock(taskID, func() error {
		task, err := b.loadOwnedTask(ctx, taskID, "task metrics")
		if err != nil {
			return err
		}
		points = readTailMetricPoints(b.backend.FS, task.TaskDir, "", 2000, afterTS)
		return nil
	})
	return points, err
}

// TaskMetricBuckets — bucket-mode chart (spec §6.4): terminal tasks read
// the on-target pyramid (1–O(log n) rfs ranged reads) only when the normal
// wrapper completed and rebuilt it. A retry appends to the task-lifetime raw
// stream while the prior attempt's pyramid still exists, and TERM cancellation
// skips the builder, so every other state must use tail-window aggregation.
// The fallback uses the SAME merge operator.
func (b *SSHBackend) TaskMetricBuckets(ctx context.Context, taskID, key string, fromTS, toTS int64, maxBuckets int) ([]workspace.PyramidBucket, string, error) {
	b.touchActivity()
	var (
		buckets []workspace.PyramidBucket
		source  string
	)
	err := b.store.WithTaskAttemptLock(taskID, func() error {
		task, err := b.loadOwnedTask(ctx, taskID, "task metric buckets")
		if err != nil {
			return err
		}
		if task.StatusSource != remote.SourceWrapper || (task.Status != "success" && task.Status != "failed") {
			buckets = tailMetricBuckets(b.backend.FS, task.TaskDir, key, fromTS, toTS, maxBuckets)
			source = "tail"
			return nil
		}
		buckets, err = workspace.QueryPyramid(ctx, b.backend.FS, task.TaskDir, key, fromTS, toTS, maxBuckets)
		switch {
		case err == nil:
			source = "pyramid"
			return nil
		case errors.Is(err, workspace.ErrPyramidNotBuilt):
			buckets = tailMetricBuckets(b.backend.FS, task.TaskDir, key, fromTS, toTS, maxBuckets)
			source = "tail"
			return nil
		default:
			return err // e.g. key not indexed — a real answer, not a fallback case
		}
	})
	return buckets, source, err
}

func (b *SSHBackend) reconcileTask(ctx context.Context, taskID string, fallback *store.TaskRow) *store.TaskRow {
	err := b.backend.EnsureFresh(ctx, fallback.JobID, DefaultReadTTL)
	if err != nil {
		err = fmt.Errorf("refresh task %s: %w", taskID, err)
		b.recordTasksSync(err)
		return fallback
	}
	b.recordTasksSync(nil)
	if fresh, err := b.store.GetTask(ctx, taskID); err == nil && fresh != nil {
		return fresh
	}
	return fallback
}

func (b *SSHBackend) KillTask(ctx context.Context, taskID string) error {
	b.touchActivity()
	task, err := b.store.GetTask(ctx, taskID)
	if err != nil {
		return err
	}
	if task == nil {
		return fmt.Errorf("task %q: %w", taskID, ErrNotFound)
	}
	if !b.scope.Owns(task.Target, task.TargetGeneration) {
		return fmt.Errorf("task %q moved to target generation %q; lane %q no longer owns it",
			taskID, task.TargetGeneration, b.scope.Generation)
	}
	if settled, err := b.sched.TryKillPending(taskID, "runq"); err != nil {
		return err
	} else if settled {
		return nil
	}
	// A manual retry can be between durable intent and pending publication.
	// It owns no external process, so cancellation may terminalize that intent
	// under the same lifecycle lock as RetryTask.
	if settled, err := b.cancelRetryIntent(ctx, taskID); err != nil {
		return err
	} else if settled {
		return nil
	}
	// Retry may have completed while cancelRetryIntent waited for the lock.
	if settled, err := b.sched.TryKillPending(taskID, "runq"); err != nil {
		return err
	} else if settled {
		return nil
	}
	if qt := b.queue.Get(taskID); qt != nil && (qt.Status == scheduler.StatusRunning || qt.Status == scheduler.StatusUnknown) {
		if err := b.sched.RequestKill(taskID); err != nil {
			return err
		}
	}
	if err := b.backend.EnsureFresh(ctx, task.JobID, 0); err != nil {
		return fmt.Errorf("reconcile before kill: %w", err)
	}
	task, err = b.store.GetTask(ctx, taskID)
	if err != nil {
		return err
	}
	if task == nil || task.Status == "success" || task.Status == "failed" || task.Status == "killed" {
		return nil
	}
	if !b.scope.Owns(task.Target, task.TargetGeneration) {
		return fmt.Errorf("task %q moved to target generation %q during kill; lane %q no longer owns it",
			taskID, task.TargetGeneration, b.scope.Generation)
	}
	// Reconciliation may have legally requeued a failed attempt.
	if settled, err := b.sched.TryKillPending(taskID, "runq"); err != nil {
		return err
	} else if settled {
		return nil
	}
	if qt := b.queue.Get(taskID); qt != nil && (qt.Status == scheduler.StatusRunning || qt.Status == scheduler.StatusUnknown) {
		if err := b.sched.RequestKill(taskID); err != nil {
			return err
		}
	}
	// RQ-74: an `unknown` task with no external id has nothing to cancel and
	// no verdict on the horizon if the submission truly never landed — the
	// user's explicit kill is the escape hatch. Settling killed here is a
	// DELIBERATE deviation from "killed only when no unmanaged job can
	// exist": the user is taking responsibility for the (reconcile-checked,
	// still-unresolved) residual risk. If a phantom cluster job does run,
	// its marker is later swept as stale bookkeeping.
	if task.Status == "unknown" && task.ExternalID == "" {
		if qt := b.queue.Get(taskID); qt != nil && qt.Status == scheduler.StatusUnknown {
			if err := b.sched.FinishTaskChecked(qt, scheduler.StatusKilled, map[string]any{"status_source": "runq"}); err != nil {
				return fmt.Errorf("persist unknown task kill: %w", err)
			}
			return nil
		}
	}
	_, err = b.backend.Kill(ctx, taskID)
	return err
}

// cancelRetryIntent handles the durable pre-effect phase of a manual retry.
// No external submission can exist yet, so a user kill may settle it directly.
func (b *SSHBackend) cancelRetryIntent(ctx context.Context, taskID string) (bool, error) {
	settled := false
	err := b.backend.WithLifecycleLock(func() error {
		return b.store.WithTaskAttemptLock(taskID, func() error {
			row, err := b.store.GetTask(ctx, taskID)
			if err != nil {
				return err
			}
			if row == nil || row.Status != "submitting" || row.StatusSource != "retry" || row.ExternalID != "" {
				return nil
			}
			if !b.scope.Owns(row.Target, row.TargetGeneration) {
				return nil
			}
			fields := map[string]any{
				"status_source":  "runq",
				"finished_at":    time.Now().Unix(),
				"kill_requested": 0,
			}
			store.FenceTaskStatusUpdate(fields, *row)
			if err := b.store.UpdateTaskStatus(ctx, taskID, "killed", fields); err != nil {
				return fmt.Errorf("persist retry-intent kill: %w", err)
			}
			var publishErr error
			if qt := b.queue.Get(taskID); qt != nil {
				publishErr = b.queue.Complete(taskID, scheduler.StatusKilled)
			}
			b.sched.ClearKillRequest(taskID)
			b.sched.RefreshJobStatus(row.JobID)
			settled = true
			return publishErr
		})
	})
	return settled, err
}

// ── RQ-44: log access（核心实现 — Human 主笔，CC review）────────────────────
//
// TaskLogRead — 契约修订版（与 Human 对齐）：字节锚点 + 行数量 → *LogPage。
// 字节做锚（O(1) seek / 断线续传 / 热力图跳转），行做量（UI 渲染单位，
// 断行对齐在 logfile 层）。logfile.Reader 正好是这个模型：
// Open(path, fs) → ReadLines(offset, maxLines)。单行长度与单页字节预算
// 在 logfile 内 clamp（病态长行防御——review 重点）。
func (b *SSHBackend) TaskLogRead(ctx context.Context, taskID string, offset int64, maxLines int) (*LogPage, error) {
	b.touchActivity()
	var page *LogPage
	err := b.store.WithTaskAttemptLock(taskID, func() error {
		task, err := b.loadOwnedTask(ctx, taskID, "task log read")
		if err != nil {
			return err
		}
		r, err := logfile.Open(task.LogPath, b.backend.FS)
		if err != nil {
			if errors.Is(err, fs.ErrNotExist) {
				page = &LogPage{Lines: []string{}} // pending: empty page, Size 0
				return nil
			}
			return err
		}
		defer r.Close()
		page, err = r.ReadLines(offset, maxLines)
		return err
	})
	return page, err
}

// TaskLogTail — 尾部视图（每次首屏的入口）：解析 + 委托 TailLines。
// 返回页的 Offset 即向上翻页锚点。
func (b *SSHBackend) TaskLogTail(ctx context.Context, taskID string, maxLines int) (*LogPage, error) {
	b.touchActivity()
	var page *LogPage
	err := b.store.WithTaskAttemptLock(taskID, func() error {
		task, err := b.loadOwnedTask(ctx, taskID, "task log tail")
		if err != nil {
			return err
		}
		r, err := logfile.Open(task.LogPath, b.backend.FS)
		if err != nil {
			if errors.Is(err, fs.ErrNotExist) {
				page = &LogPage{Lines: []string{}} // pending: empty page, Size 0
				return nil
			}
			return err
		}
		defer r.Close()
		page, err = r.TailLines(maxLines)
		return err
	})
	return page, err
}

// TaskLogPage — dashboard log contract v2: byte-budget page (positional /
// tail / rotation / optional line count) through the owning target's FS.
func (b *SSHBackend) TaskLogPage(ctx context.Context, taskID string, req logfile.PageRequest) (*LogPage, error) {
	b.touchActivity()
	var page *LogPage
	err := b.store.WithTaskAttemptLock(taskID, func() error {
		task, err := b.loadOwnedTask(ctx, taskID, "task log page")
		if err != nil {
			return err
		}
		r, err := logfile.Open(task.LogPath, b.backend.FS)
		if err != nil {
			if errors.Is(err, fs.ErrNotExist) {
				page = &LogPage{Lines: []string{}, TotalLines: -1, StartLine: -1} // pending
				return nil
			}
			return err
		}
		defer r.Close()
		page, err = r.ReadPage(req)
		return err
	})
	return page, err
}

// TaskLogFollow — pure assembly: resolve the task, hand path+FS+offset to
// logfile.Follow. *logfile.Follower satisfies LogFollower natively (LogPage
// = logfile.Page), so there is no adapter and deliberately NO goroutine:
// LogFollower is a PULL iterator, driven by the consumer's loop (SSE
// handler, CLI logs -f). offset passes through unclamped: size < offset IS
// the rotation signal. Pending log (no file yet) is handled inside
// Follower (lazy open). No lifecycleMu (read-only, produces no verdicts).
type leasedLogFollower struct {
	inner   LogFollower
	release func()
	once    sync.Once
}

func (f *leasedLogFollower) Next(ctx context.Context) (*LogPage, error) {
	return f.inner.Next(ctx)
}

func (f *leasedLogFollower) Close() error {
	err := f.inner.Close()
	f.once.Do(f.release)
	return err
}

func (b *SSHBackend) TaskLogFollow(ctx context.Context, taskID string, offset int64) (LogFollower, error) {
	b.touchActivity()
	release, err := b.acquireArtifactLease()
	if err != nil {
		return nil, err
	}
	var follower LogFollower
	err = b.store.WithTaskAttemptLock(taskID, func() error {
		task, err := b.loadOwnedTask(ctx, taskID, "task log follow")
		if err != nil {
			return err
		}
		follower, err = logfile.Follow(task.LogPath, b.backend.FS, offset)
		return err
	})
	if err != nil {
		release()
		return nil, fmt.Errorf("task log follow: %w", err)
	}
	return &leasedLogFollower{inner: follower, release: release}, nil
}

// JobLogSearch greps the job's logs on the owning side (RQ-44): one
// batched grep over FS.Exec — results travel, files don't.
func (b *SSHBackend) JobLogSearch(ctx context.Context, jobID, query string) ([]LogMatch, error) {
	b.touchActivity()
	return jobLogSearchViaExec(ctx, b.store, b.backend.FS, jobID, query)
}

// JobActivity decimates activity.tsv on the owning side — same RQ-44
// principle as JobLogSearch: results travel, files don't.
func (b *SSHBackend) JobActivity(ctx context.Context, jobID string) (*JobActivity, error) {
	b.touchActivity()
	return jobActivityViaExec(ctx, b.store, b.backend.FS, jobID)
}

// JobResults is pure SQL over ingested result_records; EnsureFresh runs
// the refresh reap first so results.jsonl deltas on the cluster have
// landed (same posture as CompareMetrics).
func (b *SSHBackend) JobResults(ctx context.Context, jobID string) (*JobResults, error) {
	if err := b.backend.EnsureFresh(ctx, jobID, DefaultReadTTL); err != nil {
		return nil, err
	}
	return jobResultsFromDB(ctx, b.store, jobID)
}

func (b *SSHBackend) RetryTask(ctx context.Context, taskID string) error {
	b.touchActivity()
	b.admissionsInFlight.Add(1)
	defer b.admissionsInFlight.Add(-1)
	if b.scope.IsRetiring() {
		return fmt.Errorf("target %s: %w — retry to reach the active configuration", b.targetName, ErrLaneRetired)
	}
	// Lock order is lane lifecycle, then the Store's shared task-attempt lock.
	// The lane lock excludes this generation's verdict producers; the shared
	// task lock also excludes cross-generation ingestion/reset effects.
	return b.backend.WithLifecycleLock(func() error {
		return b.store.WithTaskAttemptLock(taskID, func() error {
			// Validate only after taking the same lock as every verdict producer.
			// A persisted status_source=retry row is an interrupted earlier call:
			// resume its idempotent wrapper reset without consuming another epoch.
			row, err := b.store.GetTask(ctx, taskID)
			if err != nil {
				return err
			}
			if row == nil {
				return fmt.Errorf("task %q not found", taskID)
			}
			switch {
			case row.Status == "failed" || row.Status == "killed":
				// Persist retry intent and unfreeze task-lifetime SDK stream
				// offsets in one transaction BEFORE touching wrapper evidence.
				if err := b.store.BeginTaskRetry(ctx, taskID, row.RetryCount, b.scope.Generation); err != nil {
					return fmt.Errorf("begin retry: %w", err)
				}
				row, err = b.store.GetTask(ctx, taskID)
				if err != nil {
					return fmt.Errorf("read retry intent: %w", err)
				}
				if row == nil {
					return fmt.Errorf("task %q disappeared after retry intent", taskID)
				}
			case row.Status == "submitting" && row.StatusSource == "retry":
				// Resume the already-counted epoch.
			default:
				return fmt.Errorf("task %q is %s, only failed/killed tasks can be retried", taskID, row.Status)
			}

			// Wrapper reset is the external effect guarded by the durable retry
			// intent. Until it succeeds the task is not publishable as pending.
			if err := b.backend.ResetWrapperState(ctx, row.TaskDir, taskID); err != nil {
				noteErr := b.store.UpdateTaskStatus(ctx, taskID, "submitting", map[string]any{
					"status_source":  "retry",
					"failure_detail": "retry wrapper reset failed: " + err.Error(),
				})
				return errors.Join(fmt.Errorf("reset wrapper state: %w", err), noteErr)
			}
			if err := b.store.UpdateTaskStatus(ctx, taskID, "pending", map[string]any{
				"status_source":  nil,
				"failure_detail": nil,
				"kill_requested": 0,
			}); err != nil {
				// Durable retry intent remains. Recovery or a repeated user retry
				// safely repeats the wrapper reset and pending publication.
				return fmt.Errorf("publish retry as pending: %w", err)
			}

			row, err = b.store.GetTask(ctx, taskID)
			if err != nil {
				return fmt.Errorf("read retried task: %w", err)
			}
			if row == nil {
				return fmt.Errorf("task %q disappeared after retry", taskID)
			}
			task := TaskRowToSchedulerTask(row)
			// Fresh attempt by explicit user intent — a stale kill flag from an
			// earlier refused kill must not assassinate it (RQ-69).
			b.sched.ClearKillRequest(taskID)
			if !b.queue.RetryExisting(task) {
				b.queue.Push(task)
			}
			b.sched.RefreshJobStatus(task.JobID)
			return nil
		})
	})
}

func (b *SSHBackend) KillJob(ctx context.Context, jobID string) error {
	b.touchActivity()
	// Kill supersedes the pause overlay. Clear the dispatch gate immediately,
	// then always recompute the durable job projection after this cancellation
	// pass, including partial-error paths that retain kill intent for replay.
	b.sched.ClearPause(jobID)
	defer b.sched.RefreshJobStatus(jobID)
	var killErrs []error
	// Settle locally-queued tasks first: they have no cluster job to cancel,
	// and marking them killed here means backend.Kill (below) skips them as
	// terminal instead of refusing over a missing external id. Queue-running
	// tasks (submit in flight) get the kill flag instead (RQ-69): if the
	// remote cancel below is refused for a missing external id, the flag
	// settles them at their next lifecycle event instead of resubmitting.
	for _, qt := range b.queue.ListByJob(jobID) {
		switch qt.Status {
		case scheduler.StatusPending:
			settled, err := b.sched.TryKillPending(qt.ID, "runq")
			if err != nil {
				killErrs = append(killErrs, fmt.Errorf("persist queued task %s kill: %w", qt.ID, err))
			} else if !settled {
				if err := b.sched.RequestKill(qt.ID); err != nil {
					killErrs = append(killErrs, err)
				}
			}
		case scheduler.StatusRunning, scheduler.StatusUnknown:
			if err := b.sched.RequestKill(qt.ID); err != nil {
				killErrs = append(killErrs, err)
			}
		}
	}
	if err := b.backend.EnsureFresh(ctx, jobID, 0); err != nil {
		killErrs = append(killErrs, fmt.Errorf("reconcile before kill: %w", err))
		return errors.Join(killErrs...)
	}
	// Match KillTask's explicit escape hatch for outcome-unknown attempts
	// whose submit handle was lost. A whole-job kill carries the same user
	// intent and must not leave precisely those tasks behind forever.
	rows, err := b.store.ListTasks(ctx, store.TaskFilter{JobID: jobID, Scope: b.scope})
	if err != nil {
		return fmt.Errorf("list tasks after reconcile: %w", err)
	}
	for i := range rows {
		task := &rows[i]
		if task.Status == "pending" {
			settled, err := b.sched.TryKillPending(task.ID, "runq")
			if err != nil {
				killErrs = append(killErrs, err)
			} else if !settled {
				if err := b.sched.RequestKill(task.ID); err != nil {
					killErrs = append(killErrs, err)
				}
			}
			continue
		}
		if task.Status == "submitting" && task.StatusSource == "retry" {
			settled, err := b.cancelRetryIntent(ctx, task.ID)
			if err != nil {
				killErrs = append(killErrs, err)
			} else if !settled {
				if pending, err := b.sched.TryKillPending(task.ID, "runq"); err != nil {
					killErrs = append(killErrs, err)
				} else if !pending {
					if err := b.sched.RequestKill(task.ID); err != nil {
						killErrs = append(killErrs, err)
					}
				}
			}
			continue
		}
		if task.Status != "unknown" || task.ExternalID != "" {
			continue
		}
		if qt := b.queue.Get(task.ID); qt != nil && qt.Status == scheduler.StatusUnknown {
			if err := b.sched.FinishTaskChecked(qt, scheduler.StatusKilled, map[string]any{"status_source": "runq"}); err != nil {
				killErrs = append(killErrs, fmt.Errorf("persist unknown task %s kill: %w", task.ID, err))
			}
		}
	}
	_, remoteErr := b.backend.Kill(ctx, jobID)
	if remoteErr != nil {
		killErrs = append(killErrs, remoteErr)
	}
	return errors.Join(killErrs...)
}

func (b *SSHBackend) PauseJob(ctx context.Context, jobID string) error {
	job, err := b.store.GetJob(ctx, jobID)
	if err != nil {
		return err
	}
	if job == nil {
		return fmt.Errorf("job %q: %w", jobID, ErrNotFound)
	}
	if store.IsTerminalJobStatus(job.Status) {
		return fmt.Errorf("job %q is already %s", jobID, job.Status)
	}
	// Establish the linearizable dispatch gate first. Any admitted dispatch
	// finishes its pre-launch aggregate write before this returns; later
	// dispatches observe the pause. Persist only after that boundary so an
	// admitted RefreshJobStatus cannot overwrite the durable pause.
	b.sched.PauseJob(jobID)
	if err := b.store.UpdateJobStatus(ctx, jobID, "paused"); err != nil {
		b.sched.ResumeJob(jobID) // the API did not acknowledge the pause
		return fmt.Errorf("persist job pause: %w", err)
	}
	return nil
}

func (b *SSHBackend) ResumeJob(ctx context.Context, jobID string) error {
	job, err := b.store.GetJob(ctx, jobID)
	if err != nil {
		return err
	}
	if job == nil {
		return fmt.Errorf("job %q: %w", jobID, ErrNotFound)
	}
	if job.Status != "paused" && !b.sched.IsJobPaused(jobID) {
		return fmt.Errorf("job %q is %s, not paused", jobID, job.Status)
	}
	if job.Status != "paused" {
		// Another owning generation already persisted the resume. This lane
		// still has the local dispatch gate, so complete the idempotent fanout.
		b.sched.ResumeJob(jobID)
		return nil
	}
	tasks, err := b.store.ListTasks(ctx, store.TaskFilter{JobID: jobID})
	if err != nil {
		return fmt.Errorf("derive resumed job status: %w", err)
	}
	status, err := store.ProjectJobStatus(tasks)
	if err != nil {
		return fmt.Errorf("derive resumed job status: %w", err)
	}
	if err := b.store.UpdateJobStatus(ctx, jobID, status); err != nil {
		return fmt.Errorf("persist job resume: %w", err)
	}
	b.sched.ResumeJob(jobID)
	return nil
}

// ── Submit ────────────────────────────────────────────────────────────────

// SubmitJob prepares the job (plan, workspace files, run.sh, submit.cmd,
// DB rows) and hands the tasks to this target's scheduler lane. The actual
// sbatch happens asynchronously per task in remote.Launcher, throttled by
// max_inflight slots. The returned count is tasks ACCEPTED (queued).
func (b *SSHBackend) SubmitJob(ctx context.Context, cfg job.JobConfig, opts SubmitOptions) (string, int, error) {
	b.touchActivity()
	// Increment BEFORE the gate (round 8 #1): once past this line the
	// sweep sees the submit and will not close the lane under it.
	b.admissionsInFlight.Add(1)
	defer b.admissionsInFlight.Add(-1)
	// Retired = no NEW tasks (round 7, user design: the retired flag is a
	// capability switch). A request that passes this check before the flag
	// flips still lands in this lane's queue, runs correctly under its
	// config snapshot, and is covered by the in-flight counter above.
	if b.scope.IsRetiring() {
		return "", 0, fmt.Errorf("target %s: %w — retry to reach the active configuration", b.targetName, ErrLaneRetired)
	}
	proj, err := b.reg.Get(ctx, cfg.Project)
	if err != nil {
		return "", 0, fmt.Errorf("project %q: %w", cfg.Project, err)
	}
	jobID, rows, err := b.backend.Prepare(ctx, cfg, proj, remote.SubmitOpts{SkipPreflight: opts.SkipPreflight})
	if err != nil {
		return jobID, 0, err
	}
	tasks := make([]*scheduler.Task, len(rows))
	for i := range rows {
		t := TaskRowToSchedulerTask(&rows[i])
		t.GPUsNeeded = 1 // one submission slot; cluster GPUs live in the template
		tasks[i] = t
	}
	b.queue.PushBatch(tasks)
	return jobID, len(rows), nil
}

func (b *SSHBackend) PreviewSubmit(ctx context.Context, cfg job.JobConfig, skipPreflight bool) (PreviewResult, error) {
	proj, err := b.reg.Get(ctx, cfg.Project)
	if err != nil {
		return PreviewResult{}, fmt.Errorf("project %q: %w", cfg.Project, err)
	}
	text, report, err := b.backend.Preview(ctx, cfg, proj, skipPreflight)
	if err != nil {
		return PreviewResult{}, err
	}
	return PreviewResult{Preview: text, Preflight: report}, nil
}

func (b *SSHBackend) ResolveNote(ctx context.Context, cfg job.JobConfig) (string, error) {
	rows, err := b.store.ListJobs(ctx, cfg.Project, "")
	if err != nil {
		return "", err
	}
	notes := make([]string, 0, len(rows))
	for _, r := range rows {
		notes = append(notes, r.Note)
	}
	return job.RenderNote(&cfg, job.NoteContext{
		Project: cfg.Project, Now: time.Now(), ExistingNotes: notes,
	})
}

// ── Archive ───────────────────────────────────────────────────────────────

func (b *SSHBackend) ListArchivedJobs(ctx context.Context) ([]JobSummary, error) {
	jobs, err := b.store.ListJobsArchived(ctx, "", b.backend.Cfg.Name)
	if err != nil {
		return nil, err
	}
	jobIDs := make([]string, len(jobs))
	for i, j := range jobs {
		jobIDs[i] = j.ID
	}
	allTasks, err := b.store.ListTasksForJobs(ctx, jobIDs)
	if err != nil {
		return nil, err
	}
	byJob := make(map[string][]store.TaskRow, len(jobs))
	for _, t := range allTasks {
		byJob[t.JobID] = append(byJob[t.JobID], t)
	}
	out := make([]JobSummary, 0, len(jobs))
	for _, j := range jobs {
		out = append(out, BuildJobSummary(j, byJob[j.ID]))
	}
	return out, nil
}

func (b *SSHBackend) ArchiveJob(ctx context.Context, jobID string) error {
	if err := b.backend.EnsureFresh(ctx, jobID, 0); err != nil {
		return fmt.Errorf("reconcile before archive: %w", err)
	}
	return b.store.ArchiveJob(ctx, jobID)
}

func (b *SSHBackend) UnarchiveJob(ctx context.Context, jobID string) error {
	return b.store.UnarchiveJob(ctx, jobID)
}

// ── Project operations ────────────────────────────────────────────────────

func (b *SSHBackend) ListProjects(ctx context.Context) ([]ProjectSummary, error) {
	configs, err := b.reg.List(ctx)
	if err != nil {
		return nil, err
	}
	return b.configsToSummaries(ctx, configs)
}

// compile-time interface check
var _ Backend = (*SSHBackend)(nil)
