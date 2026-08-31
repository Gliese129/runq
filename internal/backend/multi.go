package backend

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/gliese129/runq-lab/internal/job"
	"github.com/gliese129/runq-lab/internal/logfile"
	"github.com/gliese129/runq-lab/internal/project"
	"github.com/gliese129/runq-lab/internal/rfs"
	"github.com/gliese129/runq-lab/internal/store"
	"github.com/gliese129/runq-lab/internal/workspace"
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
	// routingMu is the generation-transition barrier. User operations hold a
	// read lease from resolution through the backend effect; the config
	// reconciler and retirement sweep hold the write lease while changing
	// scopes/registry membership. This makes each operation linearizable to a
	// single lane generation without holding the registry map mutex over SSH.
	routingMu sync.RWMutex
	// controlMu serializes user control transitions that can move ownership or
	// terminalize work (retry/kill/pause/resume) across lane generations.
	controlMu     sync.Mutex
	mu            sync.RWMutex
	targets       map[string]Backend
	defaultTarget string
	// retiring holds superseded lane generations still tracking their
	// in-flight tasks (RQ-75): target name → generation → lane. Task-scoped
	// ops (kill/logs/refresh) route here when the task's stamped generation
	// has a live retiring lane — the OLD endpoint/templates are the only
	// ones that can correctly act on those tasks.
	retiring map[string]map[string]Backend
	// historical holds quiesced, generation-scoped lanes for terminal
	// artifact reads. They do not participate in refresh or control fanout,
	// but preserve the original host/workspace after retirement completes.
	historical map[string]map[string]Backend

	store *store.Store
	// registry is the ONE routed project registry shared by every lane
	// (RQ-65); kept here so lanes added at runtime get the same wiring
	// assembly-time lanes got.
	registry *project.Registry
}

// BeginRoutingUpdate excludes routed operations while the reconciler changes
// lane scopes and registry membership. The returned release function must be
// deferred by the caller. Cold lane construction should happen before taking
// this lease when practical; correctness takes precedence when Start itself
// participates in the ownership handoff.
func (m *MultiBackend) BeginRoutingUpdate() func() {
	m.routingMu.Lock()
	return m.routingMu.Unlock
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

// liveLanes snapshots active and retiring lanes, optionally for one target.
// Backend calls must happen after this returns; m.mu only protects registry
// membership and must never be held across SSH work.
func (m *MultiBackend) liveLanes(target string) []Backend {
	m.mu.RLock()
	defer m.mu.RUnlock()
	names := make([]string, 0, len(m.targets)+len(m.retiring))
	seenNames := make(map[string]struct{}, len(m.targets)+len(m.retiring))
	for name := range m.targets {
		if target == "" || name == target {
			seenNames[name] = struct{}{}
		}
	}
	for name := range m.retiring {
		if target == "" || name == target {
			seenNames[name] = struct{}{}
		}
	}
	for name := range seenNames {
		names = append(names, name)
	}
	sort.Strings(names)

	var out []Backend
	for _, name := range names {
		if be := m.targets[name]; be != nil {
			out = append(out, be)
		}
		generations := make([]string, 0, len(m.retiring[name]))
		for generation := range m.retiring[name] {
			generations = append(generations, generation)
		}
		sort.Strings(generations)
		for _, generation := range generations {
			be := m.retiring[name][generation]
			if be != nil && (len(out) == 0 || be != out[len(out)-1]) {
				out = append(out, be)
			}
		}
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

// RotateLane atomically installs the replacement as active AND registers
// the superseded lane as retiring (round 8 #2): a concurrent resolveTask
// can never observe a state where the old generation is neither active
// nor retiring.
func (m *MultiBackend) RotateLane(name string, newBe Backend, oldGen string, oldBe Backend) {
	if sq, ok := newBe.(interface{ setProjectRegistry(*project.Registry) }); ok {
		sq.setProjectRegistry(m.registry)
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	m.targets[name] = newBe
	if oldGen != "" && oldBe != nil {
		if m.retiring == nil {
			m.retiring = map[string]map[string]Backend{}
		}
		if m.retiring[name] == nil {
			m.retiring[name] = map[string]Backend{}
		}
		m.retiring[name][oldGen] = oldBe
	}
}

// PromoteLane atomically moves a retiring lane back to active AND, when a
// lane is being superseded by the promotion (A→B→A: B steps down as A
// steps up), registers it as retiring in the SAME transaction (round 9
// #2) — a concurrent reader never sees either generation unrouted.
func (m *MultiBackend) PromoteLane(name, gen string, be Backend, supersededGen string, supersededBe Backend) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if g := m.retiring[name]; g != nil {
		delete(g, gen)
		if len(g) == 0 {
			delete(m.retiring, name)
		}
	}
	m.targets[name] = be
	if supersededGen != "" && supersededBe != nil {
		if m.retiring == nil {
			m.retiring = map[string]map[string]Backend{}
		}
		if m.retiring[name] == nil {
			m.retiring[name] = map[string]Backend{}
		}
		m.retiring[name][supersededGen] = supersededBe
	}
}

// RetireTarget atomically unroutes a REMOVED target's active lane and
// registers it as retiring (round 9 #1) — task routing by generation
// keeps working through the whole transition, no not-found window.
func (m *MultiBackend) RetireTarget(name, gen string, be Backend) {
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.targets, name)
	if gen != "" && be != nil {
		if m.retiring == nil {
			m.retiring = map[string]map[string]Backend{}
		}
		if m.retiring[name] == nil {
			m.retiring[name] = map[string]Backend{}
		}
		m.retiring[name][gen] = be
	}
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

// CompleteRetiringLane atomically moves a settled generation out of live
// routing and into artifact-only routing.
func (m *MultiBackend) CompleteRetiringLane(name, generation string, be Backend) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if lanes := m.retiring[name]; lanes != nil {
		delete(lanes, generation)
		if len(lanes) == 0 {
			delete(m.retiring, name)
		}
	}
	if m.historical == nil {
		m.historical = map[string]map[string]Backend{}
	}
	if m.historical[name] == nil {
		m.historical[name] = map[string]Backend{}
	}
	m.historical[name][generation] = be
}

// SetHistoricalLane restores an artifact-only lane from a persisted target
// generation snapshot during daemon startup.
func (m *MultiBackend) SetHistoricalLane(name, generation string, be Backend) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.historical == nil {
		m.historical = map[string]map[string]Backend{}
	}
	if m.historical[name] == nil {
		m.historical[name] = map[string]Backend{}
	}
	m.historical[name][generation] = be
}

// RemoveHistoricalLane forgets an artifact-only lane, returning it so the
// daemon can release its filesystem. Used when that exact generation becomes
// active again and the active lane is once more the correct endpoint.
func (m *MultiBackend) RemoveHistoricalLane(name, generation string) Backend {
	m.mu.Lock()
	defer m.mu.Unlock()
	lanes := m.historical[name]
	be := lanes[generation]
	delete(lanes, generation)
	if len(lanes) == 0 {
		delete(m.historical, name)
	}
	return be
}

// retiringLane looks up a retiring lane for (target, generation).
func (m *MultiBackend) retiringLane(name, generation string) (Backend, bool) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	be, ok := m.retiring[name][generation]
	return be, ok
}

func (m *MultiBackend) historicalLane(name, generation string) (Backend, bool) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	be, ok := m.historical[name][generation]
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

// ActiveLane exposes the CURRENT active lane of a target (RQ-75
// forwarding): retiring lanes resolve their successor per handoff, so
// chained rotations always reach the newest generation.
func (m *MultiBackend) ActiveLane(name string) (Backend, bool) {
	return m.get(name)
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
	if name == "" {
		if len(m.targets) != 0 {
			return fmt.Errorf("cannot clear default target while %d target lane(s) are active", len(m.targets))
		}
		m.defaultTarget = ""
		return nil
	}
	if _, ok := m.targets[name]; !ok {
		return fmt.Errorf("default target %q not in targets map", name)
	}
	m.defaultTarget = name
	return nil
}

// NewMultiBackend creates a routing backend. The exact (empty targets,
// empty default) pair is the supported unconfigured state; otherwise targets
// must contain the defaultTarget key.
func NewMultiBackend(targets map[string]Backend, st *store.Store, defaultTarget string) (*MultiBackend, error) {
	if len(targets) == 0 && defaultTarget != "" {
		return nil, fmt.Errorf("default target %q set but no targets are configured", defaultTarget)
	}
	if len(targets) > 0 {
		if _, ok := targets[defaultTarget]; !ok {
			return nil, fmt.Errorf("default target %q not in targets map", defaultTarget)
		}
	}
	m := &MultiBackend{
		targets:       targets,
		retiring:      map[string]map[string]Backend{},
		historical:    map[string]map[string]Backend{},
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
	if target == "" && len(m.targets) == 0 {
		m.mu.RUnlock()
		return nil, noTargetConfiguredError()
	}
	be, ok := m.targets[target]
	m.mu.RUnlock()
	if !ok {
		return nil, fmt.Errorf("unknown target %q", target)
	}
	return be, nil
}

// resolveTask looks up the task's target column and returns the OWNING
// backend (RQ-75): a task stamped with a generation that has a live
// retiring lane routes there — only the old endpoint/templates can
// correctly kill/probe/read it. Settled generations use a quiesced historical
// lane, never the current endpoint. Legacy rows use the active lane.
func (m *MultiBackend) resolveTask(ctx context.Context, taskID string) (Backend, error) {
	t, err := m.store.GetTask(ctx, taskID)
	if err != nil {
		return nil, err
	}
	if t == nil {
		return nil, fmt.Errorf("task %q: %w", taskID, ErrNotFound)
	}
	return m.resolveTaskRow(t)
}

func (m *MultiBackend) resolveTaskRow(t *store.TaskRow) (Backend, error) {
	if t.TargetGeneration != "" {
		if be, ok := m.retiringLane(t.Target, t.TargetGeneration); ok {
			return be, nil
		}
		if active, ok := m.get(t.Target); ok {
			if generation, hasGeneration := active.(interface{ Generation() string }); !hasGeneration || generation.Generation() == t.TargetGeneration {
				return active, nil
			}
		}
		if be, ok := m.historicalLane(t.Target, t.TargetGeneration); ok {
			return be, nil
		}
		return nil, fmt.Errorf("task %q belongs to unavailable target generation %q", t.ID, t.TargetGeneration)
	}
	return m.resolve(t.Target)
}

type artifactTaskGroup struct {
	fs    rfs.FS
	tasks []store.TaskRow
}

// jobArtifactGroups partitions a job by its durable target generation and
// resolves each partition to that generation's exact filesystem. Group order
// follows the first task occurrence so responses stay deterministic.
func (m *MultiBackend) jobArtifactGroups(ctx context.Context, jobID string) (*store.JobRow, []store.TaskRow, []artifactTaskGroup, error) {
	j, err := m.store.GetJob(ctx, jobID)
	if err != nil {
		return nil, nil, nil, err
	}
	if j == nil {
		return nil, nil, nil, fmt.Errorf("job %q: %w", jobID, ErrNotFound)
	}
	tasks, err := m.store.ListTasks(ctx, store.TaskFilter{JobID: jobID})
	if err != nil {
		return nil, nil, nil, err
	}
	groupIndex := make(map[string]int)
	groups := make([]artifactTaskGroup, 0)
	for i := range tasks {
		be, err := m.resolveTaskRow(&tasks[i])
		if err != nil {
			return nil, nil, nil, err
		}
		fsys := rfs.FS(rfs.NewLocalFS())
		if fsp, ok := be.(interface{ FS() rfs.FS }); ok {
			fsys = fsp.FS()
		}
		if active, ok := be.(interface{ touchActivity() }); ok {
			active.touchActivity()
		}
		key := tasks[i].Target + "\x00" + tasks[i].TargetGeneration
		idx, ok := groupIndex[key]
		if !ok {
			idx = len(groups)
			groupIndex[key] = idx
			groups = append(groups, artifactTaskGroup{fs: fsys})
		}
		groups[idx].tasks = append(groups[idx].tasks, tasks[i])
	}
	return j, tasks, groups, nil
}

// refreshJobForStoreRead refreshes every live generation only while the job
// can still change externally. Terminal history is already durable and must
// remain readable even after its target has no active/retiring lane.
func (m *MultiBackend) refreshJobForStoreRead(ctx context.Context, jobID string) (*store.JobRow, error) {
	j, err := m.store.GetJob(ctx, jobID)
	if err != nil {
		return nil, err
	}
	if j == nil {
		return nil, fmt.Errorf("job %q: %w", jobID, ErrNotFound)
	}
	if !store.IsTerminalJobStatus(j.Status) {
		if _, err := m.refreshJobGenerations(ctx, jobID); err != nil {
			return nil, err
		}
		j, err = m.store.GetJob(ctx, jobID)
		if err != nil {
			return nil, err
		}
		if j == nil {
			return nil, fmt.Errorf("job %q: %w", jobID, ErrNotFound)
		}
	}
	return j, nil
}

// jobOwningLanes resolves the live lane generations that actually own tasks
// for a job. Generation stamps are authoritative: a live retiring generation
// owns its exact rows; active owns its own, legacy, and orphan-generation rows.
// primary is the active lane when present (the renderer for whole-job views),
// otherwise the first live owner of a removed target.
func (m *MultiBackend) jobOwningLanes(ctx context.Context, jobID string) (primary Backend, owners []Backend, err error) {
	j, err := m.store.GetJob(ctx, jobID)
	if err != nil {
		return nil, nil, err
	}
	if j == nil {
		return nil, nil, fmt.Errorf("job %q: %w", jobID, ErrNotFound)
	}
	tasks, err := m.store.ListTasks(ctx, store.TaskFilter{JobID: jobID})
	if err != nil {
		return nil, nil, fmt.Errorf("list job %s generations: %w", jobID, err)
	}

	m.mu.RLock()
	active := m.targets[j.Target]
	retiring := make(map[string]Backend, len(m.retiring[j.Target]))
	for generation, be := range m.retiring[j.Target] {
		retiring[generation] = be
	}
	m.mu.RUnlock()

	activeOwns := false
	ownedRetiring := make(map[string]struct{})
	for i := range tasks {
		if _, ok := retiring[tasks[i].TargetGeneration]; ok && tasks[i].TargetGeneration != "" {
			ownedRetiring[tasks[i].TargetGeneration] = struct{}{}
			continue
		}
		if active != nil {
			activeOwns = true
		}
	}
	if activeOwns {
		owners = append(owners, active)
	}
	generations := make([]string, 0, len(ownedRetiring))
	for generation := range ownedRetiring {
		generations = append(generations, generation)
	}
	sort.Strings(generations)
	for _, generation := range generations {
		be := retiring[generation]
		if be != nil && be != active {
			owners = append(owners, be)
		}
	}
	if len(owners) == 0 && active != nil {
		// Preserve the zero-task job behavior without letting an empty active
		// scope mask a real retiring owner (which would already be in owners).
		owners = append(owners, active)
	}
	primary = active
	if primary == nil && len(owners) > 0 {
		primary = owners[0]
	}
	if primary == nil {
		return nil, nil, fmt.Errorf("unknown target %q", j.Target)
	}
	return primary, owners, nil
}

// jobControlLanes includes every current owner plus the active lane even when
// its scope is empty. The active generation owns any future manual retry, so a
// paused job must already be gated there before ownership can move to it.
func (m *MultiBackend) jobControlLanes(ctx context.Context, jobID string) ([]Backend, error) {
	primary, owners, err := m.jobOwningLanes(ctx, jobID)
	if err != nil {
		return nil, err
	}
	if len(owners) == 0 || owners[0] != primary {
		owners = append([]Backend{primary}, owners...)
	}
	return owners, nil
}

// refreshJobGenerations reconciles every live lane generation that owns at
// least one task for the job. Errors are joined so an empty/successful active
// scope can never hide a retiring generation's failure.
func (m *MultiBackend) refreshJobGenerations(ctx context.Context, jobID string) (Backend, error) {
	primary, owners, err := m.jobOwningLanes(ctx, jobID)
	if err != nil {
		return nil, err
	}
	var refreshErrs []error
	for _, be := range owners {
		if err := be.RefreshJob(ctx, jobID); err != nil {
			refreshErrs = append(refreshErrs, err)
		}
	}
	return primary, errors.Join(refreshErrs...)
}

// refreshListedJobGenerations upgrades the SSH lane's active-only list refresh
// to generation-complete reconciliation. Lists remain best-effort by contract:
// failures surface through SyncInfo/stale while available rows still render.
func (m *MultiBackend) refreshListedJobGenerations(ctx context.Context, jobs []JobSummary) bool {
	refreshed := false
	type laneOutcome struct {
		be  Backend
		err []error
	}
	var outcomes []laneOutcome
	findOutcome := func(be Backend) *laneOutcome {
		for i := range outcomes {
			if outcomes[i].be == be {
				return &outcomes[i]
			}
		}
		outcomes = append(outcomes, laneOutcome{be: be})
		return &outcomes[len(outcomes)-1]
	}
	for i := range jobs {
		if store.IsTerminalJobStatus(jobs[i].Status) {
			continue
		}
		refreshed = true
		_, owners, err := m.jobOwningLanes(ctx, jobs[i].ID)
		if err != nil {
			continue
		}
		for _, be := range owners {
			outcome := findOutcome(be)
			if err := be.RefreshJob(ctx, jobs[i].ID); err != nil {
				outcome.err = append(outcome.err, fmt.Errorf("refresh job %s: %w", jobs[i].ID, err))
			}
		}
	}
	// RefreshJob records each observation immediately for point reads. A list
	// observes several jobs, so publish one final per-lane aggregate after all
	// of them: a later success must not erase an earlier failure in the same
	// returned snapshot.
	for i := range outcomes {
		if recorder, ok := outcomes[i].be.(interface{ recordTasksSync(error) }); ok {
			recorder.recordTasksSync(errors.Join(outcomes[i].err...))
		}
	}
	return refreshed
}

// defaultBackend returns the default target's backend. During a config
// reconcile the default can be momentarily unrouted: the removed loop
// unroutes the old default BEFORE the added loop builds (and
// SetDefaultTarget installs) its replacement, and an SSH lane build
// keeps that window open for seconds. Readers must degrade, never
// panic (Codex RQ2-4 F3): fall back to any live lane (deterministic:
// smallest name); nil only when no lane exists at all — callers below
// turn that into an error or a zero value.
func (m *MultiBackend) defaultBackend() Backend {
	m.mu.RLock()
	defer m.mu.RUnlock()
	if be, ok := m.targets[m.defaultTarget]; ok {
		return be
	}
	fallback := ""
	for name := range m.targets {
		if fallback == "" || name < fallback {
			fallback = name
		}
	}
	if fallback == "" {
		return nil
	}
	return m.targets[fallback]
}

// defaultBackendE is defaultBackend for error-returning paths: the
// reconcile gap becomes a retryable error instead of a nil-call panic.
func (m *MultiBackend) defaultBackendE() (Backend, error) {
	if be := m.defaultBackend(); be != nil {
		return be, nil
	}
	m.mu.RLock()
	unconfigured := len(m.targets) == 0 && m.defaultTarget == ""
	m.mu.RUnlock()
	if unconfigured {
		return nil, noTargetConfiguredError()
	}
	return nil, fmt.Errorf("no target lane available (config reconcile in progress) — retry")
}

func noTargetConfiguredError() error {
	return fmt.Errorf("%w; add one with `runq target add <name> ...`", ErrNoTargetConfigured)
}

// ── Backend interface ──────────────────────────────────────────────────────

// Capabilities returns the default target's capabilities. Individual target
// capabilities can differ; consumers that care should query per-target.
// In the reconcile gap (no lane routable) it reports zero capabilities —
// a briefly degraded UI beats a 500 for a config that DID apply.
func (m *MultiBackend) Capabilities() Capabilities {
	m.routingMu.RLock()
	defer m.routingMu.RUnlock()
	if be := m.defaultBackend(); be != nil {
		return be.Capabilities()
	}
	return Capabilities{}
}

// PerTargetCapabilities exposes each target's own capability set — the
// dashboard gates per-job UI (retry, live log, poll cadence) by the job's
// target through this map.
func (m *MultiBackend) PerTargetCapabilities() map[string]Capabilities {
	m.routingMu.RLock()
	defer m.routingMu.RUnlock()
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
	m.routingMu.RLock()
	defer m.routingMu.RUnlock()
	var firstErr error
	for _, be := range m.liveLanes("") {
		if r, ok := be.(interface{ ReconcileAll(context.Context) error }); ok {
			if err := r.ReconcileAll(ctx); err != nil && firstErr == nil {
				firstErr = err
			}
		}
	}
	return firstErr
}

func (m *MultiBackend) RefreshJob(ctx context.Context, jobID string) error {
	m.routingMu.RLock()
	defer m.routingMu.RUnlock()
	_, err := m.refreshJobGenerations(ctx, jobID)
	return err
}

func (m *MultiBackend) renderJobRows(ctx context.Context, jobs []store.JobRow) ([]JobSummary, error) {
	if len(jobs) == 0 {
		return []JobSummary{}, nil
	}
	jobIDs := make([]string, len(jobs))
	for i := range jobs {
		jobIDs[i] = jobs[i].ID
	}
	tasks, err := m.store.ListTasksForJobs(ctx, jobIDs)
	if err != nil {
		return nil, err
	}
	byJob := make(map[string][]store.TaskRow, len(jobs))
	for i := range tasks {
		byJob[tasks[i].JobID] = append(byJob[tasks[i].JobID], tasks[i])
	}
	out := make([]JobSummary, 0, len(jobs))
	for i := range jobs {
		out = append(out, BuildJobSummary(jobs[i], byJob[jobs[i].ID]))
	}
	return out, nil
}

// ListJobs renders the durable store, not the current lane registry. A target
// may be removed and every one of its generations may be settled, but its job
// history remains user data and must stay visible. Live non-terminal owners
// are reconciled before the final store read.
func (m *MultiBackend) ListJobs(ctx context.Context, projectScope string) ([]JobSummary, error) {
	m.routingMu.RLock()
	defer m.routingMu.RUnlock()
	rows, err := m.store.ListJobsVisible(ctx, projectScope, "")
	if err != nil {
		return nil, err
	}
	jobs, err := m.renderJobRows(ctx, rows)
	if err != nil {
		return nil, err
	}
	if m.refreshListedJobGenerations(ctx, jobs) {
		rows, err = m.store.ListJobsVisible(ctx, projectScope, "")
		if err != nil {
			return nil, err
		}
		return m.renderJobRows(ctx, rows)
	}
	return jobs, nil
}

// ListJobsForTarget has the same durable-history contract as ListJobs. An
// absent active lane is not an absent target history.
func (m *MultiBackend) ListJobsForTarget(ctx context.Context, target, projectScope string) ([]JobSummary, error) {
	m.routingMu.RLock()
	defer m.routingMu.RUnlock()
	rows, err := m.store.ListJobsVisible(ctx, projectScope, target)
	if err != nil {
		return nil, err
	}
	jobs, err := m.renderJobRows(ctx, rows)
	if err != nil {
		return nil, err
	}
	if m.refreshListedJobGenerations(ctx, jobs) {
		rows, err = m.store.ListJobsVisible(ctx, projectScope, target)
		if err != nil {
			return nil, err
		}
		return m.renderJobRows(ctx, rows)
	}
	return jobs, nil
}

// ListArchivedJobsForTarget also survives target removal.
func (m *MultiBackend) ListArchivedJobsForTarget(ctx context.Context, target string) ([]JobSummary, error) {
	rows, err := m.store.ListJobsArchived(ctx, "", target)
	if err != nil {
		return nil, err
	}
	return m.renderJobRows(ctx, rows)
}

func (m *MultiBackend) GetJob(ctx context.Context, jobID string) (*JobDetail, error) {
	m.routingMu.RLock()
	defer m.routingMu.RUnlock()
	j, err := m.refreshJobForStoreRead(ctx, jobID)
	if err != nil {
		return nil, err
	}
	tasks, err := m.store.ListTasks(ctx, store.TaskFilter{JobID: jobID})
	if err != nil {
		return nil, err
	}
	keys, _ := m.store.MetricKeys(ctx, jobID)
	detail := BuildJobDetail(*j, tasks, keys)
	if cfg, err := m.registry.Get(ctx, j.ProjectName); err == nil && cfg.Wandb != nil {
		detail.Wandb = &WandbInfo{
			Entity:  cfg.Wandb.Entity,
			Project: cfg.Wandb.Project,
			BaseURL: WandbBaseURL(cfg.Wandb.Entity, cfg.Wandb.Project),
		}
	}
	return &detail, nil
}

func (m *MultiBackend) CompareMetrics(ctx context.Context, jobID, key string, desc bool) ([]CompareRow, error) {
	m.routingMu.RLock()
	defer m.routingMu.RUnlock()
	if _, err := m.refreshJobForStoreRead(ctx, jobID); err != nil {
		return nil, err
	}
	return compareRowsFromDB(ctx, m.store, jobID, key, desc)
}

// GPUStatus aggregates GPU visibility across ALL targets (local ∪ remote —
// remote targets report through their gpu_template, e.g. the runq preset's
// `runq gpu --json`; targets without one contribute nothing). Slots are
// stamped with their target name for panel grouping.
func (m *MultiBackend) GPUStatus(ctx context.Context) ([]GPUSlot, error) {
	m.routingMu.RLock()
	defer m.routingMu.RUnlock()
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
	m.routingMu.RLock()
	defer m.routingMu.RUnlock()
	be, err := m.resolveTask(ctx, taskID)
	if err != nil {
		return nil, err
	}
	return be.GetTask(ctx, taskID)
}

func (m *MultiBackend) TaskMetrics(ctx context.Context, taskID string, afterTS int64) ([]MetricPoint, error) {
	m.routingMu.RLock()
	defer m.routingMu.RUnlock()
	be, err := m.resolveTask(ctx, taskID)
	if err != nil {
		return nil, err
	}
	return be.TaskMetrics(ctx, taskID, afterTS)
}

func (m *MultiBackend) MetricKeys(ctx context.Context, jobID string) ([]string, error) {
	m.routingMu.RLock()
	defer m.routingMu.RUnlock()
	j, err := m.store.GetJob(ctx, jobID)
	if err != nil {
		return nil, err
	}
	if j == nil {
		return nil, fmt.Errorf("job %q: %w", jobID, ErrNotFound)
	}
	return m.store.MetricKeys(ctx, jobID)
}

func (m *MultiBackend) TaskMetricBuckets(ctx context.Context, taskID, key string, fromTS, toTS int64, maxBuckets int) ([]workspace.PyramidBucket, string, error) {
	m.routingMu.RLock()
	defer m.routingMu.RUnlock()
	be, err := m.resolveTask(ctx, taskID)
	if err != nil {
		return nil, "", err
	}
	return be.TaskMetricBuckets(ctx, taskID, key, fromTS, toTS, maxBuckets)
}

// ── RQ-44: log access — pure ownership routing ────────────────────────────

func (m *MultiBackend) TaskLogRead(ctx context.Context, taskID string, offset int64, maxLines int) (*LogPage, error) {
	m.routingMu.RLock()
	defer m.routingMu.RUnlock()
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
// D6). Lanes without the concept report reachable with LastChecked = now.
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
	m.routingMu.RLock()
	defer m.routingMu.RUnlock()
	type syncer interface {
		SyncInfo(context.Context) (int64, bool)
	}
	first := true
	for _, be := range m.liveLanes(target) {
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

// ForceRefreshTarget refreshes every live generation of the named target.
// The receipt is conservative across lanes: refreshed is true only when all
// lanes refreshed, refreshed_at is the oldest lane timestamp, and retry_after
// is the longest requested delay. Lane errors are joined after every lane has
// been attempted, so one success cannot mask another generation's failure.
func (m *MultiBackend) ForceRefreshTarget(ctx context.Context, target string) (*RefreshReceipt, error) {
	m.routingMu.RLock()
	defer m.routingMu.RUnlock()
	if target == "" {
		target = m.defaultName()
	}
	if target == "" {
		return nil, noTargetConfiguredError()
	}
	lanes := m.liveLanes(target)
	if len(lanes) == 0 {
		return nil, fmt.Errorf("refresh: %w: unknown target %q", ErrNotFound, target)
	}

	receipt := &RefreshReceipt{Refreshed: true}
	var reasons []string
	var refreshErrs []error
	for i, be := range lanes {
		laneReceipt := &RefreshReceipt{RefreshedAt: time.Now().Unix(), Refreshed: true}
		if fr, ok := be.(interface {
			ForceRefresh(context.Context) (*RefreshReceipt, error)
		}); ok {
			var err error
			laneReceipt, err = fr.ForceRefresh(ctx)
			if err != nil {
				refreshErrs = append(refreshErrs, fmt.Errorf("refresh target %q lane %d: %w", target, i+1, err))
			}
			if laneReceipt == nil {
				laneReceipt = &RefreshReceipt{Reason: "refresh returned no receipt"}
			}
		}

		if i == 0 || laneReceipt.RefreshedAt < receipt.RefreshedAt {
			receipt.RefreshedAt = laneReceipt.RefreshedAt
		}
		receipt.Refreshed = receipt.Refreshed && laneReceipt.Refreshed
		if laneReceipt.RetryAfterSeconds > receipt.RetryAfterSeconds {
			receipt.RetryAfterSeconds = laneReceipt.RetryAfterSeconds
		}
		if laneReceipt.Reason != "" {
			reasons = append(reasons, laneReceipt.Reason)
		}
	}
	if len(refreshErrs) > 0 {
		receipt.Refreshed = false
	}
	receipt.Reason = strings.Join(reasons, "; ")
	return receipt, errors.Join(refreshErrs...)
}

// ListTasks — flat table over the shared store (all lanes write here;
// tasks.target scopes). No routing needed.
func (m *MultiBackend) ListTasks(ctx context.Context, opts TaskListOptions) ([]TaskView, int, error) {
	return listTasksFromStore(ctx, m.store, opts)
}

func (m *MultiBackend) TaskLogTail(ctx context.Context, taskID string, maxLines int) (*LogPage, error) {
	m.routingMu.RLock()
	defer m.routingMu.RUnlock()
	be, err := m.resolveTask(ctx, taskID)
	if err != nil {
		return nil, err
	}
	return be.TaskLogTail(ctx, taskID, maxLines)
}

func (m *MultiBackend) TaskLogPage(ctx context.Context, taskID string, req logfile.PageRequest) (*LogPage, error) {
	m.routingMu.RLock()
	defer m.routingMu.RUnlock()
	be, err := m.resolveTask(ctx, taskID)
	if err != nil {
		return nil, err
	}
	return be.TaskLogPage(ctx, taskID, req)
}

func (m *MultiBackend) TaskLogFollow(ctx context.Context, taskID string, offset int64) (LogFollower, error) {
	m.routingMu.RLock()
	defer m.routingMu.RUnlock()
	be, err := m.resolveTask(ctx, taskID)
	if err != nil {
		return nil, err
	}
	return be.TaskLogFollow(ctx, taskID, offset)
}

func (m *MultiBackend) JobLogSearch(ctx context.Context, jobID, query string) ([]LogMatch, error) {
	m.routingMu.RLock()
	defer m.routingMu.RUnlock()
	_, _, groups, err := m.jobArtifactGroups(ctx, jobID)
	if err != nil {
		return nil, err
	}
	out := make([]LogMatch, 0)
	for _, group := range groups {
		matches, err := jobLogSearchRowsViaExec(ctx, group.fs, group.tasks, query, jobLogSearchMaxMatches-len(out))
		if err != nil {
			return nil, err
		}
		out = append(out, matches...)
		if len(out) >= jobLogSearchMaxMatches {
			break
		}
	}
	return out, nil
}

func (m *MultiBackend) JobActivity(ctx context.Context, jobID string) (*JobActivity, error) {
	m.routingMu.RLock()
	defer m.routingMu.RUnlock()
	j, rows, groups, err := m.jobArtifactGroups(ctx, jobID)
	if err != nil {
		return nil, err
	}
	byTask := make(map[string]TaskActivity, len(rows))
	for _, group := range groups {
		activity, err := taskActivityRowsViaExec(ctx, group.fs, group.tasks)
		if err != nil {
			return nil, err
		}
		for i := range activity {
			byTask[activity[i].TaskID] = activity[i]
		}
	}
	out := &JobActivity{Tasks: make([]TaskActivity, 0, len(rows))}
	for i := range rows {
		out.Tasks = append(out.Tasks, byTask[rows[i].ID])
	}
	out.JobStart, out.JobEnd = activityWindow(j, rows)
	return out, nil
}

func (m *MultiBackend) JobResults(ctx context.Context, jobID string) (*JobResults, error) {
	m.routingMu.RLock()
	defer m.routingMu.RUnlock()
	if _, err := m.refreshJobForStoreRead(ctx, jobID); err != nil {
		return nil, err
	}
	return jobResultsFromDB(ctx, m.store, jobID)
}

func (m *MultiBackend) KillTask(ctx context.Context, taskID string) error {
	m.controlMu.Lock()
	defer m.controlMu.Unlock()
	m.routingMu.RLock()
	defer m.routingMu.RUnlock()
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
	m.controlMu.Lock()
	defer m.controlMu.Unlock()
	m.routingMu.RLock()
	defer m.routingMu.RUnlock()
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
	// BeginTaskRetry atomically stamps the active generation together with
	// durable submitting/retry intent before wrapper reset. A failed reset
	// therefore remains owned and recoverable by this active lane.
	return be.RetryTask(ctx, taskID)
}

func (m *MultiBackend) KillJob(ctx context.Context, jobID string) error {
	m.controlMu.Lock()
	defer m.controlMu.Unlock()
	m.routingMu.RLock()
	defer m.routingMu.RUnlock()
	j, err := m.store.GetJob(ctx, jobID)
	if err != nil {
		return err
	}
	if j == nil {
		return fmt.Errorf("job %q: %w", jobID, ErrNotFound)
	}
	owners := m.liveLanes(j.Target)
	if len(owners) == 0 {
		return fmt.Errorf("unknown target %q", j.Target)
	}
	var killErrs []error
	// A job may span lane generations after a config change (in-flight
	// tasks stay with the retiring lane, pending work uses the active one).
	// Attempt every owner, but do not hide a retiring-lane failure: returning
	// one joined error is the only honest contract for a partially applied
	// whole-job cancellation.
	for i, be := range owners {
		if err := be.KillJob(ctx, jobID); err != nil {
			killErrs = append(killErrs, fmt.Errorf("kill job %s on owner lane %d: %w", jobID, i+1, err))
		}
	}
	return errors.Join(killErrs...)
}

func (m *MultiBackend) PauseJob(ctx context.Context, jobID string) error {
	m.controlMu.Lock()
	defer m.controlMu.Unlock()
	m.routingMu.RLock()
	defer m.routingMu.RUnlock()
	owners, err := m.jobControlLanes(ctx, jobID)
	if err != nil {
		return err
	}
	var pauseErrs []error
	for _, be := range owners {
		if err := be.PauseJob(ctx, jobID); err != nil {
			pauseErrs = append(pauseErrs, err)
		}
	}
	return errors.Join(pauseErrs...)
}

func (m *MultiBackend) ResumeJob(ctx context.Context, jobID string) error {
	m.controlMu.Lock()
	defer m.controlMu.Unlock()
	m.routingMu.RLock()
	defer m.routingMu.RUnlock()
	owners, err := m.jobControlLanes(ctx, jobID)
	if err != nil {
		return err
	}
	var resumeErrs []error
	for _, be := range owners {
		if err := be.ResumeJob(ctx, jobID); err != nil {
			resumeErrs = append(resumeErrs, err)
		}
	}
	return errors.Join(resumeErrs...)
}

// SubmitJob routes to the target specified in opts.Target, falling back to
// default_target.
func (m *MultiBackend) SubmitJob(ctx context.Context, cfg job.JobConfig, opts SubmitOptions) (string, int, error) {
	m.routingMu.RLock()
	defer m.routingMu.RUnlock()
	be, err := m.resolve(opts.Target)
	if err != nil {
		return "", 0, err
	}
	return be.SubmitJob(ctx, cfg, opts)
}

func (m *MultiBackend) DryRun(ctx context.Context, cfg job.JobConfig) (*DryRunResult, error) {
	m.routingMu.RLock()
	defer m.routingMu.RUnlock()
	be, err := m.defaultBackendE()
	if err != nil {
		return nil, err
	}
	return be.DryRun(ctx, cfg)
}

func (m *MultiBackend) PreviewSubmit(ctx context.Context, cfg job.JobConfig, skipPreflight bool) (PreviewResult, error) {
	m.routingMu.RLock()
	defer m.routingMu.RUnlock()
	be, err := m.defaultBackendE()
	if err != nil {
		return PreviewResult{}, err
	}
	return be.PreviewSubmit(ctx, cfg, skipPreflight)
}

// PreviewSubmitForTarget routes submit preview to the named target backend.
// Used by the API handler to give `submit --dry-run --target <hpc>` access
// to the HPC backend's full run.sh + submit-command rendering.
func (m *MultiBackend) PreviewSubmitForTarget(ctx context.Context, target string, cfg job.JobConfig, skipPreflight bool) (PreviewResult, error) {
	m.routingMu.RLock()
	defer m.routingMu.RUnlock()
	be, err := m.resolve(target)
	if err != nil {
		return PreviewResult{}, err
	}
	return be.PreviewSubmit(ctx, cfg, skipPreflight)
}

// DryRunForTarget routes dry-run expansion to the named target backend.
func (m *MultiBackend) DryRunForTarget(ctx context.Context, target string, cfg job.JobConfig) (*DryRunResult, error) {
	m.routingMu.RLock()
	defer m.routingMu.RUnlock()
	be, err := m.resolve(target)
	if err != nil {
		return nil, err
	}
	return be.DryRun(ctx, cfg)
}

func (m *MultiBackend) ListArchivedJobs(ctx context.Context) ([]JobSummary, error) {
	rows, err := m.store.ListJobsArchived(ctx, "", "")
	if err != nil {
		return nil, err
	}
	return m.renderJobRows(ctx, rows)
}

func (m *MultiBackend) ArchiveJob(ctx context.Context, jobID string) error {
	m.routingMu.RLock()
	defer m.routingMu.RUnlock()
	if _, err := m.refreshJobForStoreRead(ctx, jobID); err != nil {
		return err
	}
	return m.store.ArchiveJob(ctx, jobID)
}

func (m *MultiBackend) UnarchiveJob(ctx context.Context, jobID string) error {
	m.routingMu.RLock()
	defer m.routingMu.RUnlock()
	return m.store.UnarchiveJob(ctx, jobID)
}

func (m *MultiBackend) ArchiveProject(ctx context.Context, name string) error {
	be, err := m.defaultBackendE()
	if err != nil {
		return err
	}
	return be.ArchiveProject(ctx, name)
}

func (m *MultiBackend) UnarchiveProject(ctx context.Context, name string) error {
	be, err := m.defaultBackendE()
	if err != nil {
		return err
	}
	return be.UnarchiveProject(ctx, name)
}

func (m *MultiBackend) ResolveNote(ctx context.Context, cfg job.JobConfig) (string, error) {
	be, err := m.defaultBackendE()
	if err != nil {
		return "", err
	}
	return be.ResolveNote(ctx, cfg)
}

// orphanDetector is the optional per-target capability of scanning for
// missing task dirs THROUGH that target's own filesystem (rfs.FS) — the only
// vantage point that can tell "gone" from "unobservable".
type orphanDetector interface {
	DetectOrphansNow(ctx context.Context) error
}

func (m *MultiBackend) Clean(ctx context.Context, opts CleanOptions) (*CleanResult, error) {
	m.routingMu.RLock()
	defer m.routingMu.RUnlock()
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
	return PerformClean(ctx, m.store, func(task store.TaskRow) (rfs.FS, error) {
		be, err := m.resolveTaskRow(&task)
		if err != nil {
			return nil, err
		}
		if fsp, ok := be.(interface{ FS() rfs.FS }); ok {
			return fsp.FS(), nil
		}
		return rfs.NewLocalFS(), nil
	}, opts)
}

func (m *MultiBackend) ThawTasks(ctx context.Context, owner int, force bool) (*ThawResponse, error) {
	be, err := m.defaultBackendE()
	if err != nil {
		return nil, err
	}
	return be.ThawTasks(ctx, owner, force)
}

// ── Project operations (global, not per-target) ────────────────────────────

func (m *MultiBackend) GetProject(ctx context.Context, name string) (*project.Config, error) {
	be, err := m.defaultBackendE()
	if err != nil {
		return nil, err
	}
	return be.GetProject(ctx, name)
}

func (m *MultiBackend) ListProjects(ctx context.Context) ([]ProjectSummary, error) {
	be, err := m.defaultBackendE()
	if err != nil {
		return nil, err
	}
	return be.ListProjects(ctx)
}

func (m *MultiBackend) MatchProjects(ctx context.Context, dir string) ([]ProjectSummary, error) {
	be, err := m.defaultBackendE()
	if err != nil {
		return nil, err
	}
	return be.MatchProjects(ctx, dir)
}

func (m *MultiBackend) CreateProject(ctx context.Context, cfg project.Config) error {
	// Fill the home target explicitly at birth (empty = wherever the user
	// is aimed by default). A project registered "against tsubame" must
	// SAY tsubame — implicit defaults rot when default_target changes.
	if cfg.Target == "" {
		cfg.Target = m.defaultName()
	}
	be, err := m.defaultBackendE()
	if err != nil {
		return err
	}
	return be.CreateProject(ctx, cfg)
}

func (m *MultiBackend) UpdateProject(ctx context.Context, cfg project.Config) error {
	be, err := m.defaultBackendE()
	if err != nil {
		return err
	}
	return be.UpdateProject(ctx, cfg)
}

func (m *MultiBackend) RenameProject(ctx context.Context, oldName, newName string) error {
	be, err := m.defaultBackendE()
	if err != nil {
		return err
	}
	return be.RenameProject(ctx, oldName, newName)
}

// compile-time interface check
var _ Backend = (*MultiBackend)(nil)
