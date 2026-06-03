package dashboard

import (
	"context"
	"fmt"

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

func (b *HPCBackend) ListJobs(ctx context.Context) ([]JobSummary, error) {
	jobs, err := b.store.ListJobs(ctx, "")
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

func (b *HPCBackend) GetJob(ctx context.Context, jobID string) (*JobDetail, error) {
	view, err := b.backend.Status(ctx, jobID)
	if err != nil {
		return nil, err
	}
	detail := BuildJobDetail(*view.Job, view.Tasks)
	reg := project.NewRegistry(b.store.DB())
	if cfg, err := reg.Get(view.Job.ProjectName); err == nil && cfg.Wandb != nil {
		detail.Wandb = &WandbInfo{
			Entity:  cfg.Wandb.Entity,
			Project: cfg.Wandb.Project,
			BaseURL: wandbBaseURL(cfg.Wandb.Entity, cfg.Wandb.Project),
		}
	}
	return &detail, nil
}

func (b *HPCBackend) CompareMetrics(ctx context.Context, jobID, key string, desc bool) ([]CompareRow, error) {
	if err := b.backend.Refresh(ctx, jobID); err != nil {
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

func (b *HPCBackend) KillTask(ctx context.Context, taskID string) error {
	task, err := b.store.GetTask(ctx, taskID)
	if err != nil {
		return err
	}
	if task == nil {
		return fmt.Errorf("task %q not found", taskID)
	}
	_, err = b.backend.Kill(ctx, taskID)
	return err
}

func (b *HPCBackend) RetryTask(ctx context.Context, taskID string) error {
	task, err := b.store.GetTask(ctx, taskID)
	if err != nil {
		return err
	}
	if task == nil {
		return fmt.Errorf("task %q not found", taskID)
	}
	if task.Status != "failed" && task.Status != "killed" {
		return fmt.Errorf("task %q is %s, only failed/killed tasks can be retried", taskID, task.Status)
	}
	if err := b.store.UpdateTaskStatus(ctx, taskID, "pending", map[string]any{
		"gpus":          nil,
		"pid":           nil,
		"start_time":    nil,
		"started_at":    nil,
		"finished_at":   nil,
		"retry_count":   task.RetryCount + 1,
		"external_id":   nil,
		"status_source": nil,
	}); err != nil {
		return err
	}
	return refreshStoreJobStatus(ctx, b.store, task.JobID)
}

func (b *HPCBackend) KillJob(ctx context.Context, jobID string) error {
	_, err := b.backend.Kill(ctx, jobID)
	return err
}

func (b *HPCBackend) PauseJob(ctx context.Context, jobID string) error {
	return fmt.Errorf("pause job in hpc mode: %w", ErrNotSupported)
}

func (b *HPCBackend) ResumeJob(ctx context.Context, jobID string) error {
	return fmt.Errorf("resume job in hpc mode: %w", ErrNotSupported)
}

func (b *HPCBackend) SubmitJob(ctx context.Context, cfg job.JobConfig) (string, int, error) {
	reg := project.NewRegistry(b.store.DB())
	proj, err := reg.Get(cfg.Project)
	if err != nil {
		return "", 0, fmt.Errorf("project %q not found: %w", cfg.Project, err)
	}
	return b.backend.Submit(ctx, cfg, proj, hpc.SubmitOpts{})
}

func (b *HPCBackend) DryRun(_ context.Context, cfg job.JobConfig) ([]job.TaskParams, error) {
	return job.Expand(&cfg)
}

func (b *HPCBackend) ListProjects(ctx context.Context) ([]ProjectSummary, error) {
	reg := project.NewRegistry(b.store.DB())
	configs, err := reg.List()
	if err != nil {
		return nil, err
	}
	// Count jobs per project
	jobs, _ := b.store.ListJobs(ctx, "")
	jobCounts := make(map[string]int)
	for _, j := range jobs {
		jobCounts[j.ProjectName]++
	}
	out := make([]ProjectSummary, 0, len(configs))
	for _, c := range configs {
		out = append(out, ProjectSummary{
			Name:     c.ProjectName,
			WorkDir:  c.WorkingDir,
			JobCount: jobCounts[c.ProjectName],
		})
	}
	return out, nil
}
