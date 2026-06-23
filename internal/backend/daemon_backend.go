package backend

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"time"

	"github.com/gliese129/runq/internal/job"
	"github.com/gliese129/runq/internal/project"
	"github.com/gliese129/runq/internal/resource"
	"github.com/gliese129/runq/internal/store"
)

// httpDoer abstracts the daemon HTTP client so DaemonBackend does not
// depend on the api package (which imports service → would create a
// cycle once service imports backend for the view builders).
// Satisfied by *api.Client.
type httpDoer interface {
	Do(ctx context.Context, method, path string, body any) (*http.Response, error)
	DoWithTimeout(ctx context.Context, method, path string, body any, timeout time.Duration) (*http.Response, error)
}

// daemonSubmitTimeout is the client-side timeout for the submit RPC,
// which can be slow due to preflight checks.
const daemonSubmitTimeout = 50 * time.Second

type DaemonBackend struct {
	client httpDoer
}

// NewDaemonBackend creates a Backend that proxies every call to the
// daemon over a unix-socket HTTP client. Pass api.NewClient(socketPath).
func NewDaemonBackend(client httpDoer) *DaemonBackend {
	return &DaemonBackend{client: client}
}

func (b *DaemonBackend) Capabilities() Capabilities {
	return Capabilities{
		GPUMap:      true,
		PauseResume: true,
		LiveLog:     true,
		Retry:       true,
		StateModel:  "push", // the daemon owns the truth; data is always current
		KillAsync:   false,
		// Activity heatmap and log search handlers are still TODO stubs
		// (server.go handleJobActivity p6-step4, handleJobLogSearch p6-step5).
		// Keep the capabilities off until the endpoints have real behavior so
		// the UI doesn't render affordances for unimplemented features.
		ActivityHeatmap: false,
		LogSearch:       false,
	}
}

// RefreshJob: push model — there is nothing to reconcile. Defensive only;
// the GUI never renders a refresh affordance when state_model is "push".
func (b *DaemonBackend) RefreshJob(ctx context.Context, jobID string) error {
	return fmt.Errorf("refresh job in daemon mode: %w", ErrNotSupported)
}

func (b *DaemonBackend) ListJobs(ctx context.Context, projectScope string) ([]JobSummary, error) {
	path := "/api/jobs"
	if projectScope != "" {
		path += "?project=" + url.QueryEscape(projectScope)
	}
	var out []JobSummary
	if err := b.do(ctx, "GET", path, nil, &out); err != nil {
		return nil, err
	}
	return out, nil
}

func (b *DaemonBackend) ListArchivedJobs(ctx context.Context) ([]JobSummary, error) {
	var out []JobSummary
	if err := b.do(ctx, "GET", "/api/jobs?archived=1", nil, &out); err != nil {
		return nil, err
	}
	return out, nil
}

func (b *DaemonBackend) ArchiveJob(ctx context.Context, jobID string) error {
	return b.do(ctx, "POST", "/api/jobs/"+url.PathEscape(jobID)+"/archive", nil, nil)
}

func (b *DaemonBackend) UnarchiveJob(ctx context.Context, jobID string) error {
	return b.do(ctx, "POST", "/api/jobs/"+url.PathEscape(jobID)+"/unarchive", nil, nil)
}

func (b *DaemonBackend) ArchiveProject(ctx context.Context, name string) error {
	return b.do(ctx, "POST", "/api/projects/"+url.PathEscape(name)+"/archive", nil, nil)
}

func (b *DaemonBackend) UnarchiveProject(ctx context.Context, name string) error {
	return b.do(ctx, "POST", "/api/projects/"+url.PathEscape(name)+"/unarchive", nil, nil)
}

func (b *DaemonBackend) GetJob(ctx context.Context, jobID string) (*JobDetail, error) {
	// The daemon now returns a fully-built JobDetail (with TaskViews,
	// MetricKeys, Wandb, Config) — no client-side re-derivation needed.
	var detail JobDetail
	if err := b.do(ctx, "GET", "/api/jobs/"+jobID, nil, &detail); err != nil {
		return nil, err
	}
	if detail.Job.ID == "" {
		return nil, fmt.Errorf("job %q not found", jobID)
	}
	return &detail, nil
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

func (b *DaemonBackend) GetTask(ctx context.Context, taskID string) (*TaskView, string, error) {
	var task store.TaskRow
	if err := b.do(ctx, "GET", "/api/tasks/"+taskID, nil, &task); err != nil {
		return nil, "", err
	}
	view := BuildTaskView(task)
	return &view, task.LogPath, nil
}

func (b *DaemonBackend) TaskMetrics(ctx context.Context, taskID string) ([]MetricPoint, error) {
	// The daemon API has no /metrics endpoint; mirror the log path instead —
	// fetch the task row, then read metrics.jsonl from its TaskDir directly.
	// Assumes the dashboard process shares the filesystem with the daemon
	// (single-node daemon mode). Cross-node HPC uses the template backend.
	var task store.TaskRow
	if err := b.do(ctx, "GET", "/api/tasks/"+taskID, nil, &task); err != nil {
		return nil, err
	}
	return readMetricPoints(task.TaskDir), nil
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

func (b *DaemonBackend) SubmitJob(ctx context.Context, cfg job.JobConfig, opts SubmitOptions) (string, int, error) {
	var resp struct {
		JobID      string `json:"job_id"`
		TotalTasks int    `json:"total_tasks"`
	}
	if err := b.doWithTimeout(ctx, "POST", daemonSubmitPath(opts), cfg, &resp, daemonSubmitTimeout); err != nil {
		return "", 0, err
	}
	return resp.JobID, resp.TotalTasks, nil
}

func daemonSubmitPath(opts SubmitOptions) string {
	if opts.SkipPreflight {
		return "/api/jobs?no_preflight=1"
	}
	return "/api/jobs"
}

// PreviewSubmit: daemon mode has no submit_template to render — the GUI
// hides the preview block when the capability bit is off.
func (b *DaemonBackend) PreviewSubmit(_ context.Context, _ job.JobConfig, _ bool) (string, error) {
	return "", fmt.Errorf("submit preview in daemon mode: %w", ErrNotSupported)
}

func (b *DaemonBackend) DryRun(_ context.Context, cfg job.JobConfig) ([]job.TaskParams, error) {
	return job.Expand(&cfg)
}

// ResolveNote goes over the socket — the daemon owns the store, and the
// {{version}} family scan must run against it (same path as submit).
func (b *DaemonBackend) ResolveNote(ctx context.Context, cfg job.JobConfig) (string, error) {
	var resp struct {
		Resolved string `json:"resolved"`
	}
	if err := b.do(ctx, "POST", "/api/jobs/resolve-note", cfg, &resp); err != nil {
		return "", err
	}
	return resp.Resolved, nil
}

func (b *DaemonBackend) CreateProject(ctx context.Context, cfg project.Config) error {
	return b.do(ctx, "POST", "/api/projects", cfg, nil)
}

func (b *DaemonBackend) UpdateProject(ctx context.Context, cfg project.Config) error {
	return b.do(ctx, "PUT", "/api/projects/"+url.PathEscape(cfg.ProjectName), cfg, nil)
}

func (b *DaemonBackend) DeleteJob(ctx context.Context, jobID string) error {
	return b.do(ctx, "DELETE", "/api/jobs/"+url.PathEscape(jobID)+"/data", nil, nil)
}

func (b *DaemonBackend) CleanOldTasks(_ context.Context, _ time.Time, _ bool) (*CleanResult, error) {
	return nil, fmt.Errorf("clean old tasks in daemon mode: %w", ErrNotSupported)
}

func (b *DaemonBackend) DeleteProject(ctx context.Context, name string) error {
	return b.do(ctx, "DELETE", "/api/projects/"+url.PathEscape(name), nil, nil)
}

func (b *DaemonBackend) RenameProject(ctx context.Context, oldName, newName string) error {
	return b.do(ctx, "POST", "/api/projects/"+url.PathEscape(oldName)+"/rename", map[string]string{
		"new_name": newName,
	}, nil)
}

func (b *DaemonBackend) GetProject(ctx context.Context, name string) (*project.Config, error) {
	var cfg project.Config
	if err := b.do(ctx, "GET", "/api/projects/"+url.PathEscape(name), nil, &cfg); err != nil {
		return nil, err
	}
	return &cfg, nil
}

// ListProjects forwards the daemon's server-assembled summaries verbatim.
// No client-side re-derivation: assembling job_count/archived here from
// partial lists is exactly what produced the lost-flag and zero-count bugs.
func (b *DaemonBackend) ListProjects(ctx context.Context) ([]ProjectSummary, error) {
	var out []ProjectSummary
	if err := b.do(ctx, "GET", "/api/projects/summaries", nil, &out); err != nil {
		return nil, err
	}
	return out, nil
}

// MatchProjects: the daemon matches by dir; summary data comes from the
// same server-assembled source as ListProjects (no re-derivation).
func (b *DaemonBackend) MatchProjects(ctx context.Context, dir string) ([]ProjectSummary, error) {
	var configs []project.Config
	if err := b.do(ctx, "GET", "/api/projects/match?dir="+url.QueryEscape(dir), nil, &configs); err != nil {
		return nil, err
	}
	summaries, err := b.ListProjects(ctx)
	if err != nil {
		return nil, err
	}
	byName := make(map[string]ProjectSummary, len(summaries))
	for _, p := range summaries {
		byName[p.Name] = p
	}
	out := make([]ProjectSummary, 0, len(configs))
	for _, c := range configs {
		if p, ok := byName[c.ProjectName]; ok {
			out = append(out, p)
		} else {
			out = append(out, ProjectSummary{Name: c.ProjectName, WorkDir: c.WorkingDir})
		}
	}
	return out, nil
}

func (b *DaemonBackend) taskRows(ctx context.Context, jobID string) ([]store.TaskRow, error) {
	var tasks []store.TaskRow
	if err := b.do(ctx, "GET", "/api/tasks?job="+url.QueryEscape(jobID)+"&status=all", nil, &tasks); err != nil {
		return nil, err
	}
	return tasks, nil
}

func (b *DaemonBackend) do(ctx context.Context, method, path string, body any, out any) error {
	return b.doWithTimeout(ctx, method, path, body, out, 0)
}

func (b *DaemonBackend) doWithTimeout(ctx context.Context, method, path string, body any, out any, timeout time.Duration) error {
	var resp *http.Response
	var err error
	if timeout > 0 {
		resp, err = b.client.DoWithTimeout(ctx, method, path, body, timeout)
	} else {
		resp, err = b.client.Do(ctx, method, path, body)
	}
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
