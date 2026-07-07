package backend

import (
	"context"
	"fmt"
	"sort"
	"time"

	"github.com/gliese129/runq/internal/job"
	"github.com/gliese129/runq/internal/project"
	"github.com/gliese129/runq/internal/rfs"
	"github.com/gliese129/runq/internal/store"
)

// MultiBackend routes operations to per-target backends. ListJobs aggregates
// across all targets; Submit/Kill/Get look up the target from the DB and
// delegate. Project operations are global (routed to the default target).
type MultiBackend struct {
	targets       map[string]Backend
	store         *store.Store
	defaultTarget string
}

// NewMultiBackend creates a routing backend. targets must contain at least
// the defaultTarget key.
func NewMultiBackend(targets map[string]Backend, st *store.Store, defaultTarget string) (*MultiBackend, error) {
	if _, ok := targets[defaultTarget]; !ok {
		return nil, fmt.Errorf("default target %q not in targets map", defaultTarget)
	}
	return &MultiBackend{
		targets:       targets,
		store:         st,
		defaultTarget: defaultTarget,
	}, nil
}

// ── Routing helpers ────────────────────────────────────────────────────────

// resolve returns the backend for the named target. Empty falls back to default.
func (m *MultiBackend) resolve(target string) (Backend, error) {
	if target == "" {
		target = m.defaultTarget
	}
	be, ok := m.targets[target]
	if !ok {
		return nil, fmt.Errorf("unknown target %q", target)
	}
	return be, nil
}

// resolveJob looks up the job's target column and returns the owning backend.
func (m *MultiBackend) resolveJob(ctx context.Context, jobID string) (Backend, error) {
	j, err := m.store.GetJob(ctx, jobID)
	if err != nil {
		return nil, err
	}
	if j == nil {
		return nil, fmt.Errorf("job %q: %w", jobID, ErrNotFound)
	}
	return m.resolve(j.Target)
}

// resolveTask looks up the task's target column and returns the owning backend.
func (m *MultiBackend) resolveTask(ctx context.Context, taskID string) (Backend, error) {
	t, err := m.store.GetTask(ctx, taskID)
	if err != nil {
		return nil, err
	}
	if t == nil {
		return nil, fmt.Errorf("task %q: %w", taskID, ErrNotFound)
	}
	return m.resolve(t.Target)
}

// defaultBackend returns the default target's backend.
func (m *MultiBackend) defaultBackend() Backend {
	return m.targets[m.defaultTarget]
}

// ── Backend interface ──────────────────────────────────────────────────────

// Capabilities returns the default target's capabilities. Individual target
// capabilities can differ; consumers that care should query per-target.
func (m *MultiBackend) Capabilities() Capabilities {
	return m.defaultBackend().Capabilities()
}

// PerTargetCapabilities exposes each target's own capability set — the
// dashboard gates per-job UI (retry, live log, poll cadence) by the job's
// target through this map.
func (m *MultiBackend) PerTargetCapabilities() map[string]Capabilities {
	out := make(map[string]Capabilities, len(m.targets))
	for name, be := range m.targets {
		out[name] = be.Capabilities()
	}
	return out
}

// DefaultTargetName exposes the routing default for config responses.
func (m *MultiBackend) DefaultTargetName() string { return m.defaultTarget }

// ReconcileAll fans the dashboard's activity-gated background reconcile out
// to every target that supports it (remote lanes). Local targets are push
// model — nothing to reconcile. This is what keeps a WATCHED dashboard
// fresher (30s cadence) than the lanes' own 25min alignment loops.
func (m *MultiBackend) ReconcileAll(ctx context.Context) error {
	var firstErr error
	for _, be := range m.targets {
		if r, ok := be.(interface{ ReconcileAll(context.Context) error }); ok {
			if err := r.ReconcileAll(ctx); err != nil && firstErr == nil {
				firstErr = err
			}
		}
	}
	return firstErr
}

func (m *MultiBackend) RefreshJob(ctx context.Context, jobID string) error {
	be, err := m.resolveJob(ctx, jobID)
	if err != nil {
		return err
	}
	return be.RefreshJob(ctx, jobID)
}

// ListJobs aggregates jobs from all targets. Errors from individual targets
// are logged but do not fail the entire listing — partial results are better
// than none for multi-cluster dashboards.
func (m *MultiBackend) ListJobs(ctx context.Context, projectScope string) ([]JobSummary, error) {
	var all []JobSummary
	var firstErr error
	for _, be := range m.targets {
		jobs, err := be.ListJobs(ctx, projectScope)
		if err != nil {
			if firstErr == nil {
				firstErr = err
			}
			continue
		}
		all = append(all, jobs...)
	}
	if len(all) == 0 && firstErr != nil {
		return nil, firstErr
	}
	return all, nil
}

// ListJobsForTarget returns jobs from a single target backend.
func (m *MultiBackend) ListJobsForTarget(ctx context.Context, target, projectScope string) ([]JobSummary, error) {
	be, err := m.resolve(target)
	if err != nil {
		return nil, err
	}
	return be.ListJobs(ctx, projectScope)
}

// ListArchivedJobsForTarget returns archived jobs from a single target backend.
func (m *MultiBackend) ListArchivedJobsForTarget(ctx context.Context, target string) ([]JobSummary, error) {
	be, err := m.resolve(target)
	if err != nil {
		return nil, err
	}
	return be.ListArchivedJobs(ctx)
}

func (m *MultiBackend) GetJob(ctx context.Context, jobID string) (*JobDetail, error) {
	be, err := m.resolveJob(ctx, jobID)
	if err != nil {
		return nil, err
	}
	return be.GetJob(ctx, jobID)
}

func (m *MultiBackend) CompareMetrics(ctx context.Context, jobID, key string, desc bool) ([]CompareRow, error) {
	be, err := m.resolveJob(ctx, jobID)
	if err != nil {
		return nil, err
	}
	return be.CompareMetrics(ctx, jobID, key, desc)
}

// GPUStatus aggregates GPU visibility across ALL targets (local ∪ remote —
// remote targets report through their gpu_template, e.g. the runq preset's
// `runq gpu --json`; targets without one contribute nothing). Slots are
// stamped with their target name for panel grouping.
func (m *MultiBackend) GPUStatus(ctx context.Context) ([]GPUSlot, error) {
	var all []GPUSlot
	for name, be := range m.targets {
		slots, err := be.GPUStatus(ctx)
		if err != nil {
			continue
		}
		for i := range slots {
			if slots[i].Target == "" {
				slots[i].Target = name
			}
		}
		all = append(all, slots...)
	}
	return all, nil
}

func (m *MultiBackend) GetTask(ctx context.Context, taskID string) (*TaskView, error) {
	be, err := m.resolveTask(ctx, taskID)
	if err != nil {
		return nil, err
	}
	return be.GetTask(ctx, taskID)
}

func (m *MultiBackend) TaskMetrics(ctx context.Context, taskID string) ([]MetricPoint, error) {
	be, err := m.resolveTask(ctx, taskID)
	if err != nil {
		return nil, err
	}
	return be.TaskMetrics(ctx, taskID)
}

// ── RQ-44: log access — pure ownership routing ────────────────────────────

func (m *MultiBackend) TaskLogRead(ctx context.Context, taskID string, offset int64, maxLines int) (*LogPage, error) {
	be, err := m.resolveTask(ctx, taskID)
	if err != nil {
		return nil, err
	}
	return be.TaskLogRead(ctx, taskID, offset, maxLines)
}

// TargetFS resolves the addressed target's filesystem (spec §5.2 fs 组,
// #44): the fs browser and python-envs operate on the TARGET's disk, not
// the daemon's. Lanes without an FS concept fall back to LocalFS.
func (m *MultiBackend) TargetFS(name string) (rfs.FS, error) {
	be, ok := m.targets[name]
	if !ok {
		return nil, fmt.Errorf("target %q: %w", name, ErrNotFound)
	}
	if fsp, ok := be.(interface{ FS() rfs.FS }); ok {
		return fsp.FS(), nil
	}
	return rfs.NewLocalFS(), nil
}

// PerTargetHealth collects each lane's passive reachability row (/health,
// D6). Lanes without the concept (e.g. a pure-push local backend, always
// reachable by construction) report reachable with LastChecked = now.
func (m *MultiBackend) PerTargetHealth() []TargetHealth {
	out := make([]TargetHealth, 0, len(m.targets))
	for name, be := range m.targets {
		if h, ok := be.(interface{ TargetHealth() TargetHealth }); ok {
			out = append(out, h.TargetHealth())
			continue
		}
		out = append(out, TargetHealth{Name: name, Reachable: true, LastChecked: time.Now().Unix()})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

// ListTasks — flat table over the shared store (all lanes write here;
// tasks.target scopes). No routing needed.
func (m *MultiBackend) ListTasks(ctx context.Context, opts TaskListOptions) ([]TaskView, int, error) {
	return listTasksFromStore(ctx, m.store, opts)
}

func (m *MultiBackend) TaskLogTail(ctx context.Context, taskID string, maxLines int) (*LogPage, error) {
	be, err := m.resolveTask(ctx, taskID)
	if err != nil {
		return nil, err
	}
	return be.TaskLogTail(ctx, taskID, maxLines)
}

func (m *MultiBackend) TaskLogFollow(ctx context.Context, taskID string, offset int64) (LogFollower, error) {
	be, err := m.resolveTask(ctx, taskID)
	if err != nil {
		return nil, err
	}
	return be.TaskLogFollow(ctx, taskID, offset)
}

func (m *MultiBackend) JobLogSearch(ctx context.Context, jobID, query string) ([]LogMatch, error) {
	be, err := m.resolveJob(ctx, jobID)
	if err != nil {
		return nil, err
	}
	return be.JobLogSearch(ctx, jobID, query)
}

func (m *MultiBackend) KillTask(ctx context.Context, taskID string) error {
	be, err := m.resolveTask(ctx, taskID)
	if err != nil {
		return err
	}
	return be.KillTask(ctx, taskID)
}

func (m *MultiBackend) RetryTask(ctx context.Context, taskID string) error {
	be, err := m.resolveTask(ctx, taskID)
	if err != nil {
		return err
	}
	return be.RetryTask(ctx, taskID)
}

func (m *MultiBackend) KillJob(ctx context.Context, jobID string) error {
	be, err := m.resolveJob(ctx, jobID)
	if err != nil {
		return err
	}
	return be.KillJob(ctx, jobID)
}

func (m *MultiBackend) PauseJob(ctx context.Context, jobID string) error {
	be, err := m.resolveJob(ctx, jobID)
	if err != nil {
		return err
	}
	return be.PauseJob(ctx, jobID)
}

func (m *MultiBackend) ResumeJob(ctx context.Context, jobID string) error {
	be, err := m.resolveJob(ctx, jobID)
	if err != nil {
		return err
	}
	return be.ResumeJob(ctx, jobID)
}

// SubmitJob routes to the target specified in opts.Target, falling back to
// default_target.
func (m *MultiBackend) SubmitJob(ctx context.Context, cfg job.JobConfig, opts SubmitOptions) (string, int, error) {
	be, err := m.resolve(opts.Target)
	if err != nil {
		return "", 0, err
	}
	return be.SubmitJob(ctx, cfg, opts)
}

func (m *MultiBackend) DryRun(ctx context.Context, cfg job.JobConfig) (*DryRunResult, error) {
	return m.defaultBackend().DryRun(ctx, cfg)
}

func (m *MultiBackend) PreviewSubmit(ctx context.Context, cfg job.JobConfig, skipPreflight bool) (string, error) {
	return m.defaultBackend().PreviewSubmit(ctx, cfg, skipPreflight)
}

// PreviewSubmitForTarget routes submit preview to the named target backend.
// Used by the API handler to give `submit --dry-run --target <hpc>` access
// to the HPC backend's full run.sh + submit-command rendering.
func (m *MultiBackend) PreviewSubmitForTarget(ctx context.Context, target string, cfg job.JobConfig, skipPreflight bool) (string, error) {
	be, err := m.resolve(target)
	if err != nil {
		return "", err
	}
	return be.PreviewSubmit(ctx, cfg, skipPreflight)
}

// DryRunForTarget routes dry-run expansion to the named target backend.
func (m *MultiBackend) DryRunForTarget(ctx context.Context, target string, cfg job.JobConfig) (*DryRunResult, error) {
	be, err := m.resolve(target)
	if err != nil {
		return nil, err
	}
	return be.DryRun(ctx, cfg)
}

func (m *MultiBackend) ListArchivedJobs(ctx context.Context) ([]JobSummary, error) {
	var all []JobSummary
	for _, be := range m.targets {
		jobs, err := be.ListArchivedJobs(ctx)
		if err != nil {
			continue
		}
		all = append(all, jobs...)
	}
	return all, nil
}

func (m *MultiBackend) ArchiveJob(ctx context.Context, jobID string) error {
	be, err := m.resolveJob(ctx, jobID)
	if err != nil {
		return err
	}
	return be.ArchiveJob(ctx, jobID)
}

func (m *MultiBackend) UnarchiveJob(ctx context.Context, jobID string) error {
	be, err := m.resolveJob(ctx, jobID)
	if err != nil {
		return err
	}
	return be.UnarchiveJob(ctx, jobID)
}

func (m *MultiBackend) ArchiveProject(ctx context.Context, name string) error {
	return m.defaultBackend().ArchiveProject(ctx, name)
}

func (m *MultiBackend) UnarchiveProject(ctx context.Context, name string) error {
	return m.defaultBackend().UnarchiveProject(ctx, name)
}

func (m *MultiBackend) ResolveNote(ctx context.Context, cfg job.JobConfig) (string, error) {
	return m.defaultBackend().ResolveNote(ctx, cfg)
}

// orphanDetector is the optional per-target capability of scanning for
// missing task dirs THROUGH that target's own filesystem (rfs.FS) — the only
// vantage point that can tell "gone" from "unobservable".
type orphanDetector interface {
	DetectOrphansNow(ctx context.Context) error
}

func (m *MultiBackend) Clean(ctx context.Context, opts CleanOptions) (*CleanResult, error) {
	// Orphan cleaning: refresh each target's orphan marks first, each through
	// its own FS. Detection failures are non-fatal — an unobservable target
	// simply contributes no NEW marks (its guardrails also prevent false
	// ones), and must not block cleaning the others.
	if opts.Orphan {
		for name, be := range m.targets {
			if opts.Target != "" && name != opts.Target {
				continue
			}
			if det, ok := be.(orphanDetector); ok {
				_ = det.DetectOrphansNow(ctx)
			}
		}
	}
	return m.defaultBackend().Clean(ctx, opts)
}

func (m *MultiBackend) ThawTasks(ctx context.Context, owner int, force bool) (*ThawResponse, error) {
	return m.defaultBackend().ThawTasks(ctx, owner, force)
}

// ── Project operations (global, not per-target) ────────────────────────────

func (m *MultiBackend) GetProject(ctx context.Context, name string) (*project.Config, error) {
	return m.defaultBackend().GetProject(ctx, name)
}

func (m *MultiBackend) ListProjects(ctx context.Context) ([]ProjectSummary, error) {
	return m.defaultBackend().ListProjects(ctx)
}

func (m *MultiBackend) MatchProjects(ctx context.Context, dir string) ([]ProjectSummary, error) {
	return m.defaultBackend().MatchProjects(ctx, dir)
}

func (m *MultiBackend) CreateProject(ctx context.Context, cfg project.Config) error {
	return m.defaultBackend().CreateProject(ctx, cfg)
}

func (m *MultiBackend) UpdateProject(ctx context.Context, cfg project.Config) error {
	return m.defaultBackend().UpdateProject(ctx, cfg)
}

func (m *MultiBackend) RenameProject(ctx context.Context, oldName, newName string) error {
	return m.defaultBackend().RenameProject(ctx, oldName, newName)
}

// compile-time interface check
var _ Backend = (*MultiBackend)(nil)
