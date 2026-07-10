package dashboard

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"net/http"
	"runtime/debug"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/gliese129/runq/internal/backend"
	"github.com/gliese129/runq/internal/config"
	"github.com/gliese129/runq/internal/job"
	"github.com/gliese129/runq/internal/logfile"
	"github.com/gliese129/runq/internal/project"
	"github.com/gliese129/runq/internal/version"
	"github.com/gliese129/runq/internal/workspace"
)

// backgroundReconciler is the optional capability for HPC backends that can
// run a bulk reconcile pass over all active jobs. The dashboard's background
// ticker calls this instead of triggering reconcile from the list endpoint.
type backgroundReconciler interface {
	ReconcileAll(ctx context.Context) error
}

type Server struct {
	backend      backend.Backend
	cfg          *config.GlobalConfig
	mux          *http.ServeMux
	static       fs.FS
	staticSource string
	staticErr    error
	utilsLogs    *utilsLogStore

	// Background reconcile (HPC mode only): the ticker runs SchedulerProbe
	// periodically so the frontend list endpoint is a pure DB read.
	reconciler backgroundReconciler // nil in daemon mode
	lastPoll   atomic.Int64         // unix timestamp of last frontend poll
	stopLoop   context.CancelFunc   // cancels the reconcileLoop goroutine

	version   string    // /health; stamped via internal/version (ldflags)
	startedAt time.Time // /health uptime_seconds

	// forwardStarter is the client daemon's runtime hook for POST
	// /targets/{name}/connect (start/replace a remote CLI forward without
	// a restart). nil on deployments without forwards (runqd) → 501.
	forwardStarter func(name string) error
	forwardStopper func(name string) error

	// Per-job forced-refresh floor (D22): memory-only — a restart forgiving
	// the throttle is harmless.
	jobRefreshMu sync.Mutex
	jobRefreshAt map[string]time.Time
}

// jobRefreshFloor is the per-job forced-probe floor. Shorter than the
// target-level floor would be pointless (a job refresh IS a probe); 30s
// keeps the button honest without making it feel broken.
const jobRefreshFloor = 30 * time.Second

// NewServer builds the v1 server. mode is gone (D5/D9): capabilities are
// declared per-target, never inferred.
func NewServer(be backend.Backend, cfg *config.GlobalConfig) *Server {
	return NewServerWithAssets(be, cfg, "")
}

func NewServerWithAssets(be backend.Backend, cfg *config.GlobalConfig, assetsDir string) *Server {
	static := ResolveStaticAssets(assetsDir)
	s := &Server{
		backend:      be,
		cfg:          cfg,
		mux:          http.NewServeMux(),
		static:       static.FS,
		staticSource: static.Source,
		staticErr:    static.Err,
		utilsLogs:    newUtilsLogStore(),
		version:      version.Version,
		startedAt:    time.Now(),
		jobRefreshAt: map[string]time.Time{},
	}
	s.registerRoutes()

	// HPC mode: start background reconciler so scheduler probing (qstat/
	// squeue) runs on its own 30 s cadence, decoupled from the frontend's
	// faster poll cycle. ListJobs still does per-job EnsureFresh, but the
	// probe is a TTL cache hit (local reads only) once the batch probe has
	// pre-filled the cache.
	if r, ok := be.(backgroundReconciler); ok {
		ctx, cancel := context.WithCancel(context.Background())
		s.reconciler = r
		s.stopLoop = cancel
		go s.reconcileLoop(ctx)
	}

	return s
}

func (s *Server) Handler() http.Handler {
	return recoverMiddleware(corsMiddleware(versionGateMiddleware(s.mux)))
}

// versionGateMiddleware is the client-version guardrail: a runq client that
// self-identifies (X-Runq-Version) as older than version.MinClient gets a
// 426 with upgrade instructions instead of a confusing downstream failure.
// It fires only when BOTH sides are stamped builds — dev builds and
// browsers (no header) pass through, and version echo happens on every
// response so the client can warn about mild skew (see api.warnVersionSkew).
// Protocol-shape compatibility is owned by /api/v1 path versioning, not here.
func versionGateMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Runq-Version", version.Version)
		clientV := r.Header.Get("X-Runq-Version")
		if clientV != "" && version.MinClient != "" {
			if cmp, ok := version.Compare(clientV, version.MinClient); ok && cmp < 0 {
				writeErr(w, http.StatusUpgradeRequired, backend.CodeBadRequest,
					fmt.Sprintf("runq client %s is older than the daemon's minimum %s (daemon %s) — rerun `runq connect` on your workstation to update the remote CLI, or reinstall runq",
						clientV, version.MinClient, version.Version))
				return
			}
		}
		next.ServeHTTP(w, r)
	})
}

// recoverMiddleware turns a handler panic into a logged 500 instead of a
// dropped connection: without it the CLI sees a bare "EOF" and the panic's
// stack never reaches anyone. (gin has this built in; this mux needs it.)
func recoverMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer func() {
			if rec := recover(); rec != nil {
				slog.Error("panic in dashboard handler",
					"path", r.URL.Path, "panic", rec, "stack", string(debug.Stack()))
				writeErr(w, http.StatusInternalServerError, backend.CodeInternal,
					fmt.Sprintf("internal error: %v", rec))
			}
		}()
		next.ServeHTTP(w, r)
	})
}

// Close stops the background reconcile loop (if running). Safe to call
// multiple times or when no loop was started.
func (s *Server) Close() {
	if s.stopLoop != nil {
		s.stopLoop()
	}
}

// reconcileInterval is the background reconcile cadence for HPC mode.
// Aligned with DefaultReadTTL (30 s) — this is the minimum safe interval
// for polling cluster schedulers (pbs_server, slurmctld, sge_qmaster).
// The batch probe (one `qstat -u $USER`) pre-fills every job's per-job
// TTL cache, so ListJobs' inline EnsureFresh calls only do cheap local
// reads until the next tick.
const reconcileInterval = 30 * time.Second

// pollActivityWindow: skip reconcile when the frontend hasn't polled in this
// window — nobody is watching, so don't waste scheduler queries.
const pollActivityWindow = 60 * time.Second

// reconcileLoop runs in a background goroutine (HPC mode only). It runs
// SchedulerProbe (batch) on a 30 s cadence — but ONLY when the
// frontend has polled within the last 60 s (nobody watching = no work).
//
// ListJobs still does per-job EnsureFresh on every call, but the scheduler
// probe is TTL-gated: once the batch probe here pre-fills every job's TTL
// cache, those inline calls only do cheap local reads (status.json,
// metrics.jsonl) until the next tick.
//
// An immediate warm-up run fires before the first tick so the batch probe
// cache is hot before the frontend's first poll arrives — without this,
// the first poll would trigger N per-job probes, bypassing batch entirely.
func (s *Server) reconcileLoop(ctx context.Context) {
	// Warm-up: pre-fill probe cache before the first frontend poll.
	// lastPoll is zero here, so we skip the activity check just this once.
	if err := s.reconciler.ReconcileAll(ctx); err != nil {
		slog.Warn("reconcileLoop: initial warm-up failed", "err", err)
	}

	ticker := time.NewTicker(reconcileInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			lp := s.lastPoll.Load()
			if lp == 0 {
				continue // no poll yet — nobody is watching
			}
			if time.Since(time.Unix(lp, 0)) > pollActivityWindow {
				continue // frontend went idle
			}
			if err := s.reconciler.ReconcileAll(ctx); err != nil {
				slog.Warn("reconcileLoop: scheduler probe failed", "err", err)
			}
		}
	}
}

func (s *Server) StaticAssetsUnavailable() bool {
	return s.static == nil
}

// corsMiddleware allows localhost origins for vite dev server.
func corsMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		origin := r.Header.Get("Origin")
		if strings.HasPrefix(origin, "http://localhost:") || strings.HasPrefix(origin, "http://127.0.0.1:") {
			w.Header().Set("Access-Control-Allow-Origin", origin)
			w.Header().Set("Access-Control-Allow-Methods", "GET, POST, PUT, DELETE, OPTIONS")
			w.Header().Set("Access-Control-Allow-Headers", "Content-Type")
		}
		if r.Method == http.MethodOptions {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		next.ServeHTTP(w, r)
	})
}

// registerRoutes mounts the v1 protocol surface (spec §5). Single surface
// for CLI and WebUI: the same mux is served on the unix socket and TCP.
// Layout follows the spec sections; legacy /api/dashboard/* is gone
// (spec-first, no back-compat — D1).
func (s *Server) registerRoutes() {
	// ── §5.1 System ──────────────────────────────────────────────────
	s.mux.HandleFunc("GET /api/v1/health", s.handleHealth)
	s.mux.HandleFunc("GET /api/v1/config", s.handleConfig)
	s.mux.HandleFunc("PUT /api/v1/config", s.handlePutGlobalConfig)
	s.mux.HandleFunc("POST /api/v1/clean", s.handleClean)

	// ── §5.2 Targets（含 target 级资源）────────────────────────────────
	// "presets" is a reserved name (never a valid target); register the
	// literal route before the {name} wildcards.
	s.mux.HandleFunc("GET /api/v1/targets", s.handleListTargets)
	s.mux.HandleFunc("GET /api/v1/targets/presets", s.handleTargetPresets)
	s.mux.HandleFunc("PUT /api/v1/targets/{name}", s.handlePutTarget)
	s.mux.HandleFunc("DELETE /api/v1/targets/{name}", s.handleDeleteTarget)
	s.mux.HandleFunc("POST /api/v1/targets/{name}/check", s.handleCheckTarget)
	s.mux.HandleFunc("POST /api/v1/targets/{name}/connect", s.handleConnectTarget)
	s.mux.HandleFunc("POST /api/v1/targets/{name}/disconnect", s.handleDisconnectTarget)
	s.mux.HandleFunc("POST /api/v1/targets/{name}/refresh", s.handleRefreshTarget)
	s.mux.HandleFunc("GET /api/v1/targets/{name}/gpus", s.handleTargetGPUs)
	s.mux.HandleFunc("GET /api/v1/targets/{name}/fs/list", s.handleFSList)
	s.mux.HandleFunc("GET /api/v1/targets/{name}/fs/read", s.handleFSRead)
	s.mux.HandleFunc("POST /api/v1/targets/{name}/fs/parse-script", s.handleParseScript)
	s.mux.HandleFunc("GET /api/v1/targets/{name}/python-envs", s.handlePythonEnvs)

	// ── §5.3 Projects ────────────────────────────────────────────────
	s.mux.HandleFunc("GET /api/v1/projects", s.handleListProjects) // ?dir=&archived= 吸收旧 /projects/match
	s.mux.HandleFunc("POST /api/v1/projects", s.handleCreateProject)
	s.mux.HandleFunc("GET /api/v1/projects/{name}", s.handleGetProject)
	s.mux.HandleFunc("PUT /api/v1/projects/{name}", s.handleUpdateProject)
	s.mux.HandleFunc("POST /api/v1/projects/{name}/rename", s.handleRenameProject)
	s.mux.HandleFunc("POST /api/v1/projects/{name}/archive", s.handleArchiveProject)
	s.mux.HandleFunc("POST /api/v1/projects/{name}/unarchive", s.handleUnarchiveProject)

	// ── §5.4 Jobs（提交族 = plan → preview → submit，选项进 body）──────
	s.mux.HandleFunc("POST /api/v1/jobs/plan", s.handlePlanJob) // 合并 dry-run + resolve-note
	s.mux.HandleFunc("POST /api/v1/jobs/preview", s.handlePreviewSubmit)
	s.mux.HandleFunc("POST /api/v1/jobs", s.handleSubmitJob)
	s.mux.HandleFunc("GET /api/v1/jobs", s.handleListJobs) // ?project=&archived=&status=&target=&limit=&offset=
	s.mux.HandleFunc("GET /api/v1/jobs/{id}", s.handleGetJob)
	s.mux.HandleFunc("GET /api/v1/jobs/{id}/metrics", s.handleJobMetrics) // 双模式：无 key → {keys}，有 key → {rows}
	s.mux.HandleFunc("GET /api/v1/jobs/{id}/activity", s.handleJobActivity)
	s.mux.HandleFunc("GET /api/v1/jobs/{id}/log/search", s.handleJobLogSearch)
	s.mux.HandleFunc("GET /api/v1/jobs/{id}/events", s.handleJobEvents) // SSE §6.4（capability: event_stream）
	s.mux.HandleFunc("POST /api/v1/jobs/{id}/kill", s.handleKillJob)
	s.mux.HandleFunc("POST /api/v1/jobs/{id}/pause", s.handlePauseJob)
	s.mux.HandleFunc("POST /api/v1/jobs/{id}/resume", s.handleResumeJob)
	s.mux.HandleFunc("POST /api/v1/jobs/{id}/archive", s.handleArchiveJob)
	s.mux.HandleFunc("POST /api/v1/jobs/{id}/unarchive", s.handleUnarchiveJob)
	s.mux.HandleFunc("POST /api/v1/jobs/{id}/refresh", s.handleRefreshJob)

	// ── §5.5 Tasks ───────────────────────────────────────────────────
	s.mux.HandleFunc("GET /api/v1/tasks", s.handleListTasks) // ?job=&status=&target=&limit=&offset=
	s.mux.HandleFunc("GET /api/v1/tasks/{id}", s.handleGetTask)
	s.mux.HandleFunc("GET /api/v1/tasks/{id}/log", s.handleTaskLog)
	s.mux.HandleFunc("GET /api/v1/tasks/{id}/log/stream", s.handleTaskLogStream)
	s.mux.HandleFunc("GET /api/v1/tasks/{id}/metrics", s.handleTaskMetrics)
	s.mux.HandleFunc("POST /api/v1/tasks/{id}/kill", s.handleKillTask)
	s.mux.HandleFunc("POST /api/v1/tasks/{id}/retry", s.handleRetryTask)

	// ── §5.6 Log sessions（原 /utils/log）──────────────────────────────
	s.mux.HandleFunc("POST /api/v1/log-sessions", s.handleUtilsLogUpload)
	s.mux.HandleFunc("GET /api/v1/log-sessions/{id}", s.handleUtilsLogRead)
	s.mux.HandleFunc("GET /api/v1/log-sessions/{id}/search", s.handleUtilsLogSearch)
	s.mux.HandleFunc("DELETE /api/v1/log-sessions/{id}", s.handleUtilsLogDelete)

	// (thaw is deliberately absent: freeze/thaw is a runqd-machine concern
	// and stays on the runqd executor lane, spec §9.)
	s.mux.HandleFunc("GET /api/{path...}", s.handleAPINotFound)
	s.mux.HandleFunc("POST /api/{path...}", s.handleAPINotFound)
	s.mux.HandleFunc("PUT /api/{path...}", s.handleAPINotFound)
	s.mux.HandleFunc("DELETE /api/{path...}", s.handleAPINotFound)
	s.mux.HandleFunc("GET /", s.handleSPA)
}

// handleConfig — GET /config, the v1 bootstrap summary (spec §4): paths,
// default_target, per-target type/scheduler/capabilities. Identity comes
// from config.yaml (ResolveTargets), capability bits from each backend's
// self-description (philosophy #2: declared facts, not inferences) — mode
// is gone from the wire (D5/D9).
func (s *Server) handleConfig(w http.ResponseWriter, r *http.Request) {
	resp := backend.ConfigResponse{
		DataPath:      s.cfg.DataPath,
		ConfigPath:    config.ConfigPath(),
		DefaultTarget: s.cfg.ResolveDefaultTarget(),
		Targets:       []backend.TargetSummary{},
	}
	caps := map[string]backend.Capabilities{}
	if mt, ok := s.backend.(interface {
		PerTargetCapabilities() map[string]backend.Capabilities
		DefaultTargetName() string
	}); ok {
		caps = mt.PerTargetCapabilities()
		resp.DefaultTarget = mt.DefaultTargetName()
	}
	for _, t := range s.cfg.ResolveTargets() {
		ts := backend.TargetSummary{
			Name:      t.Name,
			Type:      t.Type(),
			Scheduler: t.Scheduler,
		}
		if c, ok := caps[t.Name]; ok {
			ts.Capabilities = c
		} else {
			ts.Capabilities = s.backend.Capabilities()
		}
		resp.Targets = append(resp.Targets, ts)
	}
	writeJSON(w, http.StatusOK, resp)
}

// handleClean routes a clean request through the Backend (MultiBackend fans
// out per-target orphan detection before the shared PerformClean).
func (s *Server) handleClean(w http.ResponseWriter, r *http.Request) {
	var opts backend.CleanOptions
	if err := json.NewDecoder(r.Body).Decode(&opts); err != nil {
		writeErr(w, http.StatusBadRequest, backend.CodeBadRequest, "invalid clean options: "+err.Error())
		return
	}
	// At least one selector must be present (mirrors the legacy endpoint).
	if !opts.Orphan && !opts.Archived && opts.JobID == "" && opts.TaskID == "" &&
		len(opts.TaskIDs) == 0 && opts.OlderThan == nil {
		writeErr(w, http.StatusBadRequest, backend.CodeBadRequest, "at least one selector required (older_than, orphan, archived, job_id, task_id, task_ids)")
		return
	}
	result, err := s.backend.Clean(r.Context(), opts)
	if err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, result)
}

// handleRefreshJob — POST /jobs/{id}/refresh. Returns the D22 receipt so
// the caller knows whether it actually refreshed. Per-job floor: a second
// forced probe for the SAME job within the window gets an honest
// {refreshed:false, reason:"min_interval"} receipt instead of a qstat.
func (s *Server) handleRefreshJob(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")

	s.jobRefreshMu.Lock()
	if last, ok := s.jobRefreshAt[id]; ok {
		if since := time.Since(last); since < jobRefreshFloor {
			s.jobRefreshMu.Unlock()
			writeJSON(w, http.StatusOK, backend.RefreshReceipt{
				RefreshedAt:       last.Unix(),
				Refreshed:         false,
				Reason:            "min_interval",
				RetryAfterSeconds: int64((jobRefreshFloor - since).Seconds()) + 1,
			})
			return
		}
	}
	s.jobRefreshAt[id] = time.Now()
	s.jobRefreshMu.Unlock()

	if err := s.backend.RefreshJob(r.Context(), id); err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, backend.RefreshReceipt{
		RefreshedAt: time.Now().Unix(), // this probe JUST ran: now is the truth
		Refreshed:   true,
	})
}

// handleHealth — GET /health (spec §5.1, D6): PASSIVE endpoint. Target
// reachability comes from each lane's most recent transport outcome
// (marker scan / probe / user op) — never an active probe.
func (s *Server) handleHealth(w http.ResponseWriter, r *http.Request) {
	targets := []backend.TargetHealth{}
	if ph, ok := s.backend.(interface{ PerTargetHealth() []backend.TargetHealth }); ok {
		targets = ph.PerTargetHealth()
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"version":        s.version,
		"uptime_seconds": int64(time.Since(s.startedAt).Seconds()),
		"targets":        targets,
	})
}

// handleListTasks — GET /tasks?job=&status=&target=&limit=&offset= (spec
// §5.5, D7): the flat task table. Pagination is SQL-level (D20).
func (s *Server) handleListTasks(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	opts := backend.TaskListOptions{
		JobID:  q.Get("job"),
		Status: q.Get("status"),
		Target: q.Get("target"),
		Limit:  200,
	}
	if n, err := strconv.Atoi(q.Get("limit")); err == nil && n > 0 {
		opts.Limit = n
	}
	if n, err := strconv.Atoi(q.Get("offset")); err == nil && n > 0 {
		opts.Offset = n
	}
	items, total, err := s.backend.ListTasks(r.Context(), opts)
	if err != nil {
		writeError(w, err)
		return
	}
	env := envelope(items)
	env.Total = &total
	stampFreshness(r.Context(), s.backend, opts.Target, &env.RefreshedAt, &env.Stale)
	writeJSON(w, http.StatusOK, env)
}

// handleJobEvents — GET /jobs/{id}/events (spec §6.4, D14): SSE state
// stream, capability event_stream. Push targets emit live; poll targets
// emit on cache refresh. Lands with the cache layer + #49.
func (s *Server) handleJobEvents(w http.ResponseWriter, r *http.Request) {
	notImplemented(w, "job event stream") // TODO(#49): SSE `state` events
}

// handleListProjects — GET /projects?dir=&archived= (spec §5.3, D3).
// ?dir= absorbs the retired /projects/match; projects are local config,
// so the envelope carries no freshness fields.
func (s *Server) handleListProjects(w http.ResponseWriter, r *http.Request) {
	var projects []backend.ProjectSummary
	var err error
	if dir := r.URL.Query().Get("dir"); dir != "" {
		projects, err = s.backend.MatchProjects(r.Context(), dir)
	} else {
		projects, err = s.backend.ListProjects(r.Context())
	}
	if err != nil {
		writeError(w, err)
		return
	}
	if v := r.URL.Query().Get("archived"); v != "" {
		want := v == "true" || v == "1"
		kept := projects[:0]
		for _, p := range projects {
			if p.Archived == want {
				kept = append(kept, p)
			}
		}
		projects = kept
	}
	writeJSON(w, http.StatusOK, envelope(projects))
}

func (s *Server) handleGetProject(w http.ResponseWriter, r *http.Request) {
	name := strings.TrimSpace(r.PathValue("name"))
	if name == "" {
		writeErrorStatus(w, http.StatusBadRequest, fmt.Errorf("project name is required"))
		return
	}
	cfg, err := s.backend.GetProject(r.Context(), name)
	if err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, cfg)
}

func (s *Server) handleCreateProject(w http.ResponseWriter, r *http.Request) {
	var cfg project.Config
	if err := json.NewDecoder(r.Body).Decode(&cfg); err != nil {
		writeErrorStatus(w, http.StatusBadRequest, err)
		return
	}
	cfg.ProjectName = strings.TrimSpace(cfg.ProjectName)
	if cfg.ProjectName == "" {
		writeErrorStatus(w, http.StatusBadRequest, fmt.Errorf("project_name is required"))
		return
	}
	if err := s.backend.CreateProject(r.Context(), cfg); err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, map[string]string{
		"message": fmt.Sprintf("project %q created", cfg.ProjectName),
	})
}

func (s *Server) handleUpdateProject(w http.ResponseWriter, r *http.Request) {
	name := strings.TrimSpace(r.PathValue("name"))
	if name == "" {
		writeErrorStatus(w, http.StatusBadRequest, fmt.Errorf("project name is required"))
		return
	}
	var cfg project.Config
	if err := json.NewDecoder(r.Body).Decode(&cfg); err != nil {
		writeErrorStatus(w, http.StatusBadRequest, err)
		return
	}
	cfg.ProjectName = name
	if err := s.backend.UpdateProject(r.Context(), cfg); err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{
		"message": fmt.Sprintf("project %q updated", name),
	})
}

func (s *Server) handleRenameProject(w http.ResponseWriter, r *http.Request) {
	oldName := strings.TrimSpace(r.PathValue("name"))
	if oldName == "" {
		writeErrorStatus(w, http.StatusBadRequest, fmt.Errorf("project name is required"))
		return
	}
	var body struct {
		NewName string `json:"new_name"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeErrorStatus(w, http.StatusBadRequest, err)
		return
	}
	newName := strings.TrimSpace(body.NewName)
	if newName == "" {
		writeErrorStatus(w, http.StatusBadRequest, fmt.Errorf("new_name is required"))
		return
	}
	if err := s.backend.RenameProject(r.Context(), oldName, newName); err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{
		"message": fmt.Sprintf("project %q renamed to %q", oldName, newName),
	})
}

// targetScopedLister is MultiBackend's per-target list capability — the CLI's
// --target / RUNQ_TARGET scoping arrives here as a query parameter.
type targetScopedLister interface {
	ListJobsForTarget(ctx context.Context, target, projectScope string) ([]backend.JobSummary, error)
	ListArchivedJobsForTarget(ctx context.Context, target string) ([]backend.JobSummary, error)
}

// handleListJobs — GET /jobs?project=&archived=&status=&target=&limit=&offset=
// (spec §5.4, D3/D20). ?archived=true absorbs the retired /jobs/archived.
// status/pagination are handler-level filters for now; they move into SQL
// once the list queries grow LIMIT/OFFSET (D20 落地在 store 层时).
func (s *Server) handleListJobs(w http.ResponseWriter, r *http.Request) {
	// Mark that the frontend is actively polling — the background
	// reconcileLoop checks this to skip work when nobody is watching.
	s.lastPoll.Store(time.Now().Unix())

	q := r.URL.Query()
	archived := q.Get("archived") == "true" || q.Get("archived") == "1"
	target := q.Get("target")

	var jobs []backend.JobSummary
	var err error
	switch {
	case target != "" && archived:
		tl, ok := s.backend.(targetScopedLister)
		if !ok {
			writeErr(w, http.StatusBadRequest, backend.CodeBadRequest, "target scoping not supported by this backend")
			return
		}
		jobs, err = tl.ListArchivedJobsForTarget(r.Context(), target)
	case target != "":
		tl, ok := s.backend.(targetScopedLister)
		if !ok {
			writeErr(w, http.StatusBadRequest, backend.CodeBadRequest, "target scoping not supported by this backend")
			return
		}
		jobs, err = tl.ListJobsForTarget(r.Context(), target, q.Get("project"))
	case archived:
		jobs, err = s.backend.ListArchivedJobs(r.Context())
	default:
		jobs, err = s.backend.ListJobs(r.Context(), q.Get("project"))
	}
	if err != nil {
		writeError(w, err)
		return
	}

	if status := q.Get("status"); status != "" {
		kept := jobs[:0]
		for _, j := range jobs {
			if j.Status == status {
				kept = append(kept, j)
			}
		}
		jobs = kept
	}

	total := len(jobs)
	jobs = paginate(jobs, q.Get("limit"), q.Get("offset"))

	// Per-job freshness (spec §6.1: jobs in one response may belong to
	// DIFFERENT targets, so each row carries its own lane's timestamp).
	// One SyncInfo per distinct target, not per job.
	if sy, ok := s.backend.(interface {
		SyncInfo(context.Context, string) (int64, bool, bool)
	}); ok {
		type fresh struct {
			at    int64
			known bool
		}
		byTarget := map[string]fresh{}
		for i := range jobs {
			t := jobs[i].Target
			f, seen := byTarget[t]
			if !seen {
				at, _, known := sy.SyncInfo(r.Context(), t)
				f = fresh{at: at, known: known}
				byTarget[t] = f
			}
			if f.known && f.at > 0 {
				at := f.at
				jobs[i].RefreshedAt = &at
			}
		}
	}

	env := envelope(jobs)
	env.Total = &total
	stampFreshness(r.Context(), s.backend, target, &env.RefreshedAt, &env.Stale)
	writeJSON(w, http.StatusOK, env)
}

// stampFreshness fills envelope refreshed_at/stale from the backend's L4
// sync ledger (real persisted timestamps — never response time). Backends
// without the concept leave the fields omitted, which the spec allows.
// Reading SyncInfo doubles as the SWR trigger: soft-stale data nudges the
// lane's sensor in the background; this request never waits.
func stampFreshness(ctx context.Context, be backend.Backend, target string, refreshedAt **int64, stale **bool) {
	sy, ok := be.(interface {
		SyncInfo(context.Context, string) (int64, bool, bool)
	})
	if !ok {
		return
	}
	at, st, known := sy.SyncInfo(ctx, target)
	if !known {
		return
	}
	*refreshedAt = &at
	*stale = &st
}

// paginate applies limit/offset query semantics (default limit 200).
func paginate[T any](items []T, limitStr, offsetStr string) []T {
	limit, offset := 200, 0
	if n, err := strconv.Atoi(limitStr); err == nil && n > 0 {
		limit = n
	}
	if n, err := strconv.Atoi(offsetStr); err == nil && n > 0 {
		offset = n
	}
	if offset >= len(items) {
		return []T{}
	}
	items = items[offset:]
	if len(items) > limit {
		items = items[:limit]
	}
	return items
}

func (s *Server) handleGetJob(w http.ResponseWriter, r *http.Request) {
	detail, err := s.backend.GetJob(r.Context(), r.PathValue("id"))
	if err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, detail)
}

// handleJobMetrics — GET /jobs/{id}/metrics, dual-mode (spec §5.4, D13):
// without ?key= → {keys: [...]} (key discovery); with ?key=&order=&limit=
// → {rows: MetricRankRow[]}. Both shapes are objects so the frontend can
// discriminate by field.
func (s *Server) handleJobMetrics(w http.ResponseWriter, r *http.Request) {
	key := r.URL.Query().Get("key")
	if key == "" {
		keys, err := s.backend.MetricKeys(r.Context(), r.PathValue("id"))
		if err != nil {
			writeError(w, err)
			return
		}
		if keys == nil {
			keys = []string{}
		}
		writeJSON(w, http.StatusOK, map[string]any{"keys": keys})
		return
	}
	desc := strings.EqualFold(r.URL.Query().Get("order"), "desc")
	rows, err := s.backend.CompareMetrics(r.Context(), r.PathValue("id"), key, desc)
	if err != nil {
		writeError(w, err)
		return
	}
	if rows == nil {
		rows = []backend.CompareRow{}
	}
	rows = paginate(rows, r.URL.Query().Get("limit"), "")
	writeJSON(w, http.StatusOK, map[string]any{"rows": rows})
}

// handleTargetGPUs — GET /targets/{name}/gpus (spec §5.2, D11). The
// aggregated GPUStatus is filtered down to the addressed target.
// refreshed_at/stale become real once the light cache lands (L4); until
// then the response claims "fresh now", which is true for push targets
// and a TODO-honest lie for poll ones.
func (s *Server) handleTargetGPUs(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	gpus, err := s.backend.GPUStatus(r.Context())
	if err != nil {
		writeError(w, err)
		return
	}
	kept := make([]backend.GPUSlot, 0, len(gpus))
	for _, g := range gpus {
		if g.Target == name {
			kept = append(kept, g)
		}
	}
	env := envelope(kept)
	// GPU status is a live exec (or the lane's ≤30s view cache) — "now"
	// is honest within that TTL; no sync_state row needed.
	now := time.Now().Unix()
	stale := false
	env.RefreshedAt, env.Stale = &now, &stale
	writeJSON(w, http.StatusOK, env)
}

// submitBody is the v1 submit-family request body (spec §5.4, D12):
// options live in the body, never in query parameters.
type submitBody struct {
	Config        job.JobConfig `json:"config"`
	Target        string        `json:"target"`         // 缺省 = default_target
	SkipPreflight bool          `json:"skip_preflight"` //nolint:tagliatelle
}

func (s *Server) handleSubmitJob(w http.ResponseWriter, r *http.Request) {
	var body submitBody
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeErr(w, http.StatusBadRequest, backend.CodeBadRequest, err.Error())
		return
	}
	body.Config.Project = strings.TrimSpace(body.Config.Project)
	if body.Config.Project == "" {
		writeErr(w, http.StatusBadRequest, backend.CodeBadRequest, "config.project is required")
		return
	}
	jobID, total, err := s.backend.SubmitJob(r.Context(), body.Config, backend.SubmitOptions{
		SkipPreflight: body.SkipPreflight,
		Target:        body.Target,
	})
	if err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"job_id":      jobID,
		"total_tasks": total,
	})
}

// handlePlanJob — POST /jobs/plan (spec §5.4, D12): merges the retired
// dry-run + resolve-note into one cheap, purely local expansion so the
// submit wizard makes a single call.
func (s *Server) handlePlanJob(w http.ResponseWriter, r *http.Request) {
	var body submitBody
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeErr(w, http.StatusBadRequest, backend.CodeBadRequest, err.Error())
		return
	}
	result, err := s.backend.DryRun(r.Context(), body.Config)
	if err != nil {
		writeError(w, err)
		return
	}
	tasks := result.Tasks
	if tasks == nil {
		tasks = []job.TaskParams{}
	}
	note, err := s.backend.ResolveNote(r.Context(), body.Config)
	if err != nil {
		// note 解析失败不应阻塞 plan：降级为原样返回
		note = body.Config.Note
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"tasks":         tasks,
		"note_resolved": note,
		"warnings":      []string{}, // TODO(L3): preflight-adjacent warnings
	})
}

func (s *Server) handleArchiveJob(w http.ResponseWriter, r *http.Request) {
	if err := s.backend.ArchiveJob(r.Context(), r.PathValue("id")); err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"message": "archived"})
}

func (s *Server) handleUnarchiveJob(w http.ResponseWriter, r *http.Request) {
	if err := s.backend.UnarchiveJob(r.Context(), r.PathValue("id")); err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"message": "unarchived"})
}

func (s *Server) handleArchiveProject(w http.ResponseWriter, r *http.Request) {
	if err := s.backend.ArchiveProject(r.Context(), r.PathValue("name")); err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"message": "archived"})
}

func (s *Server) handleUnarchiveProject(w http.ResponseWriter, r *http.Request) {
	if err := s.backend.UnarchiveProject(r.Context(), r.PathValue("name")); err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"message": "unarchived"})
}

func (s *Server) handlePreviewSubmit(w http.ResponseWriter, r *http.Request) {
	var body submitBody
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeErr(w, http.StatusBadRequest, backend.CodeBadRequest, err.Error())
		return
	}
	// body.target routes the preview to that target's backend (full run.sh
	// + submit command rendering); default target otherwise. (D11: target
	// 一律显式进 body，?target= 变体已退役。)
	var text string
	var err error
	if body.Target != "" {
		pt, ok := s.backend.(interface {
			PreviewSubmitForTarget(ctx context.Context, target string, cfg job.JobConfig, skipPreflight bool) (string, error)
		})
		if !ok {
			writeErr(w, http.StatusBadRequest, backend.CodeBadRequest, "target scoping not supported by this backend")
			return
		}
		text, err = pt.PreviewSubmitForTarget(r.Context(), body.Target, body.Config, body.SkipPreflight)
	} else {
		text, err = s.backend.PreviewSubmit(r.Context(), body.Config, body.SkipPreflight)
	}
	if err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"preview": text})
}

func (s *Server) handleGetTask(w http.ResponseWriter, r *http.Request) {
	view, err := s.backend.GetTask(r.Context(), r.PathValue("id"))
	if err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, view)
}

// handleTaskLog reads a page of log lines starting at a byte offset.
// Query params:
//   - offset: byte offset into the raw log file; ABSENT = tail view
//     (TaskLogTail — the first-paint entry point)
//   - lines:  number of lines to return (default 200, max 5000)
//
// Returns a logfile.Page JSON (offset / next_offset / size / truncated).
// All path/FS resolution lives in the owning lane behind the Backend
// interface — this handler never touches the filesystem.
func (s *Server) handleTaskLog(w http.ResponseWriter, r *http.Request) {
	lines := logfile.DefaultPageLines
	if v := r.URL.Query().Get("lines"); v != "" {
		if n, e := strconv.Atoi(v); e == nil && n > 0 && n <= logfile.MaxPageLines {
			lines = n
		}
	}

	var page *backend.LogPage
	var err error
	if v := r.URL.Query().Get("offset"); v != "" {
		offset, e := strconv.ParseInt(v, 10, 64)
		if e != nil || offset < 0 {
			writeErr(w, http.StatusBadRequest, backend.CodeBadRequest, "invalid offset")
			return
		}
		page, err = s.backend.TaskLogRead(r.Context(), r.PathValue("id"), offset, lines)
	} else {
		page, err = s.backend.TaskLogTail(r.Context(), r.PathValue("id"), lines)
	}
	if err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, page)
}

// handleTaskLogStream streams new log lines via SSE.
// Query params:
//   - offset: byte offset to start streaming from (default 0)
//
// Sends "lines" events containing logfile.Page JSON as new content appears.
// The handler is a bare pull loop over LogFollower — poll cadence,
// rotation handling and pending-file waiting all live in the follower.
// Stops when the client disconnects (EventSource.close / page navigation).
func (s *Server) handleTaskLogStream(w http.ResponseWriter, r *http.Request) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		writeErr(w, http.StatusInternalServerError, backend.CodeInternal, "streaming not supported")
		return
	}

	offset := int64(0)
	if v := r.URL.Query().Get("offset"); v != "" {
		if n, e := strconv.ParseInt(v, 10, 64); e == nil && n >= 0 {
			offset = n
		}
	}

	f, err := s.backend.TaskLogFollow(r.Context(), r.PathValue("id"), offset)
	if err != nil {
		writeError(w, err)
		return
	}
	defer f.Close()

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")

	for {
		page, err := f.Next(r.Context())
		if err != nil {
			return // ctx cancelled (client gone) or follower failed: end stream
		}
		data, _ := json.Marshal(page)
		fmt.Fprintf(w, "event: lines\ndata: %s\n\n", data)
		flusher.Flush()
	}
}

// handleTaskMetrics — GET /tasks/{id}/metrics, dual-mode (spec §5.5/§6.4):
// without ?key= → {points, refreshed_at} (all-key tail points; ?after=
// incremental); with ?key=&buckets=&from=&to= → {buckets, source}
// (terminal → pyramid, otherwise tail aggregation).
func (s *Server) handleTaskMetrics(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	if key := q.Get("key"); key != "" {
		parse := func(name string) int64 {
			n, _ := strconv.ParseInt(q.Get(name), 10, 64)
			return n
		}
		maxBuckets := 2000
		if n, e := strconv.Atoi(q.Get("buckets")); e == nil && n > 0 {
			maxBuckets = n
		}
		buckets, source, err := s.backend.TaskMetricBuckets(
			r.Context(), r.PathValue("id"), key, parse("from"), parse("to"), maxBuckets)
		if err != nil {
			writeError(w, err)
			return
		}
		if buckets == nil {
			buckets = []workspace.PyramidBucket{}
		}
		writeJSON(w, http.StatusOK, map[string]any{"buckets": buckets, "source": source})
		return
	}

	after := int64(0)
	if v := q.Get("after"); v != "" {
		if n, e := strconv.ParseInt(v, 10, 64); e == nil && n > 0 {
			after = n
		}
	}
	points, err := s.backend.TaskMetrics(r.Context(), r.PathValue("id"), after)
	if err != nil {
		writeError(w, err)
		return
	}
	if points == nil {
		points = []backend.MetricPoint{}
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"points":       points,
		"refreshed_at": time.Now().Unix(), // live tail-window read: now IS the read time
	})
}

// handleJobActivity returns activity.tsv data for all tasks in a job.
// The conversion from bytes to lines is deferred to the logfile package
// once it's implemented (Step 4).
func (s *Server) handleJobActivity(w http.ResponseWriter, r *http.Request) {
	notImplemented(w, "job activity")
}

// handleJobLogSearch searches across all task logs in a job.
// The actual search is deferred to the logfile package (Step 5).
func (s *Server) handleJobLogSearch(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query().Get("q")
	if q == "" {
		writeErr(w, http.StatusBadRequest, backend.CodeBadRequest, "q parameter required")
		return
	}

	// NOTE: limit/offset paginate the RESPONSE, not the grep — the lane
	// greps up to its 500-match cap regardless, so paging here saves wire
	// bytes only. Pushing offset into the grep would need per-batch
	// cursors; not worth it while the cap bounds total work.
	matches, err := s.backend.JobLogSearch(r.Context(), r.PathValue("id"), q)
	if err != nil {
		writeError(w, err)
		return
	}
	total := len(matches)
	limit := 100
	if n, err := strconv.Atoi(r.URL.Query().Get("limit")); err == nil && n > 0 {
		limit = n
	}
	offset := 0
	if n, err := strconv.Atoi(r.URL.Query().Get("offset")); err == nil && n > 0 {
		offset = n
	}
	if offset > total {
		offset = total
	}
	end := min(offset+limit, total)
	truncated := end < total
	nextOffset := 0
	if truncated {
		nextOffset = end
	}
	// Wire shape (spec §5.4): {task_id, line_no, text}. Deliberately NO
	// byte offset — the grep source has none, and a permanent zero would
	// bait the frontend into building jump-to-offset on it; jumps go
	// through the log paging / pyramid raw ranges instead.
	type searchMatch struct {
		TaskID string `json:"task_id"`
		LineNo int    `json:"line_no"`
		Text   string `json:"text"`
	}
	out := make([]searchMatch, 0, end-offset)
	for _, m := range matches[offset:end] {
		out = append(out, searchMatch{TaskID: m.TaskID, LineNo: m.Line, Text: m.Text})
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"matches":     out,
		"next_offset": nextOffset,
		"truncated":   truncated,
	})
}

func (s *Server) handleKillTask(w http.ResponseWriter, r *http.Request) {
	s.handleAction(w, r, func() error {
		return s.backend.KillTask(r.Context(), r.PathValue("id"))
	})
}

func (s *Server) handleRetryTask(w http.ResponseWriter, r *http.Request) {
	s.handleAction(w, r, func() error {
		return s.backend.RetryTask(r.Context(), r.PathValue("id"))
	})
}

func (s *Server) handleKillJob(w http.ResponseWriter, r *http.Request) {
	s.handleAction(w, r, func() error {
		return s.backend.KillJob(r.Context(), r.PathValue("id"))
	})
}

func (s *Server) handlePauseJob(w http.ResponseWriter, r *http.Request) {
	s.handleAction(w, r, func() error {
		return s.backend.PauseJob(r.Context(), r.PathValue("id"))
	})
}

func (s *Server) handleResumeJob(w http.ResponseWriter, r *http.Request) {
	s.handleAction(w, r, func() error {
		return s.backend.ResumeJob(r.Context(), r.PathValue("id"))
	})
}

func (s *Server) handleAction(w http.ResponseWriter, _ *http.Request, fn func() error) {
	if err := fn(); err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, backend.ActionResponse{OK: true})
}

func (s *Server) handleAPINotFound(w http.ResponseWriter, r *http.Request) {
	writeErrorStatus(w, http.StatusNotFound, fmt.Errorf("api route not found: %s", r.URL.Path))
}

func (s *Server) handleSPA(w http.ResponseWriter, r *http.Request) {
	if s.static == nil {
		writeMissingDashboard(w, s.staticErr)
		return
	}
	path := strings.TrimPrefix(r.URL.Path, "/")
	if path == "" {
		path = "index.html"
	}
	if _, err := fs.Stat(s.static, path); err != nil {
		path = "index.html"
	}
	data, err := fs.ReadFile(s.static, path)
	if err != nil {
		writeErrorStatus(w, http.StatusNotFound, err)
		return
	}
	w.Header().Set("Content-Type", mimeByExt(path))
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(data)
}

func mimeByExt(path string) string {
	switch {
	case strings.HasSuffix(path, ".html"):
		return "text/html; charset=utf-8"
	case strings.HasSuffix(path, ".js"):
		return "application/javascript"
	case strings.HasSuffix(path, ".css"):
		return "text/css"
	case strings.HasSuffix(path, ".svg"):
		return "image/svg+xml"
	case strings.HasSuffix(path, ".json"):
		return "application/json"
	case strings.HasSuffix(path, ".png"):
		return "image/png"
	case strings.HasSuffix(path, ".ico"):
		return "image/x-icon"
	default:
		return "application/octet-stream"
	}
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

// writeError classifies an error into the v1 (status, code) pair (spec §2).
// Handlers that already know the code should call writeErr directly.
func writeError(w http.ResponseWriter, err error) {
	status, code := http.StatusInternalServerError, backend.CodeInternal
	msg := strings.ToLower(err.Error())
	switch {
	case errors.Is(err, backend.ErrNotSupported):
		status, code = http.StatusConflict, backend.CodeNotSupported
	case backend.IsNotFound(err):
		status, code = http.StatusNotFound, backend.CodeNotFound
	case strings.Contains(msg, "already exists"):
		status, code = http.StatusConflict, backend.CodeInvalidState
	case strings.Contains(msg, "required") || strings.Contains(msg, "invalid"):
		status, code = http.StatusBadRequest, backend.CodeBadRequest
	}
	writeErr(w, status, code, err.Error())
}

// writeErr emits the v1 error envelope: error for humans, code for
// programs (stable enum — clients branch only on code).
func writeErr(w http.ResponseWriter, status int, code, msg string) {
	writeJSON(w, status, backend.ErrorResponse{Error: msg, Code: code})
}

// writeErrorStatus is a transitional shim for status-only call sites: the
// code is derived from the status. New code should call writeErr directly.
func writeErrorStatus(w http.ResponseWriter, status int, err error) {
	code := backend.CodeInternal
	switch status {
	case http.StatusBadRequest:
		code = backend.CodeBadRequest
	case http.StatusNotFound:
		code = backend.CodeNotFound
	case http.StatusConflict:
		code = backend.CodeInvalidState
	}
	writeErr(w, status, code, err.Error())
}

// notImplemented is the spec-first stub response: the endpoint is defined
// in the protocol spec but its layer below is not built yet (501).
func notImplemented(w http.ResponseWriter, what string) {
	writeErr(w, http.StatusNotImplemented, backend.CodeNotImplemented, what+": not implemented yet")
}

// listEnvelope is the v1 list response (spec §2/D20): never bare arrays.
// Freshness fields are pointers so plain local resources (projects) omit
// them entirely.
type listEnvelope[T any] struct {
	Items       []T    `json:"items"`
	Total       *int   `json:"total,omitempty"`
	RefreshedAt *int64 `json:"refreshed_at,omitempty"`
	Stale       *bool  `json:"stale,omitempty"`
}

// envelope wraps items, materializing nil slices to [] (JSON []).
func envelope[T any](items []T) listEnvelope[T] {
	if items == nil {
		items = []T{}
	}
	return listEnvelope[T]{Items: items}
}

func ParsePort(port string) (int, error) {
	p, err := strconv.Atoi(port)
	if err != nil || p < 1 || p > 65535 {
		return 0, fmt.Errorf("invalid port %q", port)
	}
	return p, nil
}
