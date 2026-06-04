package dashboard

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/url"

	"github.com/gliese129/runq/internal/api"
	"github.com/gliese129/runq/internal/job"
	"github.com/gliese129/runq/internal/project"
	"github.com/gliese129/runq/internal/resource"
	"github.com/gliese129/runq/internal/service"
	"github.com/gliese129/runq/internal/store"
)

type DaemonBackend struct {
	client   *api.Client
	registry *project.Registry
}

func NewDaemonBackend(socketPath string, registry *project.Registry) *DaemonBackend {
	return &DaemonBackend{client: api.NewClient(socketPath), registry: registry}
}

func (b *DaemonBackend) ListJobs(ctx context.Context) ([]JobSummary, error) {
	var jobs []service.JobSummary
	if err := b.do(ctx, "GET", "/api/jobs", nil, &jobs); err != nil {
		return nil, err
	}
	out := make([]JobSummary, 0, len(jobs))
	for _, job := range jobs {
		out = append(out, summaryFromDaemonList(job))
	}
	return out, nil
}

func (b *DaemonBackend) GetJob(ctx context.Context, jobID string) (*JobDetail, error) {
	var detail struct {
		Job   *store.JobRow   `json:"job"`
		Tasks []store.TaskRow `json:"tasks"`
	}
	if err := b.do(ctx, "GET", "/api/jobs/"+jobID, nil, &detail); err != nil {
		return nil, err
	}
	if detail.Job == nil {
		return nil, fmt.Errorf("job %q not found", jobID)
	}
	view := BuildJobDetail(*detail.Job, detail.Tasks)
	if b.registry != nil {
		if cfg, err := b.registry.Get(detail.Job.ProjectName); err == nil && cfg.Wandb != nil {
			view.Wandb = &WandbInfo{
				Entity:  cfg.Wandb.Entity,
				Project: cfg.Wandb.Project,
				BaseURL: wandbBaseURL(cfg.Wandb.Entity, cfg.Wandb.Project),
			}
		}
	}
	return &view, nil
}

func (b *DaemonBackend) CompareMetrics(ctx context.Context, jobID, key string, desc bool) ([]CompareRow, error) {
	rows, err := b.taskRows(ctx, jobID)
	if err != nil {
		return nil, err
	}
	return BuildCompareRows(rows, key, desc), nil
}

func (b *DaemonBackend) GPUStatus(ctx context.Context) ([]GPUSlot, error) {
	var gpus []resource.GPUState
	if err := b.do(ctx, "GET", "/api/gpu", nil, &gpus); err != nil {
		return nil, err
	}
	out := make([]GPUSlot, 0, len(gpus))
	for _, gpu := range gpus {
		out = append(out, GPUSlot{
			Index:       gpu.Index,
			Name:        gpu.Name,
			MemTotalMB:  gpu.MemTotal,
			MemUsedMB:   gpu.MemTotal - gpu.MemFree,
			UtilPercent: gpu.UtilPct,
			TaskID:      gpu.TaskID,
			// JobID: requires task→job lookup; left empty for now.
		})
	}
	return out, nil
}

func (b *DaemonBackend) KillTask(ctx context.Context, taskID string) error {
	return b.do(ctx, "POST", "/api/tasks/"+taskID+"/kill", nil, nil)
}

func (b *DaemonBackend) RetryTask(ctx context.Context, taskID string) error {
	return b.do(ctx, "POST", "/api/tasks/"+taskID+"/retry", nil, nil)
}

func (b *DaemonBackend) KillJob(ctx context.Context, jobID string) error {
	return b.do(ctx, "DELETE", "/api/jobs/"+jobID, nil, nil)
}

func (b *DaemonBackend) PauseJob(ctx context.Context, jobID string) error {
	return b.do(ctx, "POST", "/api/jobs/"+jobID+"/pause", nil, nil)
}

func (b *DaemonBackend) ResumeJob(ctx context.Context, jobID string) error {
	return b.do(ctx, "POST", "/api/jobs/"+jobID+"/resume", nil, nil)
}

func (b *DaemonBackend) SubmitJob(ctx context.Context, cfg job.JobConfig) (string, int, error) {
	var resp struct {
		JobID      string `json:"job_id"`
		TotalTasks int    `json:"total_tasks"`
	}
	if err := b.do(ctx, "POST", "/api/jobs", cfg, &resp); err != nil {
		return "", 0, err
	}
	return resp.JobID, resp.TotalTasks, nil
}

func (b *DaemonBackend) DryRun(_ context.Context, cfg job.JobConfig) ([]job.TaskParams, error) {
	return job.Expand(&cfg)
}

func (b *DaemonBackend) CreateProject(ctx context.Context, cfg project.Config) error {
	return b.do(ctx, "POST", "/api/projects", cfg, nil)
}

func (b *DaemonBackend) GetProject(ctx context.Context, name string) (*project.Config, error) {
	var cfg project.Config
	if err := b.do(ctx, "GET", "/api/projects/"+url.PathEscape(name), nil, &cfg); err != nil {
		return nil, err
	}
	return &cfg, nil
}

func (b *DaemonBackend) ListProjects(ctx context.Context) ([]ProjectSummary, error) {
	// Fetch projects from daemon API (registry lives in daemon process)
	var configs []project.Config
	if err := b.do(ctx, "GET", "/api/projects", nil, &configs); err != nil {
		return nil, err
	}
	// Get job counts
	var jobs []service.JobSummary
	_ = b.do(ctx, "GET", "/api/jobs", nil, &jobs)
	jobCounts := make(map[string]int)
	for _, j := range jobs {
		jobCounts[j.Project]++
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

func (b *DaemonBackend) MatchProjects(ctx context.Context, dir string) ([]ProjectSummary, error) {
	var configs []project.Config
	if err := b.do(ctx, "GET", "/api/projects/match?dir="+url.QueryEscape(dir), nil, &configs); err != nil {
		return nil, err
	}
	// Get job counts
	var jobs []service.JobSummary
	_ = b.do(ctx, "GET", "/api/jobs", nil, &jobs)
	jobCounts := make(map[string]int)
	for _, j := range jobs {
		jobCounts[j.Project]++
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

func (b *DaemonBackend) taskRows(ctx context.Context, jobID string) ([]store.TaskRow, error) {
	var detail struct {
		Job   *store.JobRow   `json:"job"`
		Tasks []store.TaskRow `json:"tasks"`
	}
	if err := b.do(ctx, "GET", "/api/jobs/"+jobID, nil, &detail); err != nil {
		return nil, err
	}
	if detail.Job == nil {
		return nil, fmt.Errorf("job %q not found", jobID)
	}
	return detail.Tasks, nil
}

func (b *DaemonBackend) do(ctx context.Context, method, path string, body any, out any) error {
	_ = ctx
	resp, err := b.client.Do(method, path, body)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 400 {
		var errResp struct {
			Error string `json:"error"`
		}
		_ = json.NewDecoder(resp.Body).Decode(&errResp)
		if errResp.Error == "" {
			errResp.Error = resp.Status
		}
		return fmt.Errorf("%s", errResp.Error)
	}
	if out == nil {
		_, err = io.Copy(io.Discard, resp.Body)
		return err
	}
	return json.NewDecoder(resp.Body).Decode(out)
}

func summaryFromDaemonList(job service.JobSummary) JobSummary {
	counts := TaskCountGroup{
		Total:     job.TotalTasks,
		Pending:   job.StatusCount["pending"],
		Running:   job.StatusCount["running"],
		Completed: job.StatusCount["success"],
		Failed:    job.StatusCount["failed"] + job.StatusCount["killed"],
	}
	return JobSummary{
		ID:        job.JobID,
		Project:   job.Project,
		Status:    job.Status,
		CreatedAt: job.CreatedAt.Unix(),
		Tasks:     counts,
	}
}
