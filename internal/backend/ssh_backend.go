package backend

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"net"
	"os"
	"sync"
	"sync/atomic"
	"time"

	"github.com/gliese129/runq/internal/logfile"
	"golang.org/x/crypto/ssh"
	"golang.org/x/crypto/ssh/agent"

	"github.com/gliese129/runq/internal/config"
	"github.com/gliese129/runq/internal/ingest"
	"github.com/gliese129/runq/internal/job"
	"github.com/gliese129/runq/internal/project"
	"github.com/gliese129/runq/internal/remote"
	"github.com/gliese129/runq/internal/resource"
	"github.com/gliese129/runq/internal/rfs"
	"github.com/gliese129/runq/internal/scheduler"
	"github.com/gliese129/runq/internal/store"
	"github.com/gliese129/runq/internal/workspace"
)

// Sensor cadences for the remote lane. Both loops are gated on HasInFlight —
// zero in-flight tasks means zero SSH traffic (the silence contract).
//
//   - markerScanInterval: one SFTP readdir of the done dir. Cheap, so it can
//     run often; this is the PRIMARY completion path.
//   - probeAlignInterval: a full EnsureAllFresh pass (batch qstat). Expensive
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
	storeQueries // embeds store, reg, and shared project/clean/thaw/dryrun methods
	backend      *remote.Backend
	sshFS        *rfs.SSHFS // held for Close()
	targetName   string     // this lane's target name (tasks.target scope)

	// Per-target scheduler lane (RQ-46): queue + submission-slot pool +
	// scheduler instance + remote launcher. Same lifecycle code as the local
	// lane, assembled from unsupervised components.
	queue    *scheduler.Queue
	pool     *resource.SlotAllocator
	sched    *scheduler.Scheduler
	launcher *remote.Launcher
	logger   *slog.Logger

	loopCancel context.CancelFunc
	loopWG     sync.WaitGroup

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
}

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
	var sshFS *rfs.SSHFS
	if cfg.FS != nil {
		// Injected transport (localhost-runqd lane over LocalFS).
		laneFS = cfg.FS
	} else {
		if t.SSH == nil {
			return nil, fmt.Errorf("target %q: ssh section is required for scheduler targets", t.Name)
		}
		// Build SSH config from target.
		host := t.SSH.Host
		if t.SSH.Port > 0 {
			host = fmt.Sprintf("%s:%d", t.SSH.Host, t.SSH.Port)
		}

		auth, err := resolveSSHAuth(t.SSH)
		if err != nil {
			return nil, fmt.Errorf("target %q: ssh auth: %w", t.Name, err)
		}

		sshCfg := rfs.SSHConfig{
			Host:       host,
			User:       t.SSH.User,
			AuthMethod: auth,
			// Idle disconnect: with the sensor loops running every ~2min
			// while tasks are in flight, the connection stays warm during
			// activity and closes ~10min after the queue drains — a normal
			// SSH user's shape.
			IdleTimeout: 10 * time.Minute,
		}
		sshFS = rfs.NewSSHFS(sshCfg)
		laneFS = sshFS
	}
	hpcBe := remote.NewWithFS(&cfg.Target, cfg.Store, cfg.GlobalCfg, laneFS)

	logger := cfg.Logger
	if logger == nil {
		logger = slog.Default()
	}
	logger = logger.With("target", t.Name)

	// Assemble the lane: FIFO prioritizer, submission slots, unsupervised
	// launcher, no freeze (disk-freeze is an SDK+local-disk feature), no GPU
	// refresh loop (no local GPUs to scan).
	q := scheduler.NewQueue()
	pool := resource.NewSlotAllocator(t.MaxInflight)
	launcher := remote.NewLauncher(hpcBe)
	schedCfg := scheduler.DefaultConfig()
	schedCfg.GPURefreshInterval = 0
	sched := scheduler.New(schedCfg, q, pool, launcher, cfg.Store, logger, nil, "", nil)

	// Terminal transitions found by reconcile/markers flow through the
	// scheduler's FinishTask funnel (retry policy, slot release).
	hpcBe.Finisher = sched

	be := &SSHBackend{
		storeQueries: storeQueries{
			store: cfg.Store,
			reg:   project.NewRegistry(cfg.Store.DB()),
		},
		backend:    hpcBe,
		sshFS:      sshFS,
		queue:      q,
		pool:       pool,
		sched:      sched,
		launcher:   launcher,
		logger:     logger,
		targetName: t.Name,
	}
	// Boot counts as activity: after a daemon restart the sensor gets one
	// active window to re-align restored in-flight state before it may
	// hibernate (zero value would mean "hibernated since 1970").
	be.touchActivity()
	return be, nil
}

// Start restores this target's tasks into the lane, starts the scheduler
// loop, and launches the two sensor loops. Called once by the daemon.
func (b *SSHBackend) Start(ctx context.Context) {
	b.restoreLane(ctx)
	b.sched.Start()
	loopCtx, cancel := context.WithCancel(ctx)
	b.loopCancel = cancel
	b.loopWG.Add(1)
	go b.sensorLoop(loopCtx)
}

// restoreLane rebuilds the in-memory queue/slots from the DB on startup:
// tasks with an external id are in flight on the cluster (occupy a slot,
// restored as running); tasks without one are waiting to be (re)launched.
func (b *SSHBackend) restoreLane(ctx context.Context) {
	rows, err := b.store.ListTasks(ctx, store.TaskFilter{Target: b.backend.Cfg.Name})
	if err != nil {
		b.logger.Warn("restore: list tasks failed", "error", err)
		return
	}
	restored, queued := 0, 0
	for i := range rows {
		row := rows[i]
		switch row.Status {
		case "success", "failed", "killed":
			continue
		}
		t := TaskRowToSchedulerTask(&row)
		t.GPUsNeeded = 1 // one submission slot per remote task
		if row.ExternalID != "" {
			_ = b.pool.Reserve(nil, row.ID) // never fails for slots
			t.Status = scheduler.StatusRunning
			b.queue.Restore(t)
			restored++
		} else {
			b.queue.Push(t)
			queued++
		}
	}
	if restored+queued > 0 {
		b.logger.Info("remote lane restored", "in_flight", restored, "queued", queued)
	}
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
		case <-ticker.C:
			tick++
			if inflight, err := b.backend.HasInFlight(ctx); err != nil || !inflight {
				continue
			}
			active := b.userActive()

			// Marker scan: every tick while active, every Nth while
			// hibernated — cheap enough to keep an overnight sweep moving.
			if !active && tick%hibernatedMarkerEvery != 0 {
				continue
			}
			if err := b.backend.ScanDoneMarkers(ctx); err != nil {
				b.logger.Warn("marker scan failed", "error", err)
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
			}

			// qstat alignment: user-present periods only.
			if !active || time.Since(lastAlign) < probeAlignInterval {
				continue
			}
			lastAlign = time.Now()
			if err := b.backend.EnsureAllFresh(ctx, DefaultReadTTL); err != nil {
				b.logger.Warn("probe align failed", "error", err)
			}
			if err := b.backend.DetectOrphans(ctx, false); err != nil {
				b.logger.Warn("orphan detection failed", "error", err)
			}
		}
	}
}

// ingestRunningMetrics runs one incremental metrics pass over this lane's
// running tasks. Each task costs one stat when idle, delta-only reads when
// growing — the spread-out替代 of a read-time parse storm.
func (b *SSHBackend) ingestRunningMetrics(ctx context.Context) {
	rows, err := b.store.ListTasks(ctx, store.TaskFilter{Status: "running", Target: b.targetName})
	if err != nil {
		return
	}
	for _, r := range rows {
		if r.TaskDir == "" {
			continue
		}
		if _, err := ingest.ReapIncremental(ctx, b.store, ingest.Target{
			TaskID: r.ID, JobID: r.JobID, Dir: r.TaskDir, FS: b.backend.FS,
		}, false); err != nil {
			b.logger.Debug("metrics ingest failed", "task", r.ID, "error", err)
		}
	}
}

// DetectOrphansNow runs an immediate (no-hysteresis) orphan scan. Called by
// the interactive clean path, where the user visually confirms every entry.
func (b *SSHBackend) DetectOrphansNow(ctx context.Context) error {
	return b.backend.DetectOrphans(ctx, true)
}

// Close stops the sensor loops and the scheduler, then releases the SSH
// connection (if any — the localhost lane has none). Must be called on
// daemon shutdown.
func (b *SSHBackend) Close() error {
	if b.loopCancel != nil {
		b.loopCancel()
		b.loopWG.Wait()
		b.sched.Shutdown()
	}
	if b.sshFS != nil {
		return b.sshFS.Close()
	}
	return nil
}

// ── Capabilities ──────────────────────────────────────────────────────────

func (b *SSHBackend) Capabilities() Capabilities {
	return Capabilities{
		GPUMap:        b.backend.Cfg.GPUTemplate != "", // gpu_template-driven (runq preset)
		PauseResume:   false,                           // cluster queues have no runq-level pause
		LiveLog:       true,                            // logs readable via SSH
		Retry:         true,                            // scheduler lane re-runs submit.cmd (RQ-46)
		StateModel:    "poll",                          // best-effort projection; staleness surfaced
		KillAsync:     true,                            // qdel/scancel forwarded
		SubmitPreview: true,                            // zero-disk dry-run via submit code path
	}
}

// ── Reconcile ─────────────────────────────────────────────────────────────

func (b *SSHBackend) RefreshJob(ctx context.Context, jobID string) error {
	b.touchActivity()
	return b.backend.EnsureFresh(ctx, jobID, 0)
}

// ReconcileAll runs a full reconcile pass over all active jobs. Called by
// the dashboard's background ticker — never by the list endpoint itself.
func (b *SSHBackend) ReconcileAll(ctx context.Context) error {
	return b.backend.EnsureAllFresh(ctx, DefaultReadTTL)
}

// ── Job operations ────────────────────────────────────────────────────────

func (b *SSHBackend) ListJobs(ctx context.Context, projectScope string) ([]JobSummary, error) {
	b.touchActivity()
	target := b.backend.Cfg.Name
	jobs, err := b.store.ListJobsVisible(ctx, projectScope, target)
	if err != nil {
		return nil, err
	}
	for _, j := range jobs {
		if j.Status != "done" {
			_ = b.backend.EnsureFresh(ctx, j.ID, DefaultReadTTL)
		}
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

func (b *SSHBackend) GetTask(ctx context.Context, taskID string) (*TaskView, error) {
	b.touchActivity()
	task, err := b.store.GetTask(ctx, taskID)
	if err != nil {
		return nil, err
	}
	if task == nil {
		return nil, fmt.Errorf("task %q: %w", taskID, ErrNotFound)
	}
	task = b.reconcileTask(ctx, taskID, task)
	view := BuildTaskView(*task)
	return &view, nil
}

// FS exposes this lane's filesystem (LocalFS or SSHFS — the lane doesn't
// know the difference, and neither should callers).
func (b *SSHBackend) FS() rfs.FS { return b.backend.FS }

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

// ListTasks — the lane defaults the target scope to itself (its slice of
// the shared store); an explicit opts.Target overrides.
func (b *SSHBackend) ListTasks(ctx context.Context, opts TaskListOptions) ([]TaskView, int, error) {
	b.touchActivity()
	if opts.Target == "" {
		opts.Target = b.targetName
	}
	return listTasksFromStore(ctx, b.store, opts)
}

// TaskMetrics serves the chart: the RAW TAIL WINDOW of metrics.jsonl (one
// ranged read — recent data is small by construction). The incremental
// catch-up keeps the summary warm as a side effect (one stat when idle;
// TODO(L4): gate behind the 60s light TTL). Full-history multi-resolution
// zoom will come from the on-target pyramid (TODO: wire pyramid.Query per
// key once the builder lands). afterTS > 0 = ?after= incremental pull.
func (b *SSHBackend) TaskMetrics(ctx context.Context, taskID string, afterTS int64) ([]MetricPoint, error) {
	b.touchActivity()
	task, err := b.store.GetTask(ctx, taskID)
	if err != nil || task == nil {
		return nil, fmt.Errorf("task %q: %w", taskID, ErrNotFound)
	}
	_, _ = ingest.ReapIncremental(ctx, b.store, ingest.Target{
		TaskID: task.ID, JobID: task.JobID, Dir: task.TaskDir, FS: b.backend.FS,
	}, false)
	return readTailMetricPoints(b.backend.FS, task.TaskDir, "", 2000, afterTS), nil
}

// TaskMetricBuckets — bucket-mode chart (spec §6.4): terminal tasks read
// the on-target pyramid (1–O(log n) rfs ranged reads); pyramid absent
// (running / builder not configured) falls back to tail-window
// aggregation with the SAME merge operator.
func (b *SSHBackend) TaskMetricBuckets(ctx context.Context, taskID, key string, fromTS, toTS int64, maxBuckets int) ([]workspace.PyramidBucket, string, error) {
	b.touchActivity()
	task, err := b.store.GetTask(ctx, taskID)
	if err != nil || task == nil {
		return nil, "", fmt.Errorf("task %q: %w", taskID, ErrNotFound)
	}
	buckets, err := workspace.QueryPyramid(ctx, b.backend.FS, task.TaskDir, key, fromTS, toTS, maxBuckets)
	switch {
	case err == nil:
		return buckets, "pyramid", nil
	case errors.Is(err, workspace.ErrPyramidNotBuilt):
		return tailMetricBuckets(b.backend.FS, task.TaskDir, key, fromTS, toTS, maxBuckets), "tail", nil
	default:
		return nil, "", err // e.g. key not indexed — a real answer, not a fallback case
	}
}

func (b *SSHBackend) reconcileTask(ctx context.Context, taskID string, fallback *store.TaskRow) *store.TaskRow {
	if err := b.backend.EnsureFresh(ctx, fallback.JobID, DefaultReadTTL); err != nil {
		return fallback
	}
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
	// Queued locally (not yet handed to the cluster): nothing to cancel
	// remotely — settle through the lifecycle funnel. In-flight-but-untracked
	// tasks are queue-running, so they fall through to backend.Kill, which
	// refuses honestly (no external id → cannot cancel).
	if qt := b.queue.Get(taskID); qt != nil && qt.Status == scheduler.StatusPending {
		b.sched.FinishTask(qt, scheduler.StatusKilled, map[string]any{"status_source": "runq"})
		return nil
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
	_, err = b.backend.Kill(ctx, taskID)
	return err
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
	task, err := b.store.GetTask(ctx, taskID)
	if err != nil {
		return nil, fmt.Errorf("task log read: %w", err)
	}
	if task == nil {
		return nil, fmt.Errorf("task %q: %w", taskID, ErrNotFound)
	}
	r, err := logfile.Open(task.LogPath, b.backend.FS)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return &LogPage{Lines: []string{}}, nil // pending: empty page, Size 0
		}
		return nil, err
	}
	defer r.Close()

	return r.ReadLines(offset, maxLines) // LogPage = logfile.Page: no mapping
}

// TaskLogTail — 尾部视图（每次首屏的入口）：解析 + 委托 TailLines。
// 返回页的 Offset 即向上翻页锚点。
func (b *SSHBackend) TaskLogTail(ctx context.Context, taskID string, maxLines int) (*LogPage, error) {
	b.touchActivity()
	task, err := b.store.GetTask(ctx, taskID)
	if err != nil {
		return nil, fmt.Errorf("task log tail: %w", err)
	}
	if task == nil {
		return nil, fmt.Errorf("task %q: %w", taskID, ErrNotFound)
	}
	r, err := logfile.Open(task.LogPath, b.backend.FS)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return &LogPage{Lines: []string{}}, nil // pending: empty page, Size 0
		}
		return nil, err
	}
	defer r.Close()

	return r.TailLines(maxLines) // LogPage = logfile.Page: no mapping
}

// TaskLogFollow — pure assembly: resolve the task, hand path+FS+offset to
// logfile.Follow. *logfile.Follower satisfies LogFollower natively (LogPage
// = logfile.Page), so there is no adapter and deliberately NO goroutine:
// LogFollower is a PULL iterator, driven by the consumer's loop (SSE
// handler, CLI logs -f). offset passes through unclamped: size < offset IS
// the rotation signal. Pending log (no file yet) is handled inside
// Follower (lazy open). No lifecycleMu (read-only, produces no verdicts).
func (b *SSHBackend) TaskLogFollow(ctx context.Context, taskID string, offset int64) (LogFollower, error) {
	b.touchActivity()
	task, err := b.store.GetTask(ctx, taskID)
	if err != nil {
		return nil, fmt.Errorf("task log follow: %w", err)
	}
	if task == nil {
		return nil, fmt.Errorf("task %q: %w", taskID, ErrNotFound)
	}
	f, err := logfile.Follow(task.LogPath, b.backend.FS, offset)
	if err != nil {
		return nil, fmt.Errorf("task log follow: %w", err)
	}
	return f, nil
}

// JobLogSearch greps the job's logs on the owning side (RQ-44): one
// batched grep over FS.Exec — results travel, files don't.
func (b *SSHBackend) JobLogSearch(ctx context.Context, jobID, query string) ([]LogMatch, error) {
	b.touchActivity()
	return jobLogSearchViaExec(ctx, b.store, b.backend.FS, jobID, query)
}

func (b *SSHBackend) RetryTask(ctx context.Context, taskID string) error {
	b.touchActivity()
	row, err := b.store.GetTask(ctx, taskID)
	if err != nil {
		return err
	}
	if row == nil {
		return fmt.Errorf("task %q not found", taskID)
	}
	if row.Status != "failed" && row.Status != "killed" {
		return fmt.Errorf("task %q is %s, only failed/killed tasks can be retried", taskID, row.Status)
	}

	// The whole reset-and-requeue runs under the target's lifecycle lock:
	// a verdict delivery in flight (marker/probe/kill) must fully land or
	// fully wait — otherwise a stale verdict could interleave with the reset
	// and clobber the fresh attempt (the serialization IS the staleness
	// defense; see remote.Backend.lifecycleMu).
	return b.backend.WithLifecycleLock(func() error {
		// Reset the wrapper state files FIRST (synchronously, over SSH). A
		// stale terminal status.json would be re-reconciled into an instant
		// failure before the relaunch happens; the awaitingRelaunch guard
		// doesn't cover user retries of first-attempt failures (retry_count
		// may be 0).
		if err := b.backend.ResetWrapperState(ctx, row.TaskDir, taskID); err != nil {
			return fmt.Errorf("reset wrapper state: %w", err)
		}

		// Fresh attempt = fresh metrics: drop ingested rows AND the ingest
		// mark — a hard-final terminal froze it (Final=true), and without
		// this reset the new attempt's metrics.jsonl would never be read.
		if err := b.store.DeleteTaskMetrics(ctx, taskID); err != nil {
			return fmt.Errorf("reset metrics ingest: %w", err)
		}

		if err := b.store.UpdateTaskStatus(ctx, taskID, "pending", map[string]any{
			"gpus":        nil,
			"pid":         nil,
			"started_at":  nil,
			"finished_at": nil,
			"external_id": nil,
		}); err != nil {
			return err
		}

		row, _ = b.store.GetTask(ctx, taskID)
		task := TaskRowToSchedulerTask(row)
		task.GPUsNeeded = 1
		if !b.queue.RetryExisting(task) {
			b.queue.Push(task)
		}
		b.sched.RefreshJobStatus(task.JobID)
		return nil
	})
}

func (b *SSHBackend) KillJob(ctx context.Context, jobID string) error {
	b.touchActivity()
	// Settle locally-queued tasks first: they have no cluster job to cancel,
	// and marking them killed here means backend.Kill (below) skips them as
	// terminal instead of refusing over a missing external id.
	for _, qt := range b.queue.ListByJob(jobID) {
		if qt.Status == scheduler.StatusPending {
			b.sched.FinishTask(qt, scheduler.StatusKilled, map[string]any{"status_source": "runq"})
		}
	}
	if err := b.backend.EnsureFresh(ctx, jobID, 0); err != nil {
		return fmt.Errorf("reconcile before kill: %w", err)
	}
	_, err := b.backend.Kill(ctx, jobID)
	return err
}

func (b *SSHBackend) PauseJob(_ context.Context, _ string) error {
	return fmt.Errorf("pause job in ssh mode: %w", ErrNotSupported)
}

func (b *SSHBackend) ResumeJob(_ context.Context, _ string) error {
	return fmt.Errorf("resume job in ssh mode: %w", ErrNotSupported)
}

// ── Submit ────────────────────────────────────────────────────────────────

// SubmitJob prepares the job (plan, workspace files, run.sh, submit.cmd,
// DB rows) and hands the tasks to this target's scheduler lane. The actual
// sbatch happens asynchronously per task in remote.Launcher, throttled by
// max_inflight slots. The returned count is tasks ACCEPTED (queued).
func (b *SSHBackend) SubmitJob(ctx context.Context, cfg job.JobConfig, opts SubmitOptions) (string, int, error) {
	b.touchActivity()
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

func (b *SSHBackend) PreviewSubmit(ctx context.Context, cfg job.JobConfig, skipPreflight bool) (string, error) {
	proj, err := b.reg.Get(ctx, cfg.Project)
	if err != nil {
		return "", fmt.Errorf("project %q: %w", cfg.Project, err)
	}
	return b.backend.Preview(ctx, cfg, proj, skipPreflight)
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

// ── SSH auth helper ───────────────────────────────────────────────────────

// resolveSSHAuth builds an ssh.AuthMethod from the target's SSH config.
// If Key is set, reads the private key file. Otherwise falls back to
// the SSH agent (SSH_AUTH_SOCK).
func resolveSSHAuth(cfg *config.SSHTargetConfig) (ssh.AuthMethod, error) {
	if cfg.Key != "" {
		keyBytes, err := os.ReadFile(cfg.Key)
		if err != nil {
			return nil, fmt.Errorf("read key %q: %w", cfg.Key, err)
		}
		signer, err := ssh.ParsePrivateKey(keyBytes)
		if err != nil {
			return nil, fmt.Errorf("parse key %q: %w", cfg.Key, err)
		}
		return ssh.PublicKeys(signer), nil
	}

	// Fall back to ssh-agent.
	sock := os.Getenv("SSH_AUTH_SOCK")
	if sock == "" {
		return nil, fmt.Errorf("no key file and SSH_AUTH_SOCK not set")
	}
	conn, err := net.Dial("unix", sock)
	if err != nil {
		return nil, fmt.Errorf("connect to ssh-agent: %w", err)
	}
	// Note: conn is intentionally not closed here — the agent client holds
	// it for the lifetime of the SSH connection. The OS reclaims it on exit.
	agentClient := agent.NewClient(conn)
	return ssh.PublicKeysCallback(agentClient.Signers), nil
}

// compile-time interface check
var _ Backend = (*SSHBackend)(nil)
