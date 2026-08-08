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
	"github.com/gliese129/runq/internal/logfile"
	"github.com/gliese129/runq/internal/project"
	"github.com/gliese129/runq/internal/workspace"
)

// proxySubmitTimeout is the client-side timeout for the submit RPC,
// which can be slow due to preflight checks.
const proxySubmitTimeout = 50 * time.Second

// Proxy implements backend.Backend by forwarding every call to the client
// daemon's /api/v1/* surface over a unix-socket HTTP client (protocol spec
// §5). It is used by the CLI and any other out-of-process consumer.
type Proxy struct {
	client *Client
	// TargetFilter, when non-empty, scopes operations to a single target.
	// It is the CLI's resolved --target / $RUNQ_TARGET / .active-target —
	// the API layer itself is always explicit (D11), so the Proxy is where
	// the convenience layer turns into explicit paths/body fields.
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

// ── v1 wire types ───────────────────────────────────────────────────────

// APIError is a decoded v1 error envelope (spec §2). CLI code branches on
// Code only; Error is display text. Unwrap maps stable codes onto the
// backend sentinel errors so existing errors.Is call sites keep working.
type APIError struct {
	Status            int
	Code              string
	Message           string
	Details           string
	RetryAfterSeconds int
}

func (e *APIError) Error() string { return e.Message }

func (e *APIError) Unwrap() error {
	switch e.Code {
	case backend.CodeNotFound:
		return backend.ErrNotFound
	case backend.CodeNotSupported, backend.CodeNotImplemented:
		return backend.ErrNotSupported
	}
	return nil
}

// wireEnvelope mirrors the v1 list envelope (spec §2, D20).
type wireEnvelope[T any] struct {
	Items       []T    `json:"items"`
	Total       *int   `json:"total"`
	RefreshedAt *int64 `json:"refreshed_at"`
	Stale       *bool  `json:"stale"`
}

// getList fetches a v1 list endpoint and unwraps the envelope.
// (Free function: Go methods cannot have type parameters.)
func getList[T any](p *Proxy, ctx context.Context, path string) ([]T, error) {
	var env wireEnvelope[T]
	if err := p.do(ctx, "GET", path, nil, &env); err != nil {
		return nil, err
	}
	return env.Items, nil
}

// RefreshReceipt is the D22 refresh response — the wire type IS the
// backend type (one vocabulary, no translation layer; same move as
// LogPage = logfile.Page).
type RefreshReceipt = backend.RefreshReceipt

// ── System ──────────────────────────────────────────────────────────────

// Capabilities asks the daemon (config endpoint) instead of hardcoding:
// with mixed targets the truth is per-target. TargetFilter selects that
// target's caps; otherwise the default target's. Errors fall back to the
// legacy push-model defaults so display gating degrades instead of breaking.
func (p *Proxy) Capabilities() backend.Capabilities {
	var resp backend.ConfigResponse
	if err := p.do(context.Background(), "GET", "/api/v1/config", nil, &resp); err == nil {
		want := p.TargetFilter
		if want == "" {
			want = resp.DefaultTarget
		}
		for _, t := range resp.Targets {
			if t.Name == want {
				return t.Capabilities
			}
		}
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

// Health fetches the daemon's passive health summary (spec §5.1, D6).
func (p *Proxy) Health(ctx context.Context) (map[string]any, error) {
	var out map[string]any
	if err := p.do(ctx, "GET", "/api/v1/health", nil, &out); err != nil {
		return nil, err
	}
	return out, nil
}

// ── Jobs ────────────────────────────────────────────────────────────────

// RefreshJob forces a TTL-bypassing refresh (guarded by the 5min floor
// server-side). The receipt is dropped here for Backend interface
// compliance; --fresh flows use RefreshJobReceipt.
func (p *Proxy) RefreshJob(ctx context.Context, jobID string) error {
	_, err := p.RefreshJobReceipt(ctx, jobID)
	return err
}

// RefreshJobReceipt is RefreshJob keeping the D22 receipt: the CLI's
// --fresh prints「x 秒前已刷新」when Refreshed is false.
func (p *Proxy) RefreshJobReceipt(ctx context.Context, jobID string) (*RefreshReceipt, error) {
	var receipt RefreshReceipt
	if err := p.do(ctx, "POST", "/api/v1/jobs/"+url.PathEscape(jobID)+"/refresh", nil, &receipt); err != nil {
		return nil, err
	}
	return &receipt, nil
}

// RefreshTarget force-refreshes every cache of one target (spec §5.2).
func (p *Proxy) RefreshTarget(ctx context.Context, name string) (*RefreshReceipt, error) {
	var receipt RefreshReceipt
	if err := p.do(ctx, "POST", "/api/v1/targets/"+url.PathEscape(name)+"/refresh", nil, &receipt); err != nil {
		return nil, err
	}
	return &receipt, nil
}

func (p *Proxy) ListJobs(ctx context.Context, projectScope string) ([]backend.JobSummary, error) {
	q := url.Values{}
	if projectScope != "" {
		q.Set("project", projectScope)
	}
	if p.TargetFilter != "" {
		q.Set("target", p.TargetFilter)
	}
	path := "/api/v1/jobs"
	if len(q) > 0 {
		path += "?" + q.Encode()
	}
	return getList[backend.JobSummary](p, ctx, path)
}

// ListArchivedJobs — v1 merged archived into GET /jobs?archived=true (D3).
func (p *Proxy) ListArchivedJobs(ctx context.Context) ([]backend.JobSummary, error) {
	q := url.Values{"archived": {"true"}}
	if p.TargetFilter != "" {
		q.Set("target", p.TargetFilter)
	}
	return getList[backend.JobSummary](p, ctx, "/api/v1/jobs?"+q.Encode())
}

// ListTasks — GET /tasks flat table (spec §5.5, D7): `runq ps <job_id>`.
func (p *Proxy) ListTasks(ctx context.Context, opts backend.TaskListOptions) ([]backend.TaskView, int, error) {
	q := url.Values{}
	if opts.JobID != "" {
		q.Set("job", opts.JobID)
	}
	if opts.Status != "" {
		q.Set("status", opts.Status)
	}
	target := opts.Target
	if target == "" {
		target = p.TargetFilter
	}
	if target != "" {
		q.Set("target", target)
	}
	if opts.Limit > 0 {
		q.Set("limit", strconv.Itoa(opts.Limit))
		q.Set("offset", strconv.Itoa(opts.Offset))
	}
	path := "/api/v1/tasks"
	if len(q) > 0 {
		path += "?" + q.Encode()
	}
	var env wireEnvelope[backend.TaskView]
	if err := p.do(ctx, "GET", path, nil, &env); err != nil {
		return nil, 0, err
	}
	total := len(env.Items)
	if env.Total != nil {
		total = *env.Total
	}
	return env.Items, total, nil
}

func (p *Proxy) ArchiveJob(ctx context.Context, jobID string) error {
	return p.do(ctx, "POST", "/api/v1/jobs/"+url.PathEscape(jobID)+"/archive", nil, nil)
}

func (p *Proxy) UnarchiveJob(ctx context.Context, jobID string) error {
	return p.do(ctx, "POST", "/api/v1/jobs/"+url.PathEscape(jobID)+"/unarchive", nil, nil)
}

func (p *Proxy) GetJob(ctx context.Context, jobID string) (*backend.JobDetail, error) {
	var detail backend.JobDetail
	if err := p.do(ctx, "GET", "/api/v1/jobs/"+url.PathEscape(jobID), nil, &detail); err != nil {
		return nil, err
	}
	if detail.Job.ID == "" {
		return nil, fmt.Errorf("job %q: %w", jobID, backend.ErrNotFound)
	}
	return &detail, nil
}

// CompareMetrics — GET /jobs/{id}/metrics?key= (D13 dual-mode, rows shape).
// Server-side on purpose: the daemon routes to the owning target and reads
// metrics through that target's FS.
func (p *Proxy) CompareMetrics(ctx context.Context, jobID, key string, desc bool) ([]backend.CompareRow, error) {
	q := url.Values{"key": {key}}
	if desc {
		q.Set("order", "desc")
	}
	var resp struct {
		Rows []backend.CompareRow `json:"rows"`
	}
	if err := p.do(ctx, "GET", "/api/v1/jobs/"+url.PathEscape(jobID)+"/metrics?"+q.Encode(), nil, &resp); err != nil {
		return nil, err
	}
	return resp.Rows, nil
}

// MetricKeys — GET /jobs/{id}/metrics without ?key= (discovery mode).
func (p *Proxy) MetricKeys(ctx context.Context, jobID string) ([]string, error) {
	var resp struct {
		Keys []string `json:"keys"`
	}
	if err := p.do(ctx, "GET", "/api/v1/jobs/"+url.PathEscape(jobID)+"/metrics", nil, &resp); err != nil {
		return nil, err
	}
	return resp.Keys, nil
}

// ── GPU ─────────────────────────────────────────────────────────────────

// GPUStatus — GET /targets/{name}/gpus (spec §5.2/§7.2). v1 has no
// aggregated GPU endpoint: the target is explicit. TargetFilter wins;
// otherwise the daemon's default target is resolved via /config.
func (p *Proxy) GPUStatus(ctx context.Context) ([]backend.GPUSlot, error) {
	name := p.TargetFilter
	if name == "" {
		var cfg backend.ConfigResponse
		if err := p.do(ctx, "GET", "/api/v1/config", nil, &cfg); err != nil {
			return nil, err
		}
		name = cfg.DefaultTarget
	}
	if name == "" {
		return nil, fmt.Errorf("no target resolved for gpu status")
	}
	return getList[backend.GPUSlot](p, ctx, "/api/v1/targets/"+url.PathEscape(name)+"/gpus")
}

// GPUStatusByTarget is the CLI's aggregate view: fan out
// /targets/{name}/gpus across all configured targets → {target: GPUSlot[]}.
// The API itself stays per-target (D11) — aggregation is a CLIENT
// presentation concern. TargetFilter narrows to that single target. An
// unreachable target contributes an empty entry instead of failing the
// whole view.
func (p *Proxy) GPUStatusByTarget(ctx context.Context) (map[string][]backend.GPUSlot, error) {
	var names []string
	if p.TargetFilter != "" {
		names = []string{p.TargetFilter}
	} else {
		var cfg backend.ConfigResponse
		if err := p.do(ctx, "GET", "/api/v1/config", nil, &cfg); err != nil {
			return nil, err
		}
		for _, t := range cfg.Targets {
			names = append(names, t.Name)
		}
	}
	out := make(map[string][]backend.GPUSlot, len(names))
	for _, name := range names {
		gpus, err := getList[backend.GPUSlot](p, ctx, "/api/v1/targets/"+url.PathEscape(name)+"/gpus")
		if err != nil {
			out[name] = []backend.GPUSlot{} // 单 target 不可达不拖垮整视图
			continue
		}
		out[name] = gpus
	}
	return out, nil
}

// MachineGPUStatus is the PLUMBING view: THIS machine's GPUs from runqd's
// executor lane (spec §9). Used by `runq gpu --json` (the runq preset's
// gpu_template) — must not go through the v1 surface, which runqd
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
// executor lane (scancel plumbing).
func (p *Proxy) MachineKillTask(ctx context.Context, taskID string) error {
	return p.do(ctx, "POST", "/api/tasks/"+url.PathEscape(taskID)+"/kill", nil, nil)
}

// ── Tasks ───────────────────────────────────────────────────────────────

func (p *Proxy) GetTask(ctx context.Context, taskID string) (*backend.TaskView, error) {
	var view backend.TaskView
	if err := p.do(ctx, "GET", "/api/v1/tasks/"+url.PathEscape(taskID), nil, &view); err != nil {
		return nil, err
	}
	return &view, nil
}

// TaskMetrics is server-side: the daemon reads ingested metrics via the
// owning lane (SQL). afterTS > 0 → ?after= incremental pull (spec §8.1.4).
func (p *Proxy) TaskMetrics(ctx context.Context, taskID string, afterTS int64) ([]backend.MetricPoint, error) {
	path := "/api/v1/tasks/" + url.PathEscape(taskID) + "/metrics"
	if afterTS > 0 {
		path += "?after=" + strconv.FormatInt(afterTS, 10)
	}
	var resp struct {
		Points []backend.MetricPoint `json:"points"`
	}
	if err := p.do(ctx, "GET", path, nil, &resp); err != nil {
		return nil, err
	}
	return resp.Points, nil
}

// TaskMetricBuckets — GET /tasks/{id}/metrics?key=&buckets=&from=&to= →
// {buckets, source} (spec §6.4 bucket-mode chart).
func (p *Proxy) TaskMetricBuckets(ctx context.Context, taskID, key string, fromTS, toTS int64, maxBuckets int) ([]workspace.PyramidBucket, string, error) {
	q := url.Values{"key": {key}}
	if maxBuckets > 0 {
		q.Set("buckets", strconv.Itoa(maxBuckets))
	}
	if fromTS != 0 {
		q.Set("from", strconv.FormatInt(fromTS, 10))
	}
	if toTS != 0 {
		q.Set("to", strconv.FormatInt(toTS, 10))
	}
	var resp struct {
		Buckets []workspace.PyramidBucket `json:"buckets"`
		Source  string                    `json:"source"`
	}
	if err := p.do(ctx, "GET", "/api/v1/tasks/"+url.PathEscape(taskID)+"/metrics?"+q.Encode(), nil, &resp); err != nil {
		return nil, "", err
	}
	return resp.Buckets, resp.Source, nil
}

// ── Task logs (RQ-44) ───────────────────────────────────────────────────

// TaskLogRead — GET /tasks/{id}/log?offset=&lines= → LogPage.
func (p *Proxy) TaskLogRead(ctx context.Context, taskID string, offset int64, maxLines int) (*backend.LogPage, error) {
	q := url.Values{
		"offset": {strconv.FormatInt(offset, 10)},
		"lines":  {strconv.Itoa(maxLines)},
	}
	var page backend.LogPage
	if err := p.do(ctx, "GET", "/api/v1/tasks/"+url.PathEscape(taskID)+"/log?"+q.Encode(), nil, &page); err != nil {
		return nil, err
	}
	return &page, nil
}

// TaskLogTail — same endpoint WITHOUT offset (handler: absent = tail).
func (p *Proxy) TaskLogTail(ctx context.Context, taskID string, maxLines int) (*backend.LogPage, error) {
	q := url.Values{"lines": {strconv.Itoa(maxLines)}}
	var page backend.LogPage
	if err := p.do(ctx, "GET", "/api/v1/tasks/"+url.PathEscape(taskID)+"/log?"+q.Encode(), nil, &page); err != nil {
		return nil, err
	}
	return &page, nil
}

// TaskLogPage — GET /tasks/{id}/log with the v2 byte-budget params
// (max_bytes priority over lines; tail=1; count_lines=1).
func (p *Proxy) TaskLogPage(ctx context.Context, taskID string, req logfile.PageRequest) (*backend.LogPage, error) {
	q := url.Values{"max_bytes": {strconv.FormatInt(req.MaxBytes, 10)}}
	if req.Tail {
		q.Set("tail", "1")
	} else {
		q.Set("offset", strconv.FormatInt(req.Offset, 10))
	}
	if req.CountLines {
		q.Set("count_lines", "1")
	}
	var page backend.LogPage
	if err := p.do(ctx, "GET", "/api/v1/tasks/"+url.PathEscape(taskID)+"/log?"+q.Encode(), nil, &page); err != nil {
		return nil, err
	}
	return &page, nil
}

// TaskLogFollow: the CLI streams via SSE directly (logs -f consumes
// /tasks/{id}/log/stream — see cli logs command); a blocking pull
// iterator over a unix-socket HTTP hop would just be a worse SSE.
// Interface compliance only.
func (p *Proxy) TaskLogFollow(ctx context.Context, taskID string, offset int64) (backend.LogFollower, error) {
	return nil, fmt.Errorf("task log follow: use the SSE stream endpoint: %w", backend.ErrNotSupported)
}

// JobLogSearch — GET /jobs/{id}/log/search?q= (capability: log_search).
func (p *Proxy) JobLogSearch(ctx context.Context, jobID, query string) ([]backend.LogMatch, error) {
	q := url.Values{"q": {query}}
	var resp struct {
		Matches []struct {
			TaskID string `json:"task_id"`
			LineNo int    `json:"line_no"`
			Text   string `json:"text"`
		} `json:"matches"`
	}
	if err := p.do(ctx, "GET", "/api/v1/jobs/"+url.PathEscape(jobID)+"/log/search?"+q.Encode(), nil, &resp); err != nil {
		return nil, err
	}
	out := make([]backend.LogMatch, 0, len(resp.Matches))
	for _, m := range resp.Matches {
		out = append(out, backend.LogMatch{TaskID: m.TaskID, Line: m.LineNo, Text: m.Text})
	}
	return out, nil
}

// JobActivity — GET /jobs/{id}/activity (RQ2-1 §1): the daemon
// decimates on the owning side; the proxy just relays the JSON.
func (p *Proxy) JobActivity(ctx context.Context, jobID string) (*backend.JobActivity, error) {
	var out backend.JobActivity
	if err := p.do(ctx, "GET", "/api/v1/jobs/"+url.PathEscape(jobID)+"/activity", nil, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// ── Actions ─────────────────────────────────────────────────────────────

func (p *Proxy) KillTask(ctx context.Context, taskID string) error {
	return p.do(ctx, "POST", "/api/v1/tasks/"+url.PathEscape(taskID)+"/kill", nil, nil)
}

func (p *Proxy) RetryTask(ctx context.Context, taskID string) error {
	return p.do(ctx, "POST", "/api/v1/tasks/"+url.PathEscape(taskID)+"/retry", nil, nil)
}

// RetryTaskConfirm reruns a task ACKNOWLEDGING that its target config
// changed since submission (RQ-75): the server restamps it to the active
// generation and runs it under the NEW config.
func (p *Proxy) RetryTaskConfirm(ctx context.Context, taskID string) error {
	return p.do(ctx, "POST", "/api/v1/tasks/"+url.PathEscape(taskID)+"/retry?confirm_generation=true", nil, nil)
}

func (p *Proxy) KillJob(ctx context.Context, jobID string) error {
	return p.do(ctx, "POST", "/api/v1/jobs/"+url.PathEscape(jobID)+"/kill", nil, nil)
}

func (p *Proxy) PauseJob(ctx context.Context, jobID string) error {
	return p.do(ctx, "POST", "/api/v1/jobs/"+url.PathEscape(jobID)+"/pause", nil, nil)
}

func (p *Proxy) ResumeJob(ctx context.Context, jobID string) error {
	return p.do(ctx, "POST", "/api/v1/jobs/"+url.PathEscape(jobID)+"/resume", nil, nil)
}

// ── Submit family（plan → preview → submit，选项一律 body，D12）──────────

// submitWireBody matches the server's submitBody.
type submitWireBody struct {
	Config        job.JobConfig `json:"config"`
	Target        string        `json:"target,omitempty"`
	SkipPreflight bool          `json:"skip_preflight,omitempty"`
}

func (p *Proxy) SubmitJob(ctx context.Context, cfg job.JobConfig, opts backend.SubmitOptions) (string, int, error) {
	// TargetFilter is the default submit target when the caller doesn't
	// specify one — the CLI sets it once via resolveTarget and every
	// operation picks it up automatically.
	if opts.Target == "" {
		opts.Target = p.TargetFilter
	}
	var resp struct {
		JobID      string `json:"job_id"`
		TotalTasks int    `json:"total_tasks"`
	}
	body := submitWireBody{Config: cfg, Target: opts.Target, SkipPreflight: opts.SkipPreflight}
	if err := p.doWithTimeout(ctx, "POST", "/api/v1/jobs", body, &resp, proxySubmitTimeout); err != nil {
		return "", 0, err
	}
	return resp.JobID, resp.TotalTasks, nil
}

// PreviewSubmit routes through POST /jobs/preview, which delegates to the
// target backend (full run.sh + submit command rendering).
func (p *Proxy) PreviewSubmit(ctx context.Context, cfg job.JobConfig, skipPreflight bool) (backend.PreviewResult, error) {
	body := submitWireBody{Config: cfg, Target: p.TargetFilter, SkipPreflight: skipPreflight}
	var resp backend.PreviewResult
	if err := p.do(ctx, "POST", "/api/v1/jobs/preview", body, &resp); err != nil {
		return backend.PreviewResult{}, err
	}
	return resp, nil
}

// PlanJob — POST /jobs/plan (D12): cheap local expansion + note resolution
// in one call. The submit wizard and `runq sweep --dry` consume this.
func (p *Proxy) PlanJob(ctx context.Context, cfg job.JobConfig) (tasks []job.TaskParams, noteResolved string, warnings []string, err error) {
	body := submitWireBody{Config: cfg, Target: p.TargetFilter}
	var resp struct {
		Tasks        []job.TaskParams `json:"tasks"`
		NoteResolved string           `json:"note_resolved"`
		Warnings     []string         `json:"warnings"`
	}
	if err := p.do(ctx, "POST", "/api/v1/jobs/plan", body, &resp); err != nil {
		return nil, "", nil, err
	}
	return resp.Tasks, resp.NoteResolved, resp.Warnings, nil
}

func (p *Proxy) DryRun(ctx context.Context, cfg job.JobConfig) (*backend.DryRunResult, error) {
	// nil wsRootFor: the CLI-side fallback has no target filesystem; the
	// daemon's /jobs/plan (and the lane's DryRun override) own the real
	// workspace-root answer.
	return backend.BuildDryRunResult(cfg, func(name string) (*project.Config, error) {
		return p.GetProject(ctx, name)
	}, nil)
}

// ResolveNote rides /jobs/plan (the standalone endpoint is retired, D12) —
// the daemon owns the store, and the {{version}} family scan must run
// against it (same path as submit).
func (p *Proxy) ResolveNote(ctx context.Context, cfg job.JobConfig) (string, error) {
	_, note, _, err := p.PlanJob(ctx, cfg)
	return note, err
}

// ── Projects ────────────────────────────────────────────────────────────

func (p *Proxy) CreateProject(ctx context.Context, cfg project.Config) error {
	return p.do(ctx, "POST", "/api/v1/projects", cfg, nil)
}

func (p *Proxy) UpdateProject(ctx context.Context, cfg project.Config) error {
	return p.do(ctx, "PUT", "/api/v1/projects/"+url.PathEscape(cfg.ProjectName), cfg, nil)
}

func (p *Proxy) RenameProject(ctx context.Context, oldName, newName string) error {
	return p.do(ctx, "POST", "/api/v1/projects/"+url.PathEscape(oldName)+"/rename", map[string]string{
		"new_name": newName,
	}, nil)
}

func (p *Proxy) ArchiveProject(ctx context.Context, name string) error {
	return p.do(ctx, "POST", "/api/v1/projects/"+url.PathEscape(name)+"/archive", nil, nil)
}

func (p *Proxy) UnarchiveProject(ctx context.Context, name string) error {
	return p.do(ctx, "POST", "/api/v1/projects/"+url.PathEscape(name)+"/unarchive", nil, nil)
}

func (p *Proxy) GetProject(ctx context.Context, name string) (*project.Config, error) {
	var cfg project.Config
	if err := p.do(ctx, "GET", "/api/v1/projects/"+url.PathEscape(name), nil, &cfg); err != nil {
		return nil, err
	}
	return &cfg, nil
}

// ListProjects forwards the daemon's server-assembled summaries verbatim.
func (p *Proxy) ListProjects(ctx context.Context) ([]backend.ProjectSummary, error) {
	return getList[backend.ProjectSummary](p, ctx, "/api/v1/projects")
}

// MatchProjects — GET /projects?dir= (absorbed /projects/match, D3). The
// server now returns summaries directly; no client-side re-join needed.
func (p *Proxy) MatchProjects(ctx context.Context, dir string) ([]backend.ProjectSummary, error) {
	return getList[backend.ProjectSummary](p, ctx, "/api/v1/projects?dir="+url.QueryEscape(dir))
}

// ── System actions ──────────────────────────────────────────────────────

func (p *Proxy) Clean(ctx context.Context, opts backend.CleanOptions) (*backend.CleanResult, error) {
	// Pipe the target filter from CLI → daemon so clean is scoped correctly.
	if opts.Target == "" {
		opts.Target = p.TargetFilter
	}
	var result backend.CleanResult
	if err := p.do(ctx, "POST", "/api/v1/clean", opts, &result); err != nil {
		return nil, err
	}
	return &result, nil
}

// ThawTasks hits the runqd executor lane (plumbing, spec §9).
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

// ── Foreign task lane (runq preset, executor lane §9) ───────────────────

// Sbatch enqueues a foreign task on the server (runq preset). Returns the
// task id, which the client records as the task's external_id.
func (p *Proxy) Sbatch(ctx context.Context, spec backend.TaskSpec) (string, error) {
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

// ── HTTP helpers ────────────────────────────────────────────────────────

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
		apiErr := &APIError{Status: resp.StatusCode}
		var wire backend.ErrorResponse
		_ = json.NewDecoder(resp.Body).Decode(&wire)
		apiErr.Code = wire.Code
		apiErr.Message = wire.Error
		apiErr.Details = wire.Details
		apiErr.RetryAfterSeconds = wire.RetryAfterSeconds
		if apiErr.Message == "" {
			apiErr.Message = resp.Status
		}
		return apiErr
	}
	if out == nil {
		_, err = io.Copy(io.Discard, resp.Body)
		return err
	}
	return json.NewDecoder(resp.Body).Decode(out)
}
