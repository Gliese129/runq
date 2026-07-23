package backend

import (
	"context"
	"fmt"
	"sort"
	"sync"
	"time"

	"github.com/gliese129/runq/internal/job"
	"github.com/gliese129/runq/internal/logfile"
	"github.com/gliese129/runq/internal/project"
	"github.com/gliese129/runq/internal/rfs"
	"github.com/gliese129/runq/internal/store"
	"github.com/gliese129/runq/internal/workspace"
)

// MultiBackend routes operations to per-target backends. ListJobs aggregates
// across all targets; Submit/Kill/Get look up the target from the DB and
// delegate. Project operations are global (routed to the default target).
//
// The target map is MUTABLE at runtime (RQ-75 hot reload): the config
// reconciler adds/replaces/removes lanes as config.yaml changes. mu guards
// targets and defaultTarget; every reader goes through get/snapshot/
// defaultName so a rebuild never races an in-flight request.
type MultiBackend struct {
	mu            sync.RWMutex
	targets       map[string]Backend
	defaultTarget string
	// retiring holds superseded lane generations still tracking their
	// in-flight tasks (RQ-75): target name → generation → lane. Task-scoped
	// ops (kill/logs/refresh) route here when the task's stamped generation
	// has a live retiring lane — the OLD endpoint/templates are the only
	// ones that can correctly act on those tasks.
	retiring map[string]map[string]Backend

	store *store.Store
	// registry is the ONE routed project registry shared by every lane
	// (RQ-65); kept here so lanes added at runtime get the same wiring
	// assembly-time lanes got.
	registry *project.Registry
}

// get returns the named backend under the read lock.
func (m *MultiBackend) get(name string) (Backend, bool) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	be, ok := m.targets[name]
	return be, ok
}

// snapshot copies the current target map — iteration must never hold the
// lock across backend calls (a slow SSH lane would serialize the world).
func (m *MultiBackend) snapshot() map[string]Backend {
	m.mu.RLock()
	defer m.mu.RUnlock()
	out := make(map[string]Backend, len(m.targets))
	for k, v := range m.targets {
		out[k] = v
	}
	return out
}

// defaultName returns the routing default under the read lock.
func (m *MultiBackend) defaultName() string {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.defaultTarget
}

// SetRetiringLane registers a superseded lane generation (RQ-75).
func (m *MultiBackend) SetRetiringLane(name, generation string, be Backend) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.retiring == nil {
		m.retiring = map[string]map[string]Backend{}
	}
	if m.retiring[name] == nil {
		m.retiring[name] = map[string]Backend{}
	}
	m.retiring[name][generation] = be
}

// RemoveRetiringLane unregisters a retired lane (its count hit zero).
func (m *MultiBackend) RemoveRetiringLane(name, generation string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if g := m.retiring[name]; g != nil {
		delete(g, generation)
		if len(g) == 0 {
			delete(m.retiring, name)
		}
	}
}

// retiringLane looks up a retiring lane for (target, generation).
func (m *MultiBackend) retiringLane(name, generation string) (Backend, bool) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	be, ok := m.retiring[name][generation]
	return be, ok
}

// TargetGenerations returns the recorded generations (retiring first,
// then recently done) with live unfinished counts — the archive view.
func (m *MultiBackend) TargetGenerations(ctx context.Context) ([]TargetGenerationView, error) {
	rows, err := m.store.ListAllGenerations(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]TargetGenerationView, 0, len(rows))
	for _, g := range rows {
		v := TargetGenerationView{
			Target: g.Target, Generation: g.Generation, Reason: g.Reason,
			RetiredAt: g.RetiredAt, DoneAt: g.DoneAt,
		}
		if g.DoneAt == nil {
			if n, cerr := m.store.CountUnfinishedGenerationTasks(ctx, g.Target, g.Generation); cerr == nil {
				v.Unfinished = n
			}
		}
		out = append(out, v)
	}
	return out, nil
}

// retiringLanesOf snapshots the retiring lanes of one target.
func (m *MultiBackend) retiringLanesOf(name string) []Backend {
	m.mu.RLock()
	defer m.mu.RUnlock()
	out := make([]Backend, 0, len(m.retiring[name]))
	for _, be := range m.retiring[name] {
		out = append(out, be)
	}
	return out
}

// SetTarget adds or replaces a lane at runtime (RQ-75), wiring the shared
// project registry exactly as assembly does. The caller owns the OLD
// backend's shutdown (replace first, then close — no routing gap).
func (m *MultiBackend) SetTarget(name string, be Backend) {
	if sq, ok := be.(interface{ setProjectRegistry(*project.Registry) }); ok {
		sq.setProjectRegistry(m.registry)
	}
	m.mu.Lock()
	m.targets[name] = be
	m.mu.Unlock()
}

// RemoveTarget drops a lane from routing. The caller closes the backend
// AFTER removal so no new request can reach a closing lane.
func (m *MultiBackend) RemoveTarget(name string) {
	m.mu.Lock()
	delete(m.targets, name)
	m.mu.Unlock()
}

// SetDefaultTarget changes the routing default at runtime. Unknown names
// are rejected — a default that routes nowhere is worse than a stale one.
func (m *MultiBackend) SetDefaultTarget(name string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, ok := m.targets[name]; !ok {
		return fmt.Errorf("default target %q not in targets map", name)
	}
	m.defaultTarget = name
	return nil
}

// NewMultiBackend creates a routing backend. targets must contain at least
// the defaultTarget key.
func NewMultiBackend(targets map[string]Backend, st *store.Store, defaultTarget string) (*MultiBackend, error) {
	if _, ok := targets[defaultTarget]; !ok {
		return nil, fmt.Errorf("default target %q not in targets map", defaultTarget)
	}
	m := &MultiBackend{
		targets:       targets,
		store:         st,
		defaultTarget: defaultTarget,
	}
	// ONE routed project registry shared by every lane (RQ-65): a
	// project's yaml lives on its home target's filesystem, and only the
	// multi-backend can route across targets. Lanes embed storeQueries;
	// swapping their reg pointer gives every project code path — CRUD,
	// self-healing sync, rename — the same router.
	reg := project.NewRegistry(st.DB()).WithFSRouter(func(target string) rfs.FS {
		if target == "" {
			return nil // local machine
		}
		fsys, err := m.TargetFS(target)
		if err != nil {
			return nil // unknown target: fall back to local (fault-tolerant read path)
		}
		return fsys
	})
	m.registry = reg
	for _, be := range m.targets {
		if sq, ok := be.(interface{ setProjectRegistry(*project.Registry) }); ok {
			sq.setProjectRegistry(reg)
		}
	}
	return m, nil
}

// ── Routing helpers ────────────────────────────────────────────────────────

// resolve returns the backend for the named target. Empty falls back to default.
func (m *MultiBackend) resolve(target string) (Backend, error) {
	m.mu.RLock()
	if target == "" {
		target = m.defaultTarget
	}
	be, ok := m.targets[target]
	m.mu.RUnlock()
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

// resolveTask looks up the task's target column and returns the OWNING
// backend (RQ-75): a task stamped with a generation that has a live
// retiring lane routes there — only the old endpoint/templates can
// correctly kill/probe/read it. Everything else (active generation,
// legacy ” rows, settled generations) routes to the active lane.
func (m *MultiBackend) resolveTask(ctx context.Context, taskID string) (Backend, error) {
	t, err := m.store.GetTask(ctx, taskID)
	if err != nil {
		return nil, err
	}
	if t == nil {
		return nil, fmt.Errorf("task %q: %w", taskID, ErrNotFound)
	}
	if t.TargetGeneration != "" {
		if be, ok := m.retiringLane(t.Target, t.TargetGeneration); ok {
			return be, nil
		}
	}
	return m.resolve(t.Target)
}

// defaultBackend returns the default target's backend.
func (m *MultiBackend) defaultBackend() Backend {
	m.mu.RLock()
	defer m.mu.RUnlock()
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
	targets := m.snapshot()
	out := make(map[string]Capabilities, len(targets))
	for name, be := range targets {
		out[name] = be.Capabilities()
	}
	return out
}

// DefaultTargetName exposes the routing default for config responses.
func (m *MultiBackend) DefaultTargetName() string { return m.defaultName() }

// ReconcileAll fans the dashboard's activity-gated background reconcile out
// to every target that supports it (remote lanes). Local targets are push
// model — nothing to reconcile. This is what keeps a WATCHED dashboard
// fresher (30s cadence) than the lanes' own 25min alignment loops.
func (m *MultiBackend) ReconcileAll(ctx context.Context) error {
	var firstErr error
	for _, be := range m.snapshot() {
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
	for _, be := range m.snapshot() {
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
	for name, be := range m.snapshot() {
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

func (m *MultiBackend) TaskMetrics(ctx context.Context, taskID string, afterTS int64) ([]MetricPoint, error) {
	be, err := m.resolveTask(ctx, taskID)
	if err != nil {
		return nil, err
	}
	return be.TaskMetrics(ctx, taskID, afterTS)
}

func (m *MultiBackend) MetricKeys(ctx context.Context, jobID string) ([]string, error) {
	be, err := m.resolveJob(ctx, jobID)
	if err != nil {
		return nil, err
	}
	return be.MetricKeys(ctx, jobID)
}

func (m *MultiBackend) TaskMetricBuckets(ctx context.Context, taskID, key string, fromTS, toTS int64, maxBuckets int) ([]workspace.PyramidBucket, string, error) {
	be, err := m.resolveTask(ctx, taskID)
	if err != nil {
		return nil, "", err
	}
	return be.TaskMetricBuckets(ctx, taskID, key, fromTS, toTS, maxBuckets)
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
	be, ok := m.get(name)
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
// RecordTargetContact records a daemon-observed reachability proof for one
// target's lane (RQ-74). No-op for unknown targets and lanes without a
// contact record (local).
func (m *MultiBackend) RecordTargetContact(name string) {
	if be, ok := m.get(name); ok {
		if rc, ok := be.(interface{ RecordContactOK() }); ok {
			rc.RecordContactOK()
		}
	}
}

func (m *MultiBackend) PerTargetHealth() []TargetHealth {
	targets := m.snapshot()
	out := make([]TargetHealth, 0, len(targets))
	for name, be := range targets {
		if h, ok := be.(interface{ TargetHealth() TargetHealth }); ok {
			out = append(out, h.TargetHealth())
			continue
		}
		out = append(out, TargetHealth{Name: name, Reachable: true, LastChecked: time.Now().Unix()})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

// ── Ownership lookups (remote-CLI guard, RQ-45) ───────────────────────────
// Store-level point reads: the guard must resolve WHOSE job/task an id
// names before letting a forwarded request act on it. "" = not found.

func (m *MultiBackend) JobTarget(ctx context.Context, jobID string) (string, error) {
	j, err := m.store.GetJob(ctx, jobID)
	if err != nil || j == nil {
		return "", err
	}
	return j.Target, nil
}

func (m *MultiBackend) TaskTarget(ctx context.Context, taskID string) (string, error) {
	t, err := m.store.GetTask(ctx, taskID)
	if err != nil || t == nil {
		return "", err
	}
	return t.Target, nil
}

// ── L4: freshness routing ──────────────────────────────────────────────────

// SyncInfo aggregates lane freshness for envelope stamping. target scoping:
// named target → that lane's row; "" (aggregated list) → the OLDEST
// refreshed_at and OR of stale — a mixed response is only as fresh as its
// weakest ingredient.
func (m *MultiBackend) SyncInfo(ctx context.Context, target string) (refreshedAt int64, stale bool, known bool) {
	type syncer interface {
		SyncInfo(context.Context) (int64, bool)
	}
	if target != "" {
		be, err := m.resolve(target)
		if err != nil {
			return 0, false, false
		}
		if sy, ok := be.(syncer); ok {
			at, st := sy.SyncInfo(ctx)
			return at, st, true
		}
		return 0, false, false
	}
	first := true
	for _, be := range m.snapshot() {
		sy, ok := be.(syncer)
		if !ok {
			continue
		}
		at, st := sy.SyncInfo(ctx)
		if first || at < refreshedAt {
			refreshedAt = at
		}
		first = false
		stale = stale || st
		known = true
	}
	return refreshedAt, stale, known
}

// ForceRefreshTarget routes an explicit refresh to the named lane (D22).
func (m *MultiBackend) ForceRefreshTarget(ctx context.Context, target string) (*RefreshReceipt, error) {
	be, err := m.resolve(target)
	if err != nil {
		return nil, fmt.Errorf("refresh: %w: %v", ErrNotFound, err)
	}
	if fr, ok := be.(interface {
		ForceRefresh(context.Context) (*RefreshReceipt, error)
	}); ok {
		return fr.ForceRefresh(ctx)
	}
	return &RefreshReceipt{RefreshedAt: time.Now().Unix(), Refreshed: true}, nil
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

func (m *MultiBackend) TaskLogPage(ctx context.Context, taskID string, req logfile.PageRequest) (*LogPage, error) {
	be, err := m.resolveTask(ctx, taskID)
	if err != nil {
		return nil, err
	}
	return be.TaskLogPage(ctx, taskID, req)
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

// RetryTask reruns a terminal task on the target's ACTIVE lane (RQ-75):
// a rerun is a NEW submission, so it belongs to the new generation — never
// to the (permanently quiesced) retiring lane that owned the original run.
// An unconfirmed cross-generation rerun is refused with
// *GenerationChangedError so CLI/WebUI ask the human first.
func (m *MultiBackend) RetryTask(ctx context.Context, taskID string) error {
	return m.RetryTaskGen(ctx, taskID, false)
}

// RetryTaskGen is RetryTask with the cross-generation confirmation knob.
func (m *MultiBackend) RetryTaskGen(ctx context.Context, taskID string, confirmGeneration bool) error {
	t, err := m.store.GetTask(ctx, taskID)
	if err != nil {
		return err
	}
	if t == nil {
		return fmt.Errorf("task %q: %w", taskID, ErrNotFound)
	}
	be, err := m.resolve(t.Target)
	if err != nil {
		return err
	}
	activeGen := ""
	if g, ok := be.(interface{ Generation() string }); ok {
		activeGen = g.Generation()
	}
	if t.TargetGeneration != "" && activeGen != "" && t.TargetGeneration != activeGen && !confirmGeneration {
		return &GenerationChangedError{TaskGeneration: t.TargetGeneration, ActiveGeneration: activeGen}
	}
	// Ownership is stamped by the lane itself, inside its reset write and
	// only after the wrapper reset succeeded (review P2: a pre-retry
	// restamp would leave routing lying when the reset fails).
	return be.RetryTask(ctx, taskID)
}

func (m *MultiBackend) KillJob(ctx context.Context, jobID string) error {
	be, err := m.resolveJob(ctx, jobID)
	if err != nil {
		return err
	}
	// A job may span lane generations after a config change (in-flight
	// tasks stay with the retiring lane, pending migrated to the active
	// one) — fan the kill to the target's retiring lanes too, best-effort,
	// so old-generation tasks get killed with the templates that own them.
	j, jerr := m.store.GetJob(ctx, jobID)
	if jerr == nil && j != nil {
		for _, rbe := range m.retiringLanesOf(j.Target) {
			if rbe != be {
				_ = rbe.KillJob(ctx, jobID)
			}
		}
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
	for _, be := range m.snapshot() {
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
		for name, be := range m.snapshot() {
			if opts.Target != "" && name != opts.Target {
				continue
			}
			if det, ok := be.(orphanDetector); ok {
				_ = det.DetectOrphansNow(ctx)
			}
		}
	}
	// Run clean HERE with per-target FS routing — remote tasks' artifacts
	// live on their target's filesystem, and only the Multi knows every
	// lane. (Delegating to the default lane would delete remote dirs with
	// a local os.RemoveAll: silent no-op, artifacts left behind.)
	return PerformClean(ctx, m.store, func(target string) rfs.FS {
		fsys, err := m.TargetFS(target)
		if err != nil {
			return nil // unknown target: local semantics, worst case no-op
		}
		return fsys
	}, opts)
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
	// Fill the home target explicitly at birth (empty = wherever the user
	// is aimed by default). A project registered "against tsubame" must
	// SAY tsubame — implicit defaults rot when default_target changes.
	if cfg.Target == "" {
		cfg.Target = m.defaultName()
	}
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
