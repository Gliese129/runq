package backend

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"time"

	"github.com/gliese129/runq/internal/config"
	"github.com/gliese129/runq/internal/executor"
	"github.com/gliese129/runq/internal/ingest"
	"github.com/gliese129/runq/internal/job"
	"github.com/gliese129/runq/internal/logfile"
	"github.com/gliese129/runq/internal/project"
	"github.com/gliese129/runq/internal/resource"
	"github.com/gliese129/runq/internal/scheduler"
	"github.com/gliese129/runq/internal/store"
	"github.com/gliese129/runq/internal/submitplan"
	"github.com/gliese129/runq/internal/utils"
	"github.com/gliese129/runq/internal/workspace"
)

// LocalBackend implements Backend for the local GPU target. All daemon
// components (queue, scheduler, executor) are held directly — no
// intermediate service layer.
//
// All state is push-model: the daemon owns the truth via scheduler/executor,
// so data is always current — no reconcile needed.
type LocalBackend struct {
	storeQueries // embeds store, reg, and shared project/clean/thaw/dryrun methods
	queue        *scheduler.Queue
	scheduler    *scheduler.Scheduler
	exec         *executor.Executor
	pool         resource.Allocator
	storageCfg   *config.GlobalConfig // nil-safe: nil = project_path mode
	targetName   string               // routing key written to DB; defaults to "local"
	generation   string               // RQ-75: lane-generation stamp for new tasks ('' = adopt-by-active)
}

// LocalBackendDeps groups the daemon components that LocalBackend wraps.
type LocalBackendDeps struct {
	Store      *store.Store
	Reg        *project.Registry
	Queue      *scheduler.Queue
	Scheduler  *scheduler.Scheduler
	Exec       *executor.Executor
	Pool       resource.Allocator
	StorageCfg *config.GlobalConfig
	TargetName string // routing key written to DB; empty defaults to "local"
	Generation string // RQ-75: semantic generation of the target config ('' ok)
}

// NewLocalBackend creates a Backend that wraps the daemon's internal
// components. All calls are direct function invocations — no HTTP, no
// serialization overhead.
func NewLocalBackend(deps LocalBackendDeps) *LocalBackend {
	name := deps.TargetName
	if name == "" {
		name = "local" // matches store.targetOrDefault
	}
	return &LocalBackend{
		storeQueries: storeQueries{store: deps.Store, reg: deps.Reg},
		queue:        deps.Queue,
		scheduler:    deps.Scheduler,
		exec:         deps.Exec,
		pool:         deps.Pool,
		storageCfg:   deps.StorageCfg,
		targetName:   name,
		generation:   deps.Generation,
	}
}

// ── Task intake (runq preset, executor lane) ──────────────────────────────

// TaskSpec describes one pre-planned task handed to this server by a runq
// client — the ONLY intake path into this ledger, so there is no
// "foreign/internal" distinction to name. The client prepared run.sh, the
// task dir and the log path on THIS machine's filesystem (it IS the
// client's target workspace); the server contributes only what it owns —
// GPU allocation, scheduling, process supervision. run.sh is
// self-contained (env exports, status.json, done marker), so the client's
// sensors work on it unchanged.
type TaskSpec struct {
	RunSH   string `json:"run_sh"`
	GPUs    int    `json:"gpus"`
	Name    string `json:"name,omitempty"`
	TaskDir string `json:"task_dir"`
	LogPath string `json:"log_path,omitempty"`
	// Handle is the caller-chosen task id (RQ-69: the client's
	// "{task_id}-a{attempt}" cancel handle). When set it becomes THIS
	// server's task id, so the client can cancel/probe deterministically
	// without ever depending on the submit response. Re-submitting an
	// existing handle is an idempotent claim, not a duplicate.
	Handle string `json:"handle,omitempty"`
	// Project is the CLIENT's project name, carried through the protocol as
	// a plain label (like a Slurm job name): it makes squeue/logs greppable
	// but requires NO registration here — this ledger is a scheduler's,
	// not an experiment registry.
	Project string `json:"project,omitempty"`
}

// Enqueue inserts a single-task job and pushes it into this server's
// scheduler queue. Returns the task id — the caller (runq client) records
// it as the task's external_id, exactly as it would record a Slurm job id.
func (b *LocalBackend) Enqueue(ctx context.Context, spec TaskSpec) (string, error) {
	if spec.RunSH == "" || spec.TaskDir == "" {
		return "", fmt.Errorf("run_sh and task_dir are required")
	}
	label := spec.Project
	if label == "" {
		label = "external"
	}

	now := time.Now()
	jobID := utils.GenerateJobID()
	taskID := utils.GenerateTaskID()
	if spec.Handle != "" {
		if !validHandle(spec.Handle) {
			return "", fmt.Errorf("invalid handle %q: only [A-Za-z0-9._-] allowed", spec.Handle)
		}
		// Idempotent claim: the handle already existing means an earlier
		// submit succeeded but its response was lost — adopt it instead of
		// enqueueing a duplicate (clients resubmit with the same handle).
		if existing, gerr := b.store.GetTask(ctx, spec.Handle); gerr == nil && existing != nil {
			return spec.Handle, nil
		}
		taskID = spec.Handle
	}
	logPath := spec.LogPath
	if logPath == "" {
		logPath = filepath.Join(spec.TaskDir, taskID+".log")
	}

	jobRow := store.JobRow{
		ID: jobID, ProjectName: label, Note: spec.Name,
		Status: "pending", TotalTasks: 1, Target: b.targetName, CreatedAt: now,
	}
	if err := b.store.InsertJob(ctx, &jobRow); err != nil {
		return "", fmt.Errorf("persist foreign job: %w", err)
	}
	row := store.TaskRow{
		ID: taskID, JobID: jobID, ProjectName: label,
		Command:    "sh " + utils.ShellQuote(spec.RunSH),
		GPUsNeeded: spec.GPUs, Status: "pending",
		LogPath: logPath, WorkingDir: spec.TaskDir,
		TaskDir: spec.TaskDir, Target: b.targetName, TargetGeneration: b.generation,
		EnqueuedAt: now,
	}
	if err := b.store.InsertTask(ctx, &row); err != nil {
		return "", fmt.Errorf("persist foreign task: %w", err)
	}
	b.queue.Push(TaskRowToSchedulerTask(&row))
	return taskID, nil
}

// validHandle restricts caller-chosen task ids to filename/shell-safe
// characters — the id lands in file paths (log name) and command lines.
func validHandle(h string) bool {
	if len(h) == 0 || len(h) > 128 {
		return false
	}
	for _, c := range h {
		switch {
		case c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z', c >= '0' && c <= '9',
			c == '-', c == '_', c == '.':
		default:
			return false
		}
	}
	return true
}

// DetectOrphansNow marks/clears orphan state for this local target's
// terminal tasks. A local os.Stat is authoritative (unlike remote
// observation), so there is no hysteresis; the interactive clean
// confirmation remains the final safety layer before any deletion.
func (b *LocalBackend) DetectOrphansNow(ctx context.Context) error {
	rows, err := b.store.ListTasks(ctx, store.TaskFilter{Target: b.targetName})
	if err != nil {
		return err
	}
	now := time.Now()
	for _, r := range rows {
		if store.IsActiveStatus(r.Status) || r.TaskDir == "" {
			continue
		}
		_, serr := os.Stat(r.TaskDir)
		switch {
		case serr == nil:
			_ = b.store.ClearTaskOrphaned(ctx, r.ID)
		case os.IsNotExist(serr):
			if merr := b.store.MarkTaskOrphaned(ctx, r.ID, now); merr != nil && err == nil {
				err = merr
			}
		default:
			// Permission or I/O error: no fact learned; leave as is.
		}
	}
	return err
}

// ── RQ-44: log access（runqd 本机实现）───────────────────────────────────
//
// runqd 的公开面已退役（D1），人类不经这里读日志——但接口合规实现依然
// 给真的：logfile 走 nil FS（本机），与 SSHBackend 同一条读取核心，语义
// 不可能分叉。

func (b *LocalBackend) TaskLogRead(ctx context.Context, taskID string, offset int64, maxLines int) (*LogPage, error) {
	task, err := b.store.GetTask(ctx, taskID)
	if err != nil {
		return nil, err
	}
	if task == nil {
		return nil, fmt.Errorf("task %q: %w", taskID, ErrNotFound)
	}
	r, err := logfile.Open(task.LogPath, nil)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return &LogPage{Lines: []string{}}, nil // pending: empty page
		}
		return nil, err
	}
	defer r.Close()
	return r.ReadLines(offset, maxLines)
}

func (b *LocalBackend) TaskLogTail(ctx context.Context, taskID string, maxLines int) (*LogPage, error) {
	task, err := b.store.GetTask(ctx, taskID)
	if err != nil {
		return nil, err
	}
	if task == nil {
		return nil, fmt.Errorf("task %q: %w", taskID, ErrNotFound)
	}
	r, err := logfile.Open(task.LogPath, nil)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return &LogPage{Lines: []string{}}, nil
		}
		return nil, err
	}
	defer r.Close()
	return r.TailLines(maxLines)
}

func (b *LocalBackend) TaskLogPage(ctx context.Context, taskID string, req logfile.PageRequest) (*LogPage, error) {
	task, err := b.store.GetTask(ctx, taskID)
	if err != nil {
		return nil, err
	}
	if task == nil {
		return nil, fmt.Errorf("task %q: %w", taskID, ErrNotFound)
	}
	r, err := logfile.Open(task.LogPath, nil)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return &LogPage{Lines: []string{}, TotalLines: -1, StartLine: -1}, nil // pending
		}
		return nil, err
	}
	defer r.Close()
	return r.ReadPage(req)
}

func (b *LocalBackend) TaskLogFollow(ctx context.Context, taskID string, offset int64) (LogFollower, error) {
	task, err := b.store.GetTask(ctx, taskID)
	if err != nil {
		return nil, err
	}
	if task == nil {
		return nil, fmt.Errorf("task %q: %w", taskID, ErrNotFound)
	}
	return logfile.Follow(task.LogPath, nil, offset)
}

func (b *LocalBackend) JobLogSearch(ctx context.Context, jobID, query string) ([]LogMatch, error) {
	return jobLogSearchViaExec(ctx, b.store, nil, jobID, query)
}

func (b *LocalBackend) JobActivity(ctx context.Context, jobID string) (*JobActivity, error) {
	return jobActivityViaExec(ctx, b.store, nil, jobID)
}

// JobResults is pure SQL over ingested result_records (same posture as
// CompareMetrics: the lifecycle reap keeps the store current locally).
func (b *LocalBackend) JobResults(ctx context.Context, jobID string) (*JobResults, error) {
	return jobResultsFromDB(ctx, b.store, jobID)
}

// ── Capabilities ──────────────────────────────────────────────────────────

func (b *LocalBackend) Capabilities() Capabilities {
	return Capabilities{
		GPUMap:      true,
		PauseResume: true,
		LiveLog:     true,
		Retry:       true,
		StateModel:  "push",
		KillAsync:   false,
	}
}

// ── Job operations ────────────────────────────────────────────────────────

// RefreshJob: push model — data is always current.
// SyncInfo — the local lane's photo IS the live DB: always fresh (L4).
func (b *LocalBackend) SyncInfo(_ context.Context) (int64, bool) {
	return time.Now().Unix(), false
}

// ForceRefresh — nothing to sync; the receipt says so honestly.
func (b *LocalBackend) ForceRefresh(_ context.Context) (*RefreshReceipt, error) {
	return &RefreshReceipt{RefreshedAt: time.Now().Unix(), Refreshed: true}, nil
}

func (b *LocalBackend) RefreshJob(_ context.Context, _ string) error {
	return fmt.Errorf("refresh job in local mode: %w", ErrNotSupported)
}

func (b *LocalBackend) ListJobs(ctx context.Context, projectScope string) ([]JobSummary, error) {
	jobs, err := b.store.ListJobsVisible(ctx, projectScope, b.targetName)
	if err != nil {
		return nil, err
	}
	out := make([]JobSummary, 0, len(jobs))
	for _, j := range jobs {
		tasks, _ := b.store.ListTasks(ctx, store.TaskFilter{JobID: j.ID})
		out = append(out, BuildJobSummary(j, tasks))
	}
	return out, nil
}

func (b *LocalBackend) GetJob(ctx context.Context, jobID string) (*JobDetail, error) {
	j, err := b.store.GetJob(ctx, jobID)
	if err != nil {
		return nil, err
	}
	if j == nil {
		return nil, fmt.Errorf("job %q not found", jobID)
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

func (b *LocalBackend) CompareMetrics(ctx context.Context, jobID, key string, desc bool) ([]CompareRow, error) {
	return compareRowsFromDB(ctx, b.store, jobID, key, desc)
}

func (b *LocalBackend) GPUStatus(ctx context.Context) ([]GPUSlot, error) {
	if b.pool == nil {
		return []GPUSlot{}, nil
	}
	gpus := b.pool.Status()

	// Collect task IDs for batch job_id lookup.
	var taskIDs []string
	seen := make(map[string]bool, len(gpus))
	for _, g := range gpus {
		if g.TaskID != "" && !seen[g.TaskID] {
			seen[g.TaskID] = true
			taskIDs = append(taskIDs, g.TaskID)
		}
	}
	jobMap, _ := b.store.GetJobIDsForTasks(ctx, taskIDs)
	if jobMap == nil {
		jobMap = make(map[string]string)
	}

	out := make([]GPUSlot, 0, len(gpus))
	for _, g := range gpus {
		out = append(out, GPUSlot{
			Index:       g.Index,
			Name:        g.Name,
			MemTotalMB:  g.MemTotal,
			MemUsedMB:   g.MemTotal - g.MemFree,
			UtilPercent: g.UtilPct,
			TaskID:      g.TaskID,
			JobID:       jobMap[g.TaskID],
		})
	}
	return out, nil
}

// ── Task operations ───────────────────────────────────────────────────────

func (b *LocalBackend) GetTask(ctx context.Context, taskID string) (*TaskView, error) {
	task, err := b.store.GetTask(ctx, taskID)
	if err != nil {
		return nil, err
	}
	if task == nil {
		return nil, fmt.Errorf("task %q: %w", taskID, ErrNotFound)
	}
	view := BuildTaskView(*task)
	return &view, nil
}

// ListTasks — runqd's own ledger has no target dimension; the filter's
// Target is ignored (everything here is this machine).
func (b *LocalBackend) ListTasks(ctx context.Context, opts TaskListOptions) ([]TaskView, int, error) {
	opts.Target = ""
	return listTasksFromStore(ctx, b.store, opts)
}

func (b *LocalBackend) TaskMetrics(ctx context.Context, taskID string, afterTS int64) ([]MetricPoint, error) {
	task, err := b.store.GetTask(ctx, taskID)
	if err != nil {
		return nil, err
	}
	if task == nil {
		return nil, fmt.Errorf("task %q: %w", taskID, ErrNotFound)
	}
	_, _ = ingest.ReapIncremental(ctx, b.store, ingest.Target{
		TaskID: task.ID, JobID: task.JobID, Dir: task.TaskDir,
	}, false)
	return readTailMetricPoints(nil, task.TaskDir, "", 2000, afterTS), nil
}

// TaskMetricBuckets — bucket-mode chart; local disk, same pyramid/tail
// split as the lanes.
func (b *LocalBackend) TaskMetricBuckets(ctx context.Context, taskID, key string, fromTS, toTS int64, maxBuckets int) ([]workspace.PyramidBucket, string, error) {
	task, err := b.store.GetTask(ctx, taskID)
	if err != nil {
		return nil, "", err
	}
	if task == nil {
		return nil, "", fmt.Errorf("task %q: %w", taskID, ErrNotFound)
	}
	buckets, err := workspace.QueryPyramid(ctx, nil, task.TaskDir, key, fromTS, toTS, maxBuckets)
	switch {
	case err == nil:
		return buckets, "pyramid", nil
	case errors.Is(err, workspace.ErrPyramidNotBuilt):
		return tailMetricBuckets(nil, task.TaskDir, key, fromTS, toTS, maxBuckets), "tail", nil
	default:
		return nil, "", err
	}
}

// MetricKeys — SELECT DISTINCT over ingested rows (spec §5.4 dual-mode).
func (b *LocalBackend) MetricKeys(ctx context.Context, jobID string) ([]string, error) {
	return b.store.MetricKeys(ctx, jobID)
}

// KillTask terminates a running or pending task.
// Running: cancels the process via executor. Pending: marks killed in queue + DB.
func (b *LocalBackend) KillTask(ctx context.Context, taskID string) error {
	task := b.queue.Get(taskID)
	if task == nil {
		return fmt.Errorf("task %q not found in queue", taskID)
	}

	switch task.Status {
	case scheduler.StatusRunning:
		b.scheduler.KillTask(taskID)
	case scheduler.StatusPending:
		b.queue.Complete(taskID, scheduler.StatusKilled)
		_ = b.store.UpdateTaskStatus(ctx, taskID, "killed", map[string]any{
			"finished_at": time.Now().Unix(),
		})
		if b.scheduler != nil {
			b.scheduler.RefreshJobStatus(task.JobID)
		}
	default:
		return fmt.Errorf("task %q is %s, cannot kill", taskID, task.Status)
	}
	return nil
}

// RetryTask re-enqueues a failed or killed task.
func (b *LocalBackend) RetryTask(ctx context.Context, taskID string) error {
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

	if err := b.store.UpdateTaskStatus(ctx, taskID, "pending", map[string]any{
		"target_generation": b.generation, // RQ-75: retry = my new attempt
		"gpus":              nil,
		"pid":               nil,
		"started_at":        nil,
		"finished_at":       nil,
	}); err != nil {
		return err
	}

	row, _ = b.store.GetTask(ctx, taskID)
	task := TaskRowToSchedulerTask(row)
	if b.scheduler != nil {
		// Fresh attempt by explicit user intent — a stale kill flag must
		// not assassinate it at its first lifecycle event (RQ-69).
		b.scheduler.ClearKillRequest(taskID)
	}
	if !b.queue.RetryExisting(task) {
		b.queue.Push(task)
	}
	if b.scheduler != nil {
		b.scheduler.RefreshJobStatus(task.JobID)
	}
	return nil
}

func (b *LocalBackend) KillJob(ctx context.Context, jobID string) error {
	_, err := b.killJob(ctx, jobID)
	return err
}

func (b *LocalBackend) PauseJob(ctx context.Context, jobID string) error {
	j, err := b.store.GetJob(ctx, jobID)
	if err != nil {
		return err
	}
	if j == nil {
		return fmt.Errorf("job %q not found", jobID)
	}
	if store.IsTerminalJobStatus(j.Status) {
		return fmt.Errorf("job %q is already %s", jobID, j.Status)
	}
	b.scheduler.PauseJob(jobID)
	return b.store.UpdateJobStatus(ctx, jobID, "paused")
}

func (b *LocalBackend) ResumeJob(ctx context.Context, jobID string) error {
	j, err := b.store.GetJob(ctx, jobID)
	if err != nil {
		return err
	}
	if j == nil {
		return fmt.Errorf("job %q not found", jobID)
	}
	if j.Status != "paused" {
		return fmt.Errorf("job %q is %s, not paused", jobID, j.Status)
	}
	b.scheduler.ResumeJob(jobID)
	b.scheduler.RefreshJobStatus(jobID)
	return nil
}

// ── Submit ────────────────────────────────────────────────────────────────

func (b *LocalBackend) SubmitJob(ctx context.Context, cfg job.JobConfig, opts SubmitOptions) (string, int, error) {
	return b.submitJob(ctx, cfg, opts.SkipPreflight)
}

// PreviewSubmit: local mode has no submit_template to render.
func (b *LocalBackend) PreviewSubmit(_ context.Context, _ job.JobConfig, _ bool) (PreviewResult, error) {
	return PreviewResult{}, fmt.Errorf("submit preview in local mode: %w", ErrNotSupported)
}

func (b *LocalBackend) ResolveNote(ctx context.Context, cfg job.JobConfig) (string, error) {
	return resolveNote(ctx, b.store, cfg)
}

// ── Archive ───────────────────────────────────────────────────────────────

func (b *LocalBackend) ListArchivedJobs(ctx context.Context) ([]JobSummary, error) {
	jobs, err := b.store.ListJobsArchived(ctx, "", b.targetName)
	if err != nil {
		return nil, err
	}
	out := make([]JobSummary, 0, len(jobs))
	for _, j := range jobs {
		tasks, _ := b.store.ListTasks(ctx, store.TaskFilter{JobID: j.ID})
		out = append(out, BuildJobSummary(j, tasks))
	}
	return out, nil
}

func (b *LocalBackend) ArchiveJob(ctx context.Context, jobID string) error {
	return b.store.ArchiveJob(ctx, jobID)
}

func (b *LocalBackend) UnarchiveJob(ctx context.Context, jobID string) error {
	return b.store.UnarchiveJob(ctx, jobID)
}

// ── Project operations ────────────────────────────────────────────────────

func (b *LocalBackend) ListProjects(ctx context.Context) ([]ProjectSummary, error) {
	configs, err := b.reg.List(ctx)
	if err != nil {
		return nil, err
	}
	return b.configsToSummaries(ctx, configs)
}

// ── API-facing methods (raw store types, not view types) ─────────────────

// ProjectSummaries returns server-assembled project listing items with
// job counts and archive status. Used by the API's /projects/summaries
// endpoint.
func (b *LocalBackend) ProjectSummaries(ctx context.Context) ([]ProjectSummary, error) {
	configs, err := b.reg.List(ctx)
	if err != nil {
		return nil, err
	}
	jobs, err := b.store.ListJobs(ctx, "", b.targetName)
	if err != nil {
		return nil, err
	}
	counts := make(map[string]int, len(configs))
	for _, j := range jobs {
		counts[j.ProjectName]++
	}
	archived, err := b.reg.ArchivedNames(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]ProjectSummary, 0, len(configs))
	for _, c := range configs {
		out = append(out, ProjectSummary{
			Name:     c.ProjectName,
			WorkDir:  c.WorkingDir,
			JobCount: counts[c.ProjectName],
			Archived: archived[c.ProjectName],
		})
	}
	return out, nil
}

// ListJobsRaw returns visible job rows (not view types). Used by the API
// server which builds view types itself.
func (b *LocalBackend) ListJobsRaw(ctx context.Context, projectFilter string) ([]store.JobRow, error) {
	return b.store.ListJobsVisible(ctx, projectFilter, b.targetName)
}

// ListArchivedJobsRaw returns archived job rows.
func (b *LocalBackend) ListArchivedJobsRaw(ctx context.Context, projectFilter string) ([]store.JobRow, error) {
	return b.store.ListJobsArchived(ctx, projectFilter, b.targetName)
}

// ShowJob returns the raw job row and its tasks. Used by the API server
// which builds backend.JobDetail itself.
func (b *LocalBackend) ShowJob(ctx context.Context, jobID string) (*store.JobRow, []store.TaskRow, error) {
	j, err := b.store.GetJob(ctx, jobID)
	if err != nil {
		return nil, nil, err
	}
	if j == nil {
		return nil, nil, fmt.Errorf("job %q not found", jobID)
	}
	tasks, err := b.store.ListTasks(ctx, store.TaskFilter{JobID: jobID})
	if err != nil {
		return nil, nil, err
	}
	return j, tasks, nil
}

// SubmitJobRaw is the submit entry point used by the API server, returning
// the raw (jobID, taskCount) without going through the Backend interface's
// SubmitOptions wrapper.
func (b *LocalBackend) SubmitJobRaw(ctx context.Context, jobCfg job.JobConfig, skipPreflight bool) (string, int, error) {
	return b.submitJob(ctx, jobCfg, skipPreflight)
}

// KillJobRaw kills a job and returns the count of affected tasks.
func (b *LocalBackend) KillJobRaw(ctx context.Context, jobID string) (int, error) {
	return b.killJob(ctx, jobID)
}

// ── Internal helpers ─────────────────────────────────────────────────────

// resolveNote renders note placeholders against this project's existing
// job notes ({{version}} family scan needs them).
func resolveNote(ctx context.Context, st *store.Store, cfg job.JobConfig) (string, error) {
	rows, err := st.ListJobs(ctx, cfg.Project, "")
	if err != nil {
		return "", fmt.Errorf("list jobs for note rendering: %w", err)
	}
	notes := make([]string, 0, len(rows))
	for _, r := range rows {
		notes = append(notes, r.Note)
	}
	return job.RenderNote(&cfg, job.NoteContext{
		Project: cfg.Project, Now: time.Now(), ExistingNotes: notes,
	})
}

// submitJob validates, expands, persists, and enqueues a job.
func (b *LocalBackend) submitJob(ctx context.Context, jobCfg job.JobConfig, skipPreflight bool) (string, int, error) {
	proj, err := b.reg.Get(ctx, jobCfg.Project)
	if err != nil {
		return "", 0, fmt.Errorf("project %q not found", jobCfg.Project)
	}
	if err := b.reg.RequireSubmittable(ctx, jobCfg.Project); err != nil {
		return "", 0, err
	}

	wsRoot, err := config.ResolveRoot(b.storageCfg, proj.WorkingDir, proj.ProjectName)
	if err != nil {
		return "", 0, fmt.Errorf("resolve workspace root: %w", err)
	}

	jobID := utils.GenerateJobID()

	planCfg := jobCfg
	if resolved, nerr := resolveNote(ctx, b.store, jobCfg); nerr != nil {
		return "", 0, nerr
	} else {
		planCfg.Note = resolved
	}
	jobRoot := filepath.Join(wsRoot, workspace.JobDirName(planCfg.Note, jobID))

	plan, err := submitplan.Build(ctx, planCfg, proj, submitplan.Deps{
		JobID: jobID,
		IDGen: utils.GenerateTaskID,
		Paths: submitplan.Paths{
			WorkspaceRoot: jobRoot,
			LogRoot:       jobRoot,
		},
		SkipPreflight: skipPreflight,
	})
	if err != nil {
		return "", 0, err
	}

	if err := submitplan.RunSetup(ctx, proj, jobCfg, nil, utils.EnvPrelude{EnvFile: plan.Env["RUNQ_ENV_FILE"], Env: plan.Env}); err != nil {
		return "", 0, err
	}

	// A6: reject if gpus_per_task exceeds total available GPUs.
	if b.pool != nil {
		total := b.pool.TotalCount()
		if plan.GPUsPerTask > total {
			return "", 0, fmt.Errorf("gpus_per_task (%d) exceeds total GPUs (%d)", plan.GPUsPerTask, total)
		}
	}

	now := time.Now()
	tasks := make([]*scheduler.Task, 0, len(plan.Tasks))
	for _, planned := range plan.Tasks {
		if err := workspace.Write(planned.TaskDir, planned.Params, plan.Wandb, plan.Note); err != nil {
			return "", 0, fmt.Errorf("prepare task workspace for %s: %w", planned.TaskID, err)
		}

		tasks = append(tasks, &scheduler.Task{
			ID:            planned.TaskID,
			JobID:         plan.JobID,
			ProjectName:   plan.Project,
			Command:       planned.Command,
			Params:        planned.Params,
			GPUsNeeded:    planned.GPUsNeeded,
			MaxRetry:      planned.MaxRetry,
			LogPath:       planned.LogPath,
			WorkingDir:    planned.WorkingDir,
			Env:           planned.Env,
			Resumable:     planned.Resumable,
			ExtraArgs:     planned.ExtraArgs,
			Timeout:       planned.Timeout,
			UID:           planned.UID,
			TaskDir:       planned.TaskDir,
			CheckpointDir: planned.CheckpointDir,
			SweepKeys:     plan.SweepKeys,
			JobNote:       plan.Note,
		})
	}

	cfgJSON, err := json.Marshal(jobCfg)
	if err != nil {
		return "", 0, fmt.Errorf("marshal job config: %w", err)
	}

	jobRow := store.JobRow{
		ID: plan.JobID, ProjectName: plan.Project, Description: plan.Description,
		Note: plan.Note, ConfigJSON: string(cfgJSON), Status: "pending", TotalTasks: len(plan.Tasks), CreatedAt: now,
		Target: b.targetName,
	}
	taskRows := make([]store.TaskRow, 0, len(tasks))
	for _, t := range tasks {
		paramsJSON, _ := json.Marshal(t.Params)
		envJSON, _ := json.Marshal(t.Env)
		taskRows = append(taskRows, store.TaskRow{
			ID: t.ID, JobID: t.JobID, ProjectName: t.ProjectName,
			Command: t.Command, ParamsJSON: string(paramsJSON), GPUsNeeded: t.GPUsNeeded,
			Status: "pending", MaxRetry: t.MaxRetry, LogPath: t.LogPath,
			WorkingDir: t.WorkingDir, EnvJSON: string(envJSON),
			Resumable: t.Resumable, ExtraArgs: t.ExtraArgs,
			UID: t.UID, Timeout: t.Timeout, EnqueuedAt: now,
			TaskDir: t.TaskDir, Target: b.targetName, TargetGeneration: b.generation,
		})
	}

	if err := b.store.InsertJobWithTasks(ctx, &jobRow, taskRows); err != nil {
		return "", 0, fmt.Errorf("persist job: %w", err)
	}

	b.queue.PushBatch(tasks)
	return plan.JobID, len(tasks), nil
}

// killJob kills all running/pending tasks in a job. Returns count of affected tasks.
func (b *LocalBackend) killJob(ctx context.Context, jobID string) (int, error) {
	tasks := b.queue.ListByJob(jobID)
	if len(tasks) == 0 {
		return 0, fmt.Errorf("job %q not found", jobID)
	}

	killed := 0
	for _, t := range tasks {
		if t.Status == scheduler.StatusRunning {
			b.scheduler.KillTask(t.ID)
			killed++
		} else if t.Status == scheduler.StatusPending {
			_ = b.queue.Complete(t.ID, scheduler.StatusKilled)
			_ = b.store.UpdateTaskStatus(ctx, t.ID, "killed", map[string]any{
				"finished_at": time.Now().Unix(),
			})
			killed++
		}
	}
	// Kill overrides pause (human intent supersedes human intent, latest
	// wins): drop the flag so the aggregate below can land the terminal
	// status instead of parking at "paused" forever. nil-guarded like every
	// scheduler touchpoint here — test harnesses run without a scheduler.
	if b.scheduler != nil {
		b.scheduler.ClearPause(jobID)
	}
	if err := b.refreshJobStatus(ctx, jobID); err != nil {
		return killed, err
	}
	return killed, nil
}

func (b *LocalBackend) refreshJobStatus(ctx context.Context, jobID string) error {
	tasks, err := b.store.ListTasks(ctx, store.TaskFilter{JobID: jobID})
	if err != nil {
		return err
	}

	var running, pending, unknown, success, failed, killed int
	for _, t := range tasks {
		switch t.Status {
		case "running":
			running++
		case "pending":
			pending++
		case "unknown":
			// RQ-74: outcome-unknown submission (remote lane only, but this
			// rollup mirrors the others) — live work, blocks the terminal
			// split.
			unknown++
		case "success":
			success++
		case "failed":
			failed++
		case "killed":
			killed++
		}
	}

	isStarted := (running + unknown + success + failed + killed) > 0
	isEnded := (pending + running + unknown) == 0

	// Unconditional pause guard — mirrors scheduler.RefreshJobStatus: paused
	// is a human control state that outlives task terminality; resume/kill
	// are the only releases (both clear the set before re-aggregating).
	if b.scheduler != nil && b.scheduler.IsJobPaused(jobID) {
		return nil
	}

	var newStatus string
	if isEnded {
		newStatus = store.TerminalJobStatus(success, failed, killed)
	} else if isStarted {
		newStatus = "running"
	} else {
		newStatus = "pending"
	}

	return b.store.UpdateJobStatus(ctx, jobID, newStatus)
}

// compile-time interface check
var _ Backend = (*LocalBackend)(nil)
