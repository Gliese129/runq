package api

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"time"

	"github.com/gliese129/runq/internal/backend"
	"github.com/gliese129/runq/internal/job"
	"github.com/gliese129/runq/internal/project"
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

// Capabilities asks the daemon (config endpoint) instead of hardcoding:
// with mixed targets the truth is per-target. TargetFilter selects that
// target's caps; otherwise the default target's. Errors fall back to the
// legacy push-model defaults so display gating degrades instead of breaking.
func (p *Proxy) Capabilities() backend.Capabilities {
	var resp backend.ConfigResponse
	if err := p.do(context.Background(), "GET", "/api/dashboard/config", nil, &resp); err == nil {
		if p.TargetFilter != "" {
			if caps, ok := resp.TargetCapabilities[p.TargetFilter]; ok {
				return caps
			}
		}
		return resp.Capabilities
	}
	return backend.Capabilities{
		GPUMap:      true,
		PauseResume: true,
		LiveLog:     true,
		Retry:       true,
		StateModel:  "push",
		KillAsync:   false,
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
	path := "/api/dashboard/jobs"
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
	q := url.Values{}
	if p.TargetFilter != "" {
		q.Set("target", p.TargetFilter)
	}
	path := "/api/dashboard/jobs/archived"
	if len(q) > 0 {
		path += "?" + q.Encode()
	}
	var out []backend.JobSummary
	if err := p.do(ctx, "GET", path, nil, &out); err != nil {
		return nil, err
	}
	return out, nil
}

func (p *Proxy) ArchiveJob(ctx context.Context, jobID string) error {
	return p.do(ctx, "POST", "/api/dashboard/jobs/"+url.PathEscape(jobID)+"/archive", nil, nil)
}

func (p *Proxy) UnarchiveJob(ctx context.Context, jobID string) error {
	return p.do(ctx, "POST", "/api/dashboard/jobs/"+url.PathEscape(jobID)+"/unarchive", nil, nil)
}

func (p *Proxy) ArchiveProject(ctx context.Context, name string) error {
	return p.do(ctx, "POST", "/api/dashboard/projects/"+url.PathEscape(name)+"/archive", nil, nil)
}

func (p *Proxy) UnarchiveProject(ctx context.Context, name string) error {
	return p.do(ctx, "POST", "/api/dashboard/projects/"+url.PathEscape(name)+"/unarchive", nil, nil)
}

func (p *Proxy) GetJob(ctx context.Context, jobID string) (*backend.JobDetail, error) {
	var detail backend.JobDetail
	if err := p.do(ctx, "GET", "/api/dashboard/jobs/"+jobID, nil, &detail); err != nil {
		return nil, err
	}
	if detail.Job.ID == "" {
		return nil, fmt.Errorf("job %q: %w", jobID, backend.ErrNotFound)
	}
	return &detail, nil
}

// CompareMetrics is server-side: the daemon routes to the owning target
// and reads metrics through that target's FS — a client-side rebuild would
// only see local files.
func (p *Proxy) CompareMetrics(ctx context.Context, jobID, key string, desc bool) ([]backend.CompareRow, error) {
	q := url.Values{"key": {key}}
	if desc {
		q.Set("order", "desc")
	}
	var rows []backend.CompareRow
	if err := p.do(ctx, "GET", "/api/dashboard/jobs/"+url.PathEscape(jobID)+"/compare?"+q.Encode(), nil, &rows); err != nil {
		return nil, err
	}
	return rows, nil
}

// GPUStatus is the aggregated (local ∪ remote) view from the client daemon.
func (p *Proxy) GPUStatus(ctx context.Context) ([]backend.GPUSlot, error) {
	var gpus []backend.GPUSlot
	if err := p.do(ctx, "GET", "/api/dashboard/gpu", nil, &gpus); err != nil {
		return nil, err
	}
	return gpus, nil
}

// MachineGPUStatus is the PLUMBING view: THIS machine's GPUs from runqd's
// legacy endpoint. Used by `runq gpu --json` (the runq preset's
// gpu_template) — must not go through the dashboard routes, which runqd
// doesn't serve.
func (p *Proxy) MachineGPUStatus(ctx context.Context) ([]backend.GPUSlot, error) {
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

// MachineKillTask cancels a task on THIS machine's executor via runqd's
// legacy endpoint (scancel plumbing).
func (p *Proxy) MachineKillTask(ctx context.Context, taskID string) error {
	return p.do(ctx, "POST", "/api/tasks/"+url.PathEscape(taskID)+"/kill", nil, nil)
}

func (p *Proxy) GetTask(ctx context.Context, taskID string) (*backend.TaskView, error) {
	var view backend.TaskView
	if err := p.do(ctx, "GET", "/api/dashboard/tasks/"+url.PathEscape(taskID), nil, &view); err != nil {
		return nil, err
	}
	return &view, nil
}

// TaskMetrics is server-side: the daemon reads metrics.jsonl through the
// owning target's FS (remote tasks included).
func (p *Proxy) TaskMetrics(ctx context.Context, taskID string) ([]backend.MetricPoint, error) {
	var points []backend.MetricPoint
	if err := p.do(ctx, "GET", "/api/dashboard/tasks/"+url.PathEscape(taskID)+"/metrics", nil, &points); err != nil {
		return nil, err
	}
	return points, nil
}

// ── RQ-44: log access（CLI 侧 — CC 的边角批，等核心落地后接线）──────────────
//
// TODO(RQ-44): TaskLogRead → GET /api/dashboard/tasks/{id}/log
//   ?offset=&lines=，响应即 LogPage JSON（与现有 handler wire 对齐）。
// TODO(RQ-44): TaskLogTail → 同端点不带 offset 参数（handler 分流：无
//   offset = tail），?lines= 照传。
// TODO(RQ-44): TaskLogFollow → CLI 不经 Proxy 流式——logs -f 直接消费 SSE
//   端点（/api/dashboard/tasks/{id}/log/stream?offset=），此方法仅为接口
//   合规，保持 ErrNotSupported。
// TODO(RQ-44): JobLogSearch → GET /api/dashboard/jobs/{id}/log/search?q=。

func (p *Proxy) TaskLogRead(ctx context.Context, taskID string, offset int64, maxLines int) (*backend.LogPage, error) {
	return nil, fmt.Errorf("task log read: %w", backend.ErrNotSupported) // TODO(RQ-44)
}

func (p *Proxy) TaskLogTail(ctx context.Context, taskID string, maxLines int) (*backend.LogPage, error) {
	return nil, fmt.Errorf("task log tail: %w", backend.ErrNotSupported) // TODO(RQ-44)
}

func (p *Proxy) TaskLogFollow(ctx context.Context, taskID string, offset int64) (backend.LogFollower, error) {
	return nil, fmt.Errorf("task log follow: %w", backend.ErrNotSupported) // TODO(RQ-44)
}

func (p *Proxy) JobLogSearch(ctx context.Context, jobID, query string) ([]backend.LogMatch, error) {
	return nil, fmt.Errorf("job log search: %w", backend.ErrNotSupported) // TODO(RQ-44)
}

func (p *Proxy) KillTask(ctx context.Context, taskID string) error {
	return p.do(ctx, "POST", "/api/dashboard/tasks/"+taskID+"/kill", nil, nil)
}

func (p *Proxy) RetryTask(ctx context.Context, taskID string) error {
	return p.do(ctx, "POST", "/api/dashboard/tasks/"+taskID+"/retry", nil, nil)
}

func (p *Proxy) KillJob(ctx context.Context, jobID string) error {
	return p.do(ctx, "POST", "/api/dashboard/jobs/"+jobID+"/kill", nil, nil)
}

func (p *Proxy) PauseJob(ctx context.Context, jobID string) error {
	return p.do(ctx, "POST", "/api/dashboard/jobs/"+jobID+"/pause", nil, nil)
}

func (p *Proxy) ResumeJob(ctx context.Context, jobID string) error {
	return p.do(ctx, "POST", "/api/dashboard/jobs/"+jobID+"/resume", nil, nil)
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
		return "/api/dashboard/jobs"
	}
	return "/api/dashboard/jobs?" + q.Encode()
}

// PreviewSubmit routes through the daemon's /api/jobs/preview endpoint,
// which delegates to the target backend. HPC targets render the full
// run.sh + submit command; local targets return ErrNotSupported.
func (p *Proxy) PreviewSubmit(ctx context.Context, cfg job.JobConfig, skipPreflight bool) (string, error) {
	q := url.Values{}
	if p.TargetFilter != "" {
		q.Set("target", p.TargetFilter)
	}
	path := "/api/dashboard/jobs/preview"
	if len(q) > 0 {
		path += "?" + q.Encode()
	}
	body := map[string]any{
		"config":         cfg,
		"skip_preflight": skipPreflight,
	}
	var resp struct {
		Preview string `json:"preview"`
	}
	if err := p.do(ctx, "POST", path, body, &resp); err != nil {
		return "", err
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
	if err := p.do(ctx, "POST", "/api/dashboard/jobs/resolve-note", cfg, &resp); err != nil {
		return "", err
	}
	return resp.Resolved, nil
}

func (p *Proxy) CreateProject(ctx context.Context, cfg project.Config) error {
	return p.do(ctx, "POST", "/api/dashboard/projects", cfg, nil)
}

func (p *Proxy) UpdateProject(ctx context.Context, cfg project.Config) error {
	return p.do(ctx, "PUT", "/api/dashboard/projects/"+url.PathEscape(cfg.ProjectName), cfg, nil)
}

func (p *Proxy) Clean(ctx context.Context, opts backend.CleanOptions) (*backend.CleanResult, error) {
	// Pipe the target filter from CLI → daemon so clean is scoped correctly.
	if opts.Target == "" {
		opts.Target = p.TargetFilter
	}
	// JSON body — CleanOptions already carries the full wire shape.
	var result backend.CleanResult
	if err := p.do(ctx, "POST", "/api/dashboard/clean", opts, &result); err != nil {
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
	return p.do(ctx, "POST", "/api/dashboard/projects/"+url.PathEscape(oldName)+"/rename", map[string]string{
		"new_name": newName,
	}, nil)
}

func (p *Proxy) GetProject(ctx context.Context, name string) (*project.Config, error) {
	var cfg project.Config
	if err := p.do(ctx, "GET", "/api/dashboard/projects/"+url.PathEscape(name), nil, &cfg); err != nil {
		return nil, err
	}
	return &cfg, nil
}

// ListProjects forwards the daemon's server-assembled summaries verbatim.
func (p *Proxy) ListProjects(ctx context.Context) ([]backend.ProjectSummary, error) {
	var out []backend.ProjectSummary
	if err := p.do(ctx, "GET", "/api/dashboard/projects", nil, &out); err != nil {
		return nil, err
	}
	return out, nil
}

func (p *Proxy) MatchProjects(ctx context.Context, dir string) ([]backend.ProjectSummary, error) {
	var configs []project.Config
	if err := p.do(ctx, "GET", "/api/dashboard/projects/match?dir="+url.QueryEscape(dir), nil, &configs); err != nil {
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

// Sbatch enqueues a foreign task on the server (runq preset). Returns the
// task id, which the client records as the task's external_id.
func (p *Proxy) Sbatch(ctx context.Context, spec backend.ForeignTaskSpec) (string, error) {
	var out struct {
		TaskID string `json:"task_id"`
	}
	if err := p.do(ctx, "POST", "/api/sbatch", spec, &out); err != nil {
		return "", err
	}
	return out.TaskID, nil
}

// Squeue lists the server's non-terminal tasks (runq preset batch probe).
func (p *Proxy) Squeue(ctx context.Context) ([]backend.QueueEntry, error) {
	var out []backend.QueueEntry
	if err := p.do(ctx, "GET", "/api/squeue", nil, &out); err != nil {
		return nil, err
	}
	return out, nil
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
