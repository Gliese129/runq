package backend

import (
	"context"
	"fmt"
	"log/slog"
	"net"
	"os"
	"sync"
	"time"

	"golang.org/x/crypto/ssh"
	"golang.org/x/crypto/ssh/agent"

	"github.com/gliese129/runq/internal/config"
	"github.com/gliese129/runq/internal/job"
	"github.com/gliese129/runq/internal/project"
	"github.com/gliese129/runq/internal/remote"
	"github.com/gliese129/runq/internal/resource"
	"github.com/gliese129/runq/internal/rfs"
	"github.com/gliese129/runq/internal/scheduler"
	"github.com/gliese129/runq/internal/store"
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
	probeAlignInterval = 25 * time.Minute
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
}

// SSHBackendConfig bundles everything needed to build an SSHBackend.
type SSHBackendConfig struct {
	Target    config.TargetConfig
	Store     *store.Store
	GlobalCfg *config.GlobalConfig
	Logger    *slog.Logger // nil → slog.Default()
}

// NewSSHBackend creates a Backend for a remote HPC target. The SSH
// connection is lazy — no dial happens until the first operation.
//
// The caller must call Close() on shutdown to release the SSH connection.
func NewSSHBackend(cfg SSHBackendConfig) (*SSHBackend, error) {
	t := cfg.Target
	if t.SSH == nil {
		return nil, fmt.Errorf("target %q: ssh section is required for scheduler targets", t.Name)
	}
	if t.SubmitTemplate == "" {
		return nil, fmt.Errorf("target %q: submit_template is required", t.Name)
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
		// Idle disconnect: with the sensor loops running every ~2min while
		// tasks are in flight, the connection stays warm during activity and
		// closes ~10min after the queue drains — a normal SSH user's shape.
		IdleTimeout: 10 * time.Minute,
	}

	sshFS := rfs.NewSSHFS(sshCfg)
	hpcBe := remote.NewWithFS(&cfg.Target, cfg.Store, cfg.GlobalCfg, sshFS)

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

	return &SSHBackend{
		storeQueries: storeQueries{
			store: cfg.Store,
			reg:   project.NewRegistry(cfg.Store.DB()),
		},
		backend:  hpcBe,
		sshFS:    sshFS,
		queue:    q,
		pool:     pool,
		sched:    sched,
		launcher: launcher,
		logger:   logger,
	}, nil
}

// Start restores this target's tasks into the lane, starts the scheduler
// loop, and launches the two sensor loops. Called once by the daemon.
func (b *SSHBackend) Start(ctx context.Context) {
	b.restoreLane(ctx)
	b.sched.Start()
	loopCtx, cancel := context.WithCancel(ctx)
	b.loopCancel = cancel
	b.loopWG.Add(2)
	go b.markerLoop(loopCtx)
	go b.probeAlignLoop(loopCtx)
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

// markerLoop is the primary completion sensor: one readdir per tick, only
// while something is in flight.
func (b *SSHBackend) markerLoop(ctx context.Context) {
	defer b.loopWG.Done()
	ticker := time.NewTicker(markerScanInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if inflight, err := b.backend.HasInFlight(ctx); err != nil || !inflight {
				continue
			}
			if err := b.backend.ScanDoneMarkers(ctx); err != nil {
				b.logger.Warn("marker scan failed", "error", err)
			}
		}
	}
}

// probeAlignLoop is the low-frequency safety net: a batch scheduler probe
// catches tasks whose wrapper never wrote a marker (node fail, hard kill)
// so their slots don't leak.
func (b *SSHBackend) probeAlignLoop(ctx context.Context) {
	defer b.loopWG.Done()
	ticker := time.NewTicker(probeAlignInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if inflight, err := b.backend.HasInFlight(ctx); err != nil || !inflight {
				continue
			}
			if err := b.backend.EnsureAllFresh(ctx, DefaultReadTTL); err != nil {
				b.logger.Warn("probe align failed", "error", err)
			}
		}
	}
}

// Close stops the sensor loops and the scheduler, then releases the SSH
// connection. Must be called on daemon shutdown.
func (b *SSHBackend) Close() error {
	if b.loopCancel != nil {
		b.loopCancel()
		b.loopWG.Wait()
		b.sched.Shutdown()
	}
	return b.sshFS.Close()
}

// ── Capabilities ──────────────────────────────────────────────────────────

func (b *SSHBackend) Capabilities() Capabilities {
	return Capabilities{
		GPUMap:        false,  // no node-local GPU visibility
		PauseResume:   false,  // cluster queues have no runq-level pause (Step 3 candidate)
		LiveLog:       true,   // logs readable via SSH
		Retry:         true,   // scheduler lane re-runs submit.cmd (RQ-46)
		StateModel:    "poll", // best-effort projection; staleness surfaced
		KillAsync:     true,   // qdel/scancel forwarded
		SubmitPreview: true,   // zero-disk dry-run via submit code path
	}
}

// ── Reconcile ─────────────────────────────────────────────────────────────

func (b *SSHBackend) RefreshJob(ctx context.Context, jobID string) error {
	return b.backend.EnsureFresh(ctx, jobID, 0)
}

// ReconcileAll runs a full reconcile pass over all active jobs. Called by
// the dashboard's background ticker — never by the list endpoint itself.
func (b *SSHBackend) ReconcileAll(ctx context.Context) error {
	return b.backend.EnsureAllFresh(ctx, DefaultReadTTL)
}

// ── Job operations ────────────────────────────────────────────────────────

func (b *SSHBackend) ListJobs(ctx context.Context, projectScope string) ([]JobSummary, error) {
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
	detail := BuildJobDetail(*j, tasks)
	if cfg, err := b.reg.Get(ctx, j.ProjectName); err == nil && cfg.Wandb != nil {
		detail.Wandb = &WandbInfo{
			Entity:  cfg.Wandb.Entity,
			Project: cfg.Wandb.Project,
			BaseURL: WandbBaseURL(cfg.Wandb.Entity, cfg.Wandb.Project),
		}
	}
	return &detail, nil
}

func (b *SSHBackend) CompareMetrics(ctx context.Context, jobID, key string, desc bool) ([]CompareRow, error) {
	if err := b.backend.EnsureFresh(ctx, jobID, DefaultReadTTL); err != nil {
		return nil, err
	}
	tasks, err := b.store.ListTasks(ctx, store.TaskFilter{JobID: jobID})
	if err != nil {
		return nil, err
	}
	return BuildCompareRows(tasks, key, desc), nil
}

func (b *SSHBackend) GPUStatus(_ context.Context) ([]GPUSlot, error) {
	return []GPUSlot{}, nil
}

// ── Task operations ───────────────────────────────────────────────────────

func (b *SSHBackend) GetTask(ctx context.Context, taskID string) (*TaskView, error) {
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

func (b *SSHBackend) TaskMetrics(ctx context.Context, taskID string) ([]MetricPoint, error) {
	task, err := b.store.GetTask(ctx, taskID)
	if err != nil || task == nil {
		return nil, fmt.Errorf("task %q: %w", taskID, ErrNotFound)
	}
	task = b.reconcileTask(ctx, taskID, task)
	return ReadMetricPoints(task.TaskDir), nil
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

func (b *SSHBackend) RetryTask(ctx context.Context, taskID string) error {
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

	// Reset the wrapper state files FIRST (synchronously, over SSH). A stale
	// terminal status.json would be re-reconciled into an instant failure
	// before the relaunch happens; the awaitingRelaunch guard doesn't cover
	// user retries of first-attempt failures (retry_count may be 0).
	if err := b.backend.ResetWrapperState(ctx, row.TaskDir, taskID); err != nil {
		return fmt.Errorf("reset wrapper state: %w", err)
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
}

func (b *SSHBackend) KillJob(ctx context.Context, jobID string) error {
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
