package backend

import (
	"context"
	"fmt"

	"github.com/gliese129/runq/internal/job"
	"github.com/gliese129/runq/internal/project"
	"github.com/gliese129/runq/internal/resource"
	"github.com/gliese129/runq/internal/service"
	"github.com/gliese129/runq/internal/store"
)

// LocalBackend implements Backend by calling the daemon's service layer
// directly (in-process). Used by the embedded dashboard for the local GPU
// target — no HTTP proxy overhead.
//
// All state is push-model: the daemon owns the truth via scheduler/executor,
// so data is always current — no reconcile needed.
type LocalBackend struct {
	storeBackend // embeds store, reg, and shared project/clean/thaw/dryrun methods
	jobSvc       *service.JobService
	taskSvc      *service.TaskService
	pool         resource.Allocator
}

// LocalBackendDeps groups the daemon components that LocalBackend wraps.
type LocalBackendDeps struct {
	Store   *store.Store
	JobSvc  *service.JobService
	TaskSvc *service.TaskService
	Pool    resource.Allocator
	Reg     *project.Registry
}

// NewLocalBackend creates a Backend that wraps the daemon's internal
// components. All calls are direct function invocations — no HTTP, no
// serialization overhead.
func NewLocalBackend(deps LocalBackendDeps) *LocalBackend {
	return &LocalBackend{
		storeBackend: storeBackend{store: deps.Store, reg: deps.Reg},
		jobSvc:       deps.JobSvc,
		taskSvc:      deps.TaskSvc,
		pool:         deps.Pool,
	}
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
func (b *LocalBackend) RefreshJob(_ context.Context, _ string) error {
	return fmt.Errorf("refresh job in local mode: %w", ErrNotSupported)
}

func (b *LocalBackend) ListJobs(ctx context.Context, projectScope string) ([]JobSummary, error) {
	jobs, err := b.jobSvc.ListJobs(ctx, projectScope)
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
	j, tasks, err := b.jobSvc.ShowJob(ctx, jobID)
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

func (b *LocalBackend) CompareMetrics(ctx context.Context, jobID, key string, desc bool) ([]CompareRow, error) {
	tasks, err := b.store.ListTasks(ctx, store.TaskFilter{JobID: jobID})
	if err != nil {
		return nil, err
	}
	return BuildCompareRows(tasks, key, desc), nil
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

func (b *LocalBackend) TaskMetrics(ctx context.Context, taskID string) ([]MetricPoint, error) {
	task, err := b.store.GetTask(ctx, taskID)
	if err != nil {
		return nil, err
	}
	if task == nil {
		return nil, fmt.Errorf("task %q: %w", taskID, ErrNotFound)
	}
	return readMetricPoints(task.TaskDir), nil
}

func (b *LocalBackend) KillTask(ctx context.Context, taskID string) error {
	return b.taskSvc.KillTask(ctx, taskID)
}

func (b *LocalBackend) RetryTask(ctx context.Context, taskID string) error {
	return b.taskSvc.RetryTask(ctx, taskID)
}

func (b *LocalBackend) KillJob(ctx context.Context, jobID string) error {
	_, err := b.jobSvc.KillJob(ctx, jobID)
	return err
}

func (b *LocalBackend) PauseJob(ctx context.Context, jobID string) error {
	return b.jobSvc.PauseJob(ctx, jobID)
}

func (b *LocalBackend) ResumeJob(ctx context.Context, jobID string) error {
	return b.jobSvc.ResumeJob(ctx, jobID)
}

// ── Submit ────────────────────────────────────────────────────────────────

func (b *LocalBackend) SubmitJob(ctx context.Context, cfg job.JobConfig, opts SubmitOptions) (string, int, error) {
	return b.jobSvc.SubmitJobWithOpts(ctx, cfg, service.SubmitJobOpts{
		SkipPreflight: opts.SkipPreflight,
	})
}

// PreviewSubmit: local mode has no submit_template to render.
func (b *LocalBackend) PreviewSubmit(_ context.Context, _ job.JobConfig, _ bool) (string, error) {
	return "", fmt.Errorf("submit preview in local mode: %w", ErrNotSupported)
}

func (b *LocalBackend) ResolveNote(ctx context.Context, cfg job.JobConfig) (string, error) {
	return b.jobSvc.ResolveNote(ctx, cfg)
}

// ── Archive ───────────────────────────────────────────────────────────────

func (b *LocalBackend) ListArchivedJobs(ctx context.Context) ([]JobSummary, error) {
	jobs, err := b.jobSvc.ListArchivedJobs(ctx, "")
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
	return b.jobSvc.ArchiveJob(ctx, jobID)
}

func (b *LocalBackend) UnarchiveJob(ctx context.Context, jobID string) error {
	return b.jobSvc.UnarchiveJob(ctx, jobID)
}

// ── Project operations ────────────────────────────────────────────────────

func (b *LocalBackend) ListProjects(ctx context.Context) ([]ProjectSummary, error) {
	svc := b.jobSvc.ProjectSummaries
	sums, err := svc(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]ProjectSummary, 0, len(sums))
	for _, s := range sums {
		out = append(out, ProjectSummary{
			Name:     s.Name,
			WorkDir:  s.WorkDir,
			JobCount: s.JobCount,
			Archived: s.Archived,
		})
	}
	return out, nil
}

// compile-time interface check
var _ Backend = (*LocalBackend)(nil)
