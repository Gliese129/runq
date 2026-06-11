package dashboard

import (
	"context"
	"fmt"
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
		GPUMap:      false, // no node-local GPU visibility from the login node
		PauseResume: false, // cluster queues have no runq-level pause concept
		LiveLog:     true,  // deployment assumption: dashboard runs on a login node with the shared FS
		Retry:       true,
		StateModel:  "poll", // best-effort projection; staleness must be surfaced
		KillAsync:   true,   // qdel/scancel is forwarded; killed only after a poll confirms
	}
}

// RefreshJob forces a reconcile pass (status.json + optional scheduler probe).
func (b *HPCBackend) RefreshJob(ctx context.Context, jobID string) error {
	return b.backend.Refresh(ctx, jobID)
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

func (b *HPCBackend) GetTask(ctx context.Context, taskID string) (*TaskView, string, error) {
	task, err := b.store.GetTask(ctx, taskID)
	if err != nil {
		return nil, "", err
	}
	if task == nil {
		return nil, "", fmt.Errorf("task %q not found", taskID)
	}
	view := BuildTaskView(*task)
	return &view, task.LogPath, nil
}

func (b *HPCBackend) TaskMetrics(ctx context.Context, taskID string) ([]MetricPoint, error) {
	task, err := b.store.GetTask(ctx, taskID)
	if err != nil || task == nil {
		return nil, fmt.Errorf("task %q not found", taskID)
	}
	return readMetricPoints(task.TaskDir), nil
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

func (b *HPCBackend) SubmitJob(ctx context.Context, cfg job.JobConfig, opts SubmitOptions) (string, int, error) {
	reg := project.NewRegistry(b.store.DB())
	proj, err := reg.Get(cfg.Project)
	if err != nil {
		return "", 0, fmt.Errorf("project %q not found: %w", cfg.Project, err)
	}
	return b.backend.Submit(ctx, cfg, proj, hpc.SubmitOpts{SkipPreflight: opts.SkipPreflight})
}

func (b *HPCBackend) DryRun(_ context.Context, cfg job.JobConfig) ([]job.TaskParams, error) {
	return job.Expand(&cfg)
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

func (b *HPCBackend) CreateProject(_ context.Context, cfg project.Config) error {
	reg := project.NewRegistry(b.store.DB())
	return reg.Add(cfg)
}

func (b *HPCBackend) UpdateProject(_ context.Context, cfg project.Config) error {
	reg := project.NewRegistry(b.store.DB())
	return reg.Update(cfg)
}

func (b *HPCBackend) RenameProject(_ context.Context, oldName, newName string) error {
	reg := project.NewRegistry(b.store.DB())
	return reg.Rename(oldName, newName)
}

func (b *HPCBackend) GetProject(_ context.Context, name string) (*project.Config, error) {
	reg := project.NewRegistry(b.store.DB())
	return reg.Get(name)
}

func (b *HPCBackend) ListProjects(ctx context.Context) ([]ProjectSummary, error) {
	reg := project.NewRegistry(b.store.DB())
	configs, err := reg.List()
	if err != nil {
		return nil, err
	}
	return b.configsToSummaries(ctx, configs)
}

func (b *HPCBackend) MatchProjects(ctx context.Context, dir string) ([]ProjectSummary, error) {
	reg := project.NewRegistry(b.store.DB())
	configs, err := reg.Match(dir)
	if err != nil {
		return nil, err
	}
	return b.configsToSummaries(ctx, configs)
}

func (b *HPCBackend) configsToSummaries(ctx context.Context, configs []project.Config) ([]ProjectSummary, error) {
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
