package backend

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/gliese129/runq/internal/hpc"
	"github.com/gliese129/runq/internal/job"
	"github.com/gliese129/runq/internal/project"
	"github.com/gliese129/runq/internal/store"
)

type HPCBackend struct {
	backend *hpc.Backend
	store   *store.Store
}

func NewHPCBackend(backend *hpc.Backend, st *store.Store) *HPCBackend {
	return &HPCBackend{backend: backend, store: st}
}

func (b *HPCBackend) Capabilities() Capabilities {
	return Capabilities{
		GPUMap:        false,  // no node-local GPU visibility from the login node
		PauseResume:   false,  // cluster queues have no runq-level pause concept
		LiveLog:       true,   // deployment assumption: dashboard runs on a login node with the shared FS
		Retry:         false,  // HPC has no resident process to re-pick pending tasks
		StateModel:    "poll", // best-effort projection; staleness must be surfaced
		KillAsync:     true,   // qdel/scancel is forwarded; killed persisted as soon as the cancel command succeeds
		SubmitPreview: true,   // zero-disk dry-run via the submit code path
		// Activity heatmap and log search handlers are still TODO stubs
		// (server.go handleJobActivity p6-step4, handleJobLogSearch p6-step5).
		// Keep the capabilities off until the endpoints have real behavior so
		// the UI doesn't render affordances for unimplemented features.
		ActivityHeatmap: false,
		LogSearch:       false,
	}
}

// RefreshJob forces a reconcile pass (status.json + optional scheduler probe).
func (b *HPCBackend) RefreshJob(ctx context.Context, jobID string) error {
	return b.backend.EnsureFresh(ctx, jobID, 0)
}

func (b *HPCBackend) ListJobs(ctx context.Context, projectScope string) ([]JobSummary, error) {
	// Query visible jobs first to scope the reconcile to exactly this result
	// set. Without this, an unscoped EnsureAllFresh would reconcile every
	// active job in the DB (including archived/hidden projects) — wasteful on
	// a cluster with many projects (RQ-26 P2).
	jobs, err := b.store.ListJobsVisible(ctx, projectScope)
	if err != nil {
		return nil, err
	}

	// Reconcile only the non-done jobs in this result set (best-effort: a
	// reconcile failure should not block the list entirely).
	for _, j := range jobs {
		if j.Status != "done" {
			_ = b.backend.EnsureFresh(ctx, j.ID, DefaultReadTTL)
		}
	}

	// Re-query to pick up any status changes from the reconcile pass.
	jobs, err = b.store.ListJobsVisible(ctx, projectScope)
	if err != nil {
		return nil, err
	}

	// Load all tasks once and group by job_id to avoid N+1 queries.
	allTasks, err := b.store.ListTasks(ctx, store.TaskFilter{})
	if err != nil {
		return nil, err
	}
	byJob := make(map[string][]store.TaskRow, len(jobs))
	for _, t := range allTasks {
		byJob[t.JobID] = append(byJob[t.JobID], t)
	}
	out := make([]JobSummary, 0, len(jobs))
	for _, job := range jobs {
		out = append(out, BuildJobSummary(job, byJob[job.ID]))
	}
	return out, nil
}

func (b *HPCBackend) ListArchivedJobs(ctx context.Context) ([]JobSummary, error) {
	jobs, err := b.store.ListJobsArchived(ctx, "")
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

func (b *HPCBackend) ArchiveJob(ctx context.Context, jobID string) error {
	if err := b.backend.EnsureFresh(ctx, jobID, 0); err != nil {
		return fmt.Errorf("reconcile before archive: %w", err)
	}
	return b.store.ArchiveJob(ctx, jobID)
}

func (b *HPCBackend) UnarchiveJob(ctx context.Context, jobID string) error {
	return b.store.UnarchiveJob(ctx, jobID)
}

func (b *HPCBackend) ArchiveProject(ctx context.Context, name string) error {
	return project.NewRegistry(b.store.DB()).Archive(ctx, name)
}

func (b *HPCBackend) UnarchiveProject(ctx context.Context, name string) error {
	return project.NewRegistry(b.store.DB()).Unarchive(ctx, name)
}

func (b *HPCBackend) GetJob(ctx context.Context, jobID string) (*JobDetail, error) {
	if err := b.backend.EnsureFresh(ctx, jobID, DefaultReadTTL); err != nil {
		return nil, err
	}
	job, err := b.store.GetJob(ctx, jobID)
	if err != nil {
		return nil, err
	}
	if job == nil {
		return nil, fmt.Errorf("job %q not found", jobID)
	}
	tasks, err := b.store.ListTasks(ctx, store.TaskFilter{JobID: jobID})
	if err != nil {
		return nil, err
	}
	detail := BuildJobDetail(*job, tasks)
	reg := project.NewRegistry(b.store.DB())
	if cfg, err := reg.Get(ctx, job.ProjectName); err == nil && cfg.Wandb != nil {
		detail.Wandb = &WandbInfo{
			Entity:  cfg.Wandb.Entity,
			Project: cfg.Wandb.Project,
			BaseURL: WandbBaseURL(cfg.Wandb.Entity, cfg.Wandb.Project),
		}
	}
	return &detail, nil
}

func (b *HPCBackend) CompareMetrics(ctx context.Context, jobID, key string, desc bool) ([]CompareRow, error) {
	if err := b.backend.EnsureFresh(ctx, jobID, DefaultReadTTL); err != nil {
		return nil, err
	}
	tasks, err := b.store.ListTasks(ctx, store.TaskFilter{JobID: jobID})
	if err != nil {
		return nil, err
	}
	return BuildCompareRows(tasks, key, desc), nil
}

func (b *HPCBackend) GPUStatus(ctx context.Context) ([]GPUSlot, error) {
	return []GPUSlot{}, nil
}

func (b *HPCBackend) GetTask(ctx context.Context, taskID string) (*TaskView, string, error) {
	task, err := b.store.GetTask(ctx, taskID)
	if err != nil {
		return nil, "", err
	}
	if task == nil {
		return nil, "", fmt.Errorf("task %q not found", taskID)
	}
	// Lazy reconcile from the external scheduler before reading detail, the
	// same read-path reconcile the list/compare endpoints already do (the
	// scheduler probe rides the throttle, so polling this page is cheap).
	// Without it the task page can poll forever on stale DB state — a task
	// finished externally never advances until some other reconciled path runs.
	task = b.reconcileTask(ctx, taskID, task)
	view := BuildTaskView(*task)
	return &view, task.LogPath, nil
}

func (b *HPCBackend) TaskMetrics(ctx context.Context, taskID string) ([]MetricPoint, error) {
	task, err := b.store.GetTask(ctx, taskID)
	if err != nil || task == nil {
		return nil, fmt.Errorf("task %q not found", taskID)
	}
	// Same lazy reconcile as GetTask: status-sensitive read paths must run the
	// reconcile pass so metrics/status don't lag behind external completion.
	task = b.reconcileTask(ctx, taskID, task)
	return readMetricPoints(task.TaskDir), nil
}

// reconcileTask ensures the task's job data is fresh, then returns the freshest
// task row. On any error it falls back to the row passed in, so callers always
// get a usable task.
func (b *HPCBackend) reconcileTask(ctx context.Context, taskID string, fallback *store.TaskRow) *store.TaskRow {
	if err := b.backend.EnsureFresh(ctx, fallback.JobID, DefaultReadTTL); err != nil {
		return fallback
	}
	if fresh, err := b.store.GetTask(ctx, taskID); err == nil && fresh != nil {
		return fresh
	}
	return fallback
}

func (b *HPCBackend) KillTask(ctx context.Context, taskID string) error {
	task, err := b.store.GetTask(ctx, taskID)
	if err != nil {
		return err
	}
	if task == nil {
		return fmt.Errorf("task %q not found", taskID)
	}
	// Reconcile before kill so we don't send qdel for an already-finished task.
	if err := b.backend.EnsureFresh(ctx, task.JobID, 0); err != nil {
		return fmt.Errorf("reconcile before kill: %w", err)
	}
	// Re-read: the reconcile may have advanced the task to a terminal state.
	task, err = b.store.GetTask(ctx, taskID)
	if err != nil {
		return err
	}
	if task == nil || task.Status == "success" || task.Status == "failed" || task.Status == "killed" {
		return nil // already finished, nothing to kill
	}
	_, err = b.backend.Kill(ctx, taskID)
	return err
}

// RetryTask is not supported in HPC mode — there is no resident process to
// re-submit the task to the cluster scheduler after resetting its DB state.
func (b *HPCBackend) RetryTask(_ context.Context, _ string) error {
	return fmt.Errorf("retry task in hpc mode: %w", ErrNotSupported)
}

func (b *HPCBackend) KillJob(ctx context.Context, jobID string) error {
	if err := b.backend.EnsureFresh(ctx, jobID, 0); err != nil {
		return fmt.Errorf("reconcile before kill: %w", err)
	}
	_, err := b.backend.Kill(ctx, jobID)
	return err
}

func (b *HPCBackend) PauseJob(ctx context.Context, jobID string) error {
	return fmt.Errorf("pause job in hpc mode: %w", ErrNotSupported)
}

func (b *HPCBackend) ResumeJob(ctx context.Context, jobID string) error {
	return fmt.Errorf("resume job in hpc mode: %w", ErrNotSupported)
}

func (b *HPCBackend) SubmitJob(ctx context.Context, cfg job.JobConfig, opts SubmitOptions) (string, int, error) {
	reg := project.NewRegistry(b.store.DB())
	proj, err := reg.Get(ctx, cfg.Project)
	if err != nil {
		return "", 0, fmt.Errorf("project %q not found: %w", cfg.Project, err)
	}
	return b.backend.Submit(ctx, cfg, proj, hpc.SubmitOpts{SkipPreflight: opts.SkipPreflight})
}

func (b *HPCBackend) DryRun(_ context.Context, cfg job.JobConfig) ([]job.TaskParams, error) {
	return job.Expand(&cfg)
}

// PreviewSubmit is the GUI face of `--dry-run`: same code path, same text.
func (b *HPCBackend) PreviewSubmit(ctx context.Context, cfg job.JobConfig, skipPreflight bool) (string, error) {
	reg := project.NewRegistry(b.store.DB())
	proj, err := reg.Get(ctx, cfg.Project)
	if err != nil {
		return "", fmt.Errorf("project %q not found: %w", cfg.Project, err)
	}
	return b.backend.Preview(ctx, cfg, proj, skipPreflight)
}

func (b *HPCBackend) ResolveNote(ctx context.Context, cfg job.JobConfig) (string, error) {
	rows, err := b.store.ListJobs(ctx, cfg.Project)
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

func (b *HPCBackend) CreateProject(ctx context.Context, cfg project.Config) error {
	reg := project.NewRegistry(b.store.DB())
	return reg.Add(ctx, cfg)
}

func (b *HPCBackend) UpdateProject(ctx context.Context, cfg project.Config) error {
	reg := project.NewRegistry(b.store.DB())
	return reg.Update(ctx, cfg)
}

func (b *HPCBackend) DeleteProject(ctx context.Context, name string) error {
	reg := project.NewRegistry(b.store.DB())
	return reg.Remove(ctx, name)
}

func (b *HPCBackend) RenameProject(ctx context.Context, oldName, newName string) error {
	reg := project.NewRegistry(b.store.DB())
	return reg.Rename(ctx, oldName, newName)
}

func (b *HPCBackend) GetProject(ctx context.Context, name string) (*project.Config, error) {
	reg := project.NewRegistry(b.store.DB())
	return reg.Get(ctx, name)
}

func (b *HPCBackend) ListProjects(ctx context.Context) ([]ProjectSummary, error) {
	reg := project.NewRegistry(b.store.DB())
	configs, err := reg.List(ctx)
	if err != nil {
		return nil, err
	}
	return b.configsToSummaries(ctx, configs)
}

func (b *HPCBackend) MatchProjects(ctx context.Context, dir string) ([]ProjectSummary, error) {
	reg := project.NewRegistry(b.store.DB())
	configs, err := reg.Match(ctx, dir)
	if err != nil {
		return nil, err
	}
	return b.configsToSummaries(ctx, configs)
}

func (b *HPCBackend) DeleteJob(ctx context.Context, jobID string) error {
	j, err := b.store.GetJob(ctx, jobID)
	if err != nil {
		return err
	}
	if j == nil {
		return fmt.Errorf("job %q not found", jobID)
	}
	if j.Status != "done" {
		return fmt.Errorf("job %q is %s, only completed jobs can be deleted (kill it first?)", jobID, j.Status)
	}
	return b.store.DeleteJob(ctx, jobID)
}

func (b *HPCBackend) CleanOldTasks(ctx context.Context, cutoff time.Time, dryRun bool) (*CleanResult, error) {
	tasks, err := b.store.ListFinishedTasksBefore(ctx, cutoff)
	if err != nil {
		return nil, fmt.Errorf("query tasks: %w", err)
	}

	if dryRun {
		preview := make([]CleanPreviewItem, 0, len(tasks))
		for _, t := range tasks {
			preview = append(preview, CleanPreviewItem{
				TaskID:     t.ID,
				Status:     t.Status,
				FinishedAt: t.FinishedAt,
				TaskDir:    t.TaskDir,
			})
		}
		return &CleanResult{Preview: preview}, nil
	}

	var deletedTasks int
	var freedBytes int64
	for _, t := range tasks {
		freedBytes += cleanTaskArtifacts(t)
		if err := b.store.DeleteTask(ctx, t.ID); err != nil {
			continue
		}
		deletedTasks++
	}

	deletedJobs, _ := b.store.DeleteOrphanJobs(ctx)

	return &CleanResult{
		Tasks:      deletedTasks,
		Jobs:       int(deletedJobs),
		FreedBytes: freedBytes,
	}, nil
}

// cleanTaskArtifacts removes the task's workspace directory and log file.
func cleanTaskArtifacts(t store.TaskRow) int64 {
	var freed int64
	if t.TaskDir != "" {
		freed += dirSize(t.TaskDir)
		os.RemoveAll(t.TaskDir)
	}
	if t.LogPath != "" && (t.TaskDir == "" || !strings.HasPrefix(t.LogPath, t.TaskDir)) {
		if info, err := os.Stat(t.LogPath); err == nil {
			freed += info.Size()
			os.Remove(t.LogPath)
		}
	}
	return freed
}

func dirSize(dir string) int64 {
	var total int64
	filepath.Walk(dir, func(_ string, info os.FileInfo, err error) error {
		if err == nil && !info.IsDir() {
			total += info.Size()
		}
		return nil
	})
	return total
}

func (b *HPCBackend) configsToSummaries(ctx context.Context, configs []project.Config) ([]ProjectSummary, error) {
	// Count jobs per project
	jobs, _ := b.store.ListJobs(ctx, "")
	jobCounts := make(map[string]int)
	for _, j := range jobs {
		jobCounts[j.ProjectName]++
	}
	archived, _ := project.NewRegistry(b.store.DB()).ArchivedNames(ctx)
	out := make([]ProjectSummary, 0, len(configs))
	for _, c := range configs {
		out = append(out, ProjectSummary{
			Name:     c.ProjectName,
			WorkDir:  c.WorkingDir,
			JobCount: jobCounts[c.ProjectName],
			Archived: archived[c.ProjectName],
		})
	}
	return out, nil
}
