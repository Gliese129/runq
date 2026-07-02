package api

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/gliese129/runq/internal/backend"
	"github.com/gliese129/runq/internal/job"
	"github.com/gliese129/runq/internal/project"
	"github.com/gliese129/runq/internal/store"
)

// proxySubmitTimeout is the client-side timeout for the submit RPC,
// which can be slow due to preflight checks.
const proxySubmitTimeout = 50 * time.Second

// Proxy implements backend.Backend by forwarding every call to the
// daemon over a unix-socket HTTP client. It is used by the CLI and
// any other out-of-process consumer that talks to the daemon.
type Proxy struct {
	client *Client
	// TargetFilter, when non-empty, scopes list operations (ListJobs,
	// ListArchivedJobs, Clean) to a single target. Submit operations pass
	// target explicitly via SubmitOptions instead.
	TargetFilter string
}

// NewProxy creates a Backend proxy connected to the daemon at
// socketPath. Equivalent to NewProxyFromClient(NewClient(socketPath)).
func NewProxy(socketPath string) *Proxy {
	return &Proxy{client: NewClient(socketPath)}
}

// NewProxyFromClient creates a Proxy from an existing Client.
func NewProxyFromClient(c *Client) *Proxy {
	return &Proxy{client: c}
}

func (p *Proxy) Capabilities() backend.Capabilities {
	return backend.Capabilities{
		GPUMap:      true,
		PauseResume: true,
		LiveLog:     true,
		Retry:       true,
		StateModel:  "push", // the daemon owns the truth; data is always current
		KillAsync:   false,
		// Activity heatmap and log search handlers are still TODO stubs.
		// Keep the capabilities off until the endpoints have real behavior.
		ActivityHeatmap: false,
		LogSearch:       false,
	}
}

// RefreshJob: push model — there is nothing to reconcile. Defensive only;
// the GUI never renders a refresh affordance when state_model is "push".
func (p *Proxy) RefreshJob(ctx context.Context, jobID string) error {
	return fmt.Errorf("refresh job in daemon mode: %w", backend.ErrNotSupported)
}

func (p *Proxy) ListJobs(ctx context.Context, projectScope string) ([]backend.JobSummary, error) {
	q := url.Values{}
	if projectScope != "" {
		q.Set("project", projectScope)
	}
	if p.TargetFilter != "" {
		q.Set("target", p.TargetFilter)
	}
	path := "/api/jobs"
	if len(q) > 0 {
		path += "?" + q.Encode()
	}
	var out []backend.JobSummary
	if err := p.do(ctx, "GET", path, nil, &out); err != nil {
		return nil, err
	}
	return out, nil
}

func (p *Proxy) ListArchivedJobs(ctx context.Context) ([]backend.JobSummary, error) {
	q := url.Values{"archived": {"1"}}
	if p.TargetFilter != "" {
		q.Set("target", p.TargetFilter)
	}
	var out []backend.JobSummary
	if err := p.do(ctx, "GET", "/api/jobs?"+q.Encode(), nil, &out); err != nil {
		return nil, err
	}
	return out, nil
}

func (p *Proxy) ArchiveJob(ctx context.Context, jobID string) error {
	return p.do(ctx, "POST", "/api/jobs/"+url.PathEscape(jobID)+"/archive", nil, nil)
}

func (p *Proxy) UnarchiveJob(ctx context.Context, jobID string) error {
	return p.do(ctx, "POST", "/api/jobs/"+url.PathEscape(jobID)+"/unarchive", nil, nil)
}

func (p *Proxy) ArchiveProject(ctx context.Context, name string) error {
	return p.do(ctx, "POST", "/api/projects/"+url.PathEscape(name)+"/archive", nil, nil)
}

func (p *Proxy) UnarchiveProject(ctx context.Context, name string) error {
	return p.do(ctx, "POST", "/api/projects/"+url.PathEscape(name)+"/unarchive", nil, nil)
}

func (p *Proxy) GetJob(ctx context.Context, jobID string) (*backend.JobDetail, error) {
	var detail backend.JobDetail
	if err := p.do(ctx, "GET", "/api/jobs/"+jobID, nil, &detail); err != nil {
		return nil, err
	}
	if detail.Job.ID == "" {
		return nil, fmt.Errorf("job %q: %w", jobID, backend.ErrNotFound)
	}
	return &detail, nil
}

func (p *Proxy) CompareMetrics(ctx context.Context, jobID, key string, desc bool) ([]backend.CompareRow, error) {
	rows, err := p.taskRows(ctx, jobID)
	if err != nil {
		return nil, err
	}
	return backend.BuildCompareRows(rows, key, desc), nil
}

func (p *Proxy) GPUStatus(ctx context.Context) ([]backend.GPUSlot, error) {
	type enrichedGPU struct {
		Index    int    `json:"index"`
		Name     string `json:"name"`
		MemTotal int    `json:"mem_total"`
		MemFree  int    `json:"mem_free"`
		UtilPct  int    `json:"util_pct"`
		TaskID   string `json:"task_id"`
		JobID    string `json:"job_id"`
	}
	var gpus []enrichedGPU
	if err := p.do(ctx, "GET", "/api/gpu", nil, &gpus); err != nil {
		return nil, err
	}
	out := make([]backend.GPUSlot, 0, len(gpus))
	for _, gpu := range gpus {
		out = append(out, backend.GPUSlot{
			Index:       gpu.Index,
			Name:        gpu.Name,
			MemTotalMB:  gpu.MemTotal,
			MemUsedMB:   gpu.MemTotal - gpu.MemFree,
			UtilPercent: gpu.UtilPct,
			TaskID:      gpu.TaskID,
			JobID:       gpu.JobID,
		})
	}
	return out, nil
}

func (p *Proxy) GetTask(ctx context.Context, taskID string) (*backend.TaskView, error) {
	var task store.TaskRow
	if err := p.do(ctx, "GET", "/api/tasks/"+taskID, nil, &task); err != nil {
		return nil, err
	}
	view := backend.BuildTaskView(task)
	return &view, nil
}

func (p *Proxy) TaskMetrics(ctx context.Context, taskID string) ([]backend.MetricPoint, error) {
	var task store.TaskRow
	if err := p.do(ctx, "GET", "/api/tasks/"+taskID, nil, &task); err != nil {
		return nil, err
	}
	// Client-side local read: only correct for local-target tasks; remote
	// tasks are served by the daemon's Multi routing before reaching here.
	return backend.ReadMetricPoints(nil, task.TaskDir), nil
}

func (p *Proxy) KillTask(ctx context.Context, taskID string) error {
	return p.do(ctx, "POST", "/api/tasks/"+taskID+"/kill", nil, nil)
}

func (p *Proxy) RetryTask(ctx context.Context, taskID string) error {
	return p.do(ctx, "POST", "/api/tasks/"+taskID+"/retry", nil, nil)
}

func (p *Proxy) KillJob(ctx context.Context, jobID string) error {
	return p.do(ctx, "DELETE", "/api/jobs/"+jobID, nil, nil)
}

func (p *Proxy) PauseJob(ctx context.Context, jobID string) error {
	return p.do(ctx, "POST", "/api/jobs/"+jobID+"/pause", nil, nil)
}

func (p *Proxy) ResumeJob(ctx context.Context, jobID string) error {
	return p.do(ctx, "POST", "/api/jobs/"+jobID+"/resume", nil, nil)
}

func (p *Proxy) SubmitJob(ctx context.Context, cfg job.JobConfig, opts backend.SubmitOptions) (string, int, error) {
	// Use TargetFilter as the default submit target when the caller doesn't
	// specify one explicitly. This unifies the target resolution path: the
	// CLI sets TargetFilter once via resolveTarget and every operation —
	// list, submit, clean — picks it up automatically.
	if opts.Target == "" {
		opts.Target = p.TargetFilter
	}
	var resp struct {
		JobID      string `json:"job_id"`
		TotalTasks int    `json:"total_tasks"`
	}
	if err := p.doWithTimeout(ctx, "POST", proxySubmitPath(opts), cfg, &resp, proxySubmitTimeout); err != nil {
		return "", 0, err
	}
	return resp.JobID, resp.TotalTasks, nil
}

func proxySubmitPath(opts backend.SubmitOptions) string {
	q := url.Values{}
	if opts.SkipPreflight {
		q.Set("no_preflight", "1")
	}
	if opts.Target != "" {
		q.Set("target", opts.Target)
	}
	if len(q) == 0 {
		return "/api/jobs"
	}
	return "/api/jobs?" + q.Encode()
}

// PreviewSubmit routes through the daemon's /api/jobs/preview endpoint,
// which delegates to the target backend. HPC targets render the full
// run.sh + submit command; local targets return ErrNotSupported.
func (p *Proxy) PreviewSubmit(ctx context.Context, cfg job.JobConfig, skipPreflight bool) (string, error) {
	q := url.Values{}
	if p.TargetFilter != "" {
		q.Set("target", p.TargetFilter)
	}
	if skipPreflight {
		q.Set("no_preflight", "1")
	}
	var resp struct {
		Supported bool   `json:"supported"`
		Preview   string `json:"preview"`
	}
	if err := p.do(ctx, "POST", "/api/jobs/preview?"+q.Encode(), cfg, &resp); err != nil {
		return "", err
	}
	if !resp.Supported {
		return "", fmt.Errorf("submit preview: %w", backend.ErrNotSupported)
	}
	return resp.Preview, nil
}

func (p *Proxy) DryRun(ctx context.Context, cfg job.JobConfig) (*backend.DryRunResult, error) {
	return backend.BuildDryRunResult(cfg, func(name string) (*project.Config, error) {
		return p.GetProject(ctx, name)
	})
}

// ResolveNote goes over the socket — the daemon owns the store, and the
// {{version}} family scan must run against it (same path as submit).
func (p *Proxy) ResolveNote(ctx context.Context, cfg job.JobConfig) (string, error) {
	var resp struct {
		Resolved string `json:"resolved"`
	}
	if err := p.do(ctx, "POST", "/api/jobs/resolve-note", cfg, &resp); err != nil {
		return "", err
	}
	return resp.Resolved, nil
}

func (p *Proxy) CreateProject(ctx context.Context, cfg project.Config) error {
	return p.do(ctx, "POST", "/api/projects", cfg, nil)
}

func (p *Proxy) UpdateProject(ctx context.Context, cfg project.Config) error {
	return p.do(ctx, "PUT", "/api/projects/"+url.PathEscape(cfg.ProjectName), cfg, nil)
}

func (p *Proxy) Clean(ctx context.Context, opts backend.CleanOptions) (*backend.CleanResult, error) {
	q := url.Values{}
	if opts.OlderThan != nil {
		q.Set("cutoff", strconv.FormatInt(opts.OlderThan.Unix(), 10))
	}
	if opts.DryRun {
		q.Set("dry_run", "true")
	}
	if opts.Orphan {
		q.Set("orphan", "true")
	}
	if opts.Archived {
		q.Set("archived", "true")
	}
	if opts.JobID != "" {
		q.Set("job", opts.JobID)
	}
	if opts.TaskID != "" {
		q.Set("task", opts.TaskID)
	}
	if len(opts.TaskIDs) > 0 {
		// Exact-set execute (interactive selection). Comma-safe: task ids
		// contain no commas.
		q.Set("task_ids", strings.Join(opts.TaskIDs, ","))
	}
	if opts.CkptOnly {
		q.Set("ckpt_only", "true")
	}
	// Pipe target filter from CLI → daemon so clean is scoped correctly.
	if p.TargetFilter != "" {
		q.Set("target", p.TargetFilter)
	}
	var result backend.CleanResult
	if err := p.do(ctx, "POST", "/api/clean?"+q.Encode(), nil, &result); err != nil {
		return nil, err
	}
	return &result, nil
}

func (p *Proxy) ThawTasks(ctx context.Context, owner int, force bool) (*backend.ThawResponse, error) {
	q := url.Values{}
	q.Set("owner", strconv.Itoa(owner))
	if force {
		q.Set("force", "true")
	}
	var result backend.ThawResponse
	if err := p.do(ctx, "POST", "/api/thaw?"+q.Encode(), nil, &result); err != nil {
		return nil, err
	}
	return &result, nil
}

func (p *Proxy) RenameProject(ctx context.Context, oldName, newName string) error {
	return p.do(ctx, "POST", "/api/projects/"+url.PathEscape(oldName)+"/rename", map[string]string{
		"new_name": newName,
	}, nil)
}

func (p *Proxy) GetProject(ctx context.Context, name string) (*project.Config, error) {
	var cfg project.Config
	if err := p.do(ctx, "GET", "/api/projects/"+url.PathEscape(name), nil, &cfg); err != nil {
		return nil, err
	}
	return &cfg, nil
}

// ListProjects forwards the daemon's server-assembled summaries verbatim.
func (p *Proxy) ListProjects(ctx context.Context) ([]backend.ProjectSummary, error) {
	var out []backend.ProjectSummary
	if err := p.do(ctx, "GET", "/api/projects/summaries", nil, &out); err != nil {
		return nil, err
	}
	return out, nil
}

func (p *Proxy) MatchProjects(ctx context.Context, dir string) ([]backend.ProjectSummary, error) {
	var configs []project.Config
	if err := p.do(ctx, "GET", "/api/projects/match?dir="+url.QueryEscape(dir), nil, &configs); err != nil {
		return nil, err
	}
	summaries, err := p.ListProjects(ctx)
	if err != nil {
		return nil, err
	}
	byName := make(map[string]backend.ProjectSummary, len(summaries))
	for _, s := range summaries {
		byName[s.Name] = s
	}
	out := make([]backend.ProjectSummary, 0, len(configs))
	for _, c := range configs {
		if s, ok := byName[c.ProjectName]; ok {
			out = append(out, s)
		} else {
			out = append(out, backend.ProjectSummary{Name: c.ProjectName, WorkDir: c.WorkingDir})
		}
	}
	return out, nil
}

// ── HTTP helpers ────────────────────────────────────────────────────────

func (p *Proxy) taskRows(ctx context.Context, jobID string) ([]store.TaskRow, error) {
	var tasks []store.TaskRow
	if err := p.do(ctx, "GET", "/api/tasks?job="+url.QueryEscape(jobID)+"&status=all", nil, &tasks); err != nil {
		return nil, err
	}
	return tasks, nil
}

func (p *Proxy) do(ctx context.Context, method, path string, body any, out any) error {
	return p.doWithTimeout(ctx, method, path, body, out, 0)
}

func (p *Proxy) doWithTimeout(ctx context.Context, method, path string, body any, out any, timeout time.Duration) error {
	var resp *http.Response
	var err error
	if timeout > 0 {
		resp, err = p.client.DoWithTimeout(ctx, method, path, body, timeout)
	} else {
		resp, err = p.client.Do(ctx, method, path, body)
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
