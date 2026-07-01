package dashboard

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	"github.com/gliese129/runq/internal/backend"
	"github.com/gliese129/runq/internal/config"
	"github.com/gliese129/runq/internal/job"
	"github.com/gliese129/runq/internal/logfile"
	"github.com/gliese129/runq/internal/project"
)

// backgroundReconciler is the optional capability for HPC backends that can
// run a bulk reconcile pass over all active jobs. The dashboard's background
// ticker calls this instead of triggering reconcile from the list endpoint.
type backgroundReconciler interface {
	ReconcileAll(ctx context.Context) error
}

type Server struct {
	backend      backend.Backend
	mode         string
	cfg          *config.GlobalConfig
	mux          *http.ServeMux
	static       fs.FS
	staticSource string
	staticErr    error
	utilsLogs    *utilsLogStore

	// Background reconcile (HPC mode only): the ticker runs EnsureAllFresh
	// periodically so the frontend list endpoint is a pure DB read.
	reconciler backgroundReconciler // nil in daemon mode
	lastPoll   atomic.Int64         // unix timestamp of last frontend poll
	stopLoop   context.CancelFunc   // cancels the reconcileLoop goroutine
}

func NewServer(be backend.Backend, mode string, cfg *config.GlobalConfig) *Server {
	return NewServerWithAssets(be, mode, cfg, "")
}

func NewServerWithAssets(be backend.Backend, mode string, cfg *config.GlobalConfig, assetsDir string) *Server {
	static := ResolveStaticAssets(assetsDir)
	s := &Server{
		backend:      be,
		mode:         mode,
		cfg:          cfg,
		mux:          http.NewServeMux(),
		static:       static.FS,
		staticSource: static.Source,
		staticErr:    static.Err,
		utilsLogs:    newUtilsLogStore(),
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
	return corsMiddleware(s.mux)
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
// EnsureAllFresh (batch probe) on a 30 s cadence — but ONLY when the
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
				slog.Warn("reconcileLoop: EnsureAllFresh failed", "err", err)
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

func (s *Server) registerRoutes() {
	s.mux.HandleFunc("GET /api/dashboard/config", s.handleConfig)
	s.mux.HandleFunc("GET /api/dashboard/jobs", s.handleListJobs)
	s.mux.HandleFunc("GET /api/dashboard/jobs/{id}", s.handleGetJob)
	s.mux.HandleFunc("GET /api/dashboard/jobs/{id}/compare", s.handleCompare)
	s.mux.HandleFunc("GET /api/dashboard/projects", s.handleListProjects)
	s.mux.HandleFunc("GET /api/dashboard/projects/match", s.handleMatchProjects)
	s.mux.HandleFunc("GET /api/dashboard/projects/{name}", s.handleGetProject)
	s.mux.HandleFunc("POST /api/dashboard/projects", s.handleCreateProject)
	s.mux.HandleFunc("PUT /api/dashboard/projects/{name}", s.handleUpdateProject)
	s.mux.HandleFunc("POST /api/dashboard/projects/{name}/rename", s.handleRenameProject)
	s.mux.HandleFunc("GET /api/dashboard/gpu", s.handleGPU)
	s.mux.HandleFunc("POST /api/dashboard/jobs", s.handleSubmitJob)
	s.mux.HandleFunc("POST /api/dashboard/jobs/dry-run", s.handleDryRun)
	s.mux.HandleFunc("POST /api/dashboard/jobs/preview", s.handlePreviewSubmit)
	s.mux.HandleFunc("GET /api/dashboard/jobs/archived", s.handleListArchivedJobs)
	s.mux.HandleFunc("POST /api/dashboard/jobs/{id}/archive", s.handleArchiveJob)
	s.mux.HandleFunc("POST /api/dashboard/jobs/{id}/unarchive", s.handleUnarchiveJob)
	s.mux.HandleFunc("POST /api/dashboard/projects/{name}/archive", s.handleArchiveProject)
	s.mux.HandleFunc("POST /api/dashboard/projects/{name}/unarchive", s.handleUnarchiveProject)
	s.mux.HandleFunc("POST /api/dashboard/jobs/resolve-note", s.handleResolveNote)
	s.mux.HandleFunc("GET /api/dashboard/tasks/{id}", s.handleGetTask)
	s.mux.HandleFunc("GET /api/dashboard/tasks/{id}/log", s.handleTaskLog)
	s.mux.HandleFunc("GET /api/dashboard/tasks/{id}/log/stream", s.handleTaskLogStream)
	s.mux.HandleFunc("GET /api/dashboard/tasks/{id}/metrics", s.handleTaskMetrics)
	s.mux.HandleFunc("GET /api/dashboard/jobs/{id}/activity", s.handleJobActivity)
	s.mux.HandleFunc("GET /api/dashboard/jobs/{id}/log/search", s.handleJobLogSearch)
	s.mux.HandleFunc("POST /api/dashboard/tasks/{id}/kill", s.handleKillTask)
	s.mux.HandleFunc("POST /api/dashboard/tasks/{id}/retry", s.handleRetryTask)
	s.mux.HandleFunc("POST /api/dashboard/utils/log", s.handleUtilsLogUpload)
	s.mux.HandleFunc("GET /api/dashboard/utils/log/{id}", s.handleUtilsLogRead)
	s.mux.HandleFunc("GET /api/dashboard/utils/log/{id}/search", s.handleUtilsLogSearch)
	s.mux.HandleFunc("DELETE /api/dashboard/utils/log/{id}", s.handleUtilsLogDelete)
	s.mux.HandleFunc("POST /api/dashboard/jobs/{id}/kill", s.handleKillJob)
	s.mux.HandleFunc("POST /api/dashboard/jobs/{id}/refresh", s.handleRefreshJob)
	s.mux.HandleFunc("POST /api/dashboard/jobs/{id}/pause", s.handlePauseJob)
	s.mux.HandleFunc("POST /api/dashboard/jobs/{id}/resume", s.handleResumeJob)
	s.mux.HandleFunc("GET /api/dashboard/hpc-config", s.handleGetHPCConfig)
	s.mux.HandleFunc("GET /api/dashboard/hpc-config/presets", s.handleHPCPresets)
	s.mux.HandleFunc("PUT /api/dashboard/hpc-config", s.handlePutHPCConfig)
	s.mux.HandleFunc("POST /api/dashboard/hpc-config/check", s.handleCheckHPCConfig)
	s.mux.HandleFunc("PUT /api/dashboard/global-config", s.handlePutGlobalConfig)
	s.mux.HandleFunc("GET /api/dashboard/fs/list", s.handleFSList)
	s.mux.HandleFunc("GET /api/dashboard/fs/read", s.handleFSRead)
	s.mux.HandleFunc("POST /api/dashboard/fs/parse-script", s.handleParseScript)
	s.mux.HandleFunc("GET /api/dashboard/conda/envs", s.handleCondaEnvs)
	s.mux.HandleFunc("GET /api/{path...}", s.handleAPINotFound)
	s.mux.HandleFunc("POST /api/{path...}", s.handleAPINotFound)
	s.mux.HandleFunc("PUT /api/{path...}", s.handleAPINotFound)
	s.mux.HandleFunc("DELETE /api/{path...}", s.handleAPINotFound)
	s.mux.HandleFunc("GET /", s.handleSPA)
}

func (s *Server) handleConfig(w http.ResponseWriter, r *http.Request) {
	// Capabilities come from the backend's self-description — the server
	// does not infer them from mode (design philosophy #2).
	writeJSON(w, http.StatusOK, backend.ConfigResponse{
		Mode:         s.mode,
		DataPath:     s.cfg.DataPath,
		ConfigPath:   config.ConfigPath(),
		Capabilities: s.backend.Capabilities(),
	})
}

func (s *Server) handleRefreshJob(w http.ResponseWriter, r *http.Request) {
	if err := s.backend.RefreshJob(r.Context(), r.PathValue("id")); err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, backend.ActionResponse{OK: true})
}

func (s *Server) handleListProjects(w http.ResponseWriter, r *http.Request) {
	projects, err := s.backend.ListProjects(r.Context())
	if err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, projects)
}

func (s *Server) handleMatchProjects(w http.ResponseWriter, r *http.Request) {
	dir := r.URL.Query().Get("dir")
	if dir == "" {
		writeErrorStatus(w, http.StatusBadRequest, fmt.Errorf("dir query parameter is required"))
		return
	}
	projects, err := s.backend.MatchProjects(r.Context(), dir)
	if err != nil {
		writeError(w, err)
		return
	}
	if projects == nil {
		projects = []backend.ProjectSummary{}
	}
	writeJSON(w, http.StatusOK, projects)
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

func (s *Server) handleListJobs(w http.ResponseWriter, r *http.Request) {
	// Mark that the frontend is actively polling — the background
	// reconcileLoop checks this to skip work when nobody is watching.
	s.lastPoll.Store(time.Now().Unix())

	jobs, err := s.backend.ListJobs(r.Context(), r.URL.Query().Get("project"))
	if err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, jobs)
}

func (s *Server) handleGetJob(w http.ResponseWriter, r *http.Request) {
	detail, err := s.backend.GetJob(r.Context(), r.PathValue("id"))
	if err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, detail)
}

func (s *Server) handleCompare(w http.ResponseWriter, r *http.Request) {
	key := r.URL.Query().Get("key")
	if key == "" {
		writeErrorStatus(w, http.StatusBadRequest, fmt.Errorf("key is required"))
		return
	}
	desc := strings.EqualFold(r.URL.Query().Get("order"), "desc")
	rows, err := s.backend.CompareMetrics(r.Context(), r.PathValue("id"), key, desc)
	if err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, rows)
}

func (s *Server) handleGPU(w http.ResponseWriter, r *http.Request) {
	gpus, err := s.backend.GPUStatus(r.Context())
	if err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, gpus)
}

func (s *Server) handleSubmitJob(w http.ResponseWriter, r *http.Request) {
	var cfg job.JobConfig
	if err := json.NewDecoder(r.Body).Decode(&cfg); err != nil {
		writeErrorStatus(w, http.StatusBadRequest, err)
		return
	}
	cfg.Project = strings.TrimSpace(cfg.Project)
	if cfg.Project == "" {
		writeErrorStatus(w, http.StatusBadRequest, fmt.Errorf("project is required"))
		return
	}
	opts := backend.SubmitOptions{
		SkipPreflight: r.URL.Query().Get("no_preflight") == "1",
		Target:        r.URL.Query().Get("target"),
	}
	jobID, total, err := s.backend.SubmitJob(r.Context(), cfg, opts)
	if err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"job_id":      jobID,
		"total_tasks": total,
	})
}

func (s *Server) handleDryRun(w http.ResponseWriter, r *http.Request) {
	var cfg job.JobConfig
	if err := json.NewDecoder(r.Body).Decode(&cfg); err != nil {
		writeErrorStatus(w, http.StatusBadRequest, err)
		return
	}
	result, err := s.backend.DryRun(r.Context(), cfg)
	if err != nil {
		writeError(w, err)
		return
	}
	tasks := result.Tasks
	if tasks == nil {
		tasks = []job.TaskParams{}
	}
	writeJSON(w, http.StatusOK, tasks)
}

func (s *Server) handleListArchivedJobs(w http.ResponseWriter, r *http.Request) {
	jobs, err := s.backend.ListArchivedJobs(r.Context())
	if err != nil {
		writeError(w, err)
		return
	}
	if jobs == nil {
		jobs = []backend.JobSummary{}
	}
	writeJSON(w, http.StatusOK, jobs)
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
	var body struct {
		Config        job.JobConfig `json:"config"`
		SkipPreflight bool          `json:"skip_preflight"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeErrorStatus(w, http.StatusBadRequest, err)
		return
	}
	text, err := s.backend.PreviewSubmit(r.Context(), body.Config, body.SkipPreflight)
	if err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"preview": text})
}

func (s *Server) handleGetTask(w http.ResponseWriter, r *http.Request) {
	view, err := s.backend.GetTask(r.Context(), r.PathValue("id"))
	if err != nil {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, view)
}

// handleTaskLog reads a page of log lines starting at a byte offset.
// Query params:
//   - offset: byte offset into the raw log file (default 0)
//   - lines:  number of lines to return (default 200, max 5000)
//
// Returns a logfile.Page JSON. The frontend uses start_offset / end_offset
// for forward/backward paging and total_bytes for progress indication.
func (s *Server) handleTaskLog(w http.ResponseWriter, r *http.Request) {
	view, err := s.backend.GetTask(r.Context(), r.PathValue("id"))
	if err != nil {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": err.Error()})
		return
	}
	logPath := view.LogPath
	if logPath == "" {
		writeJSON(w, http.StatusOK, &logfile.Page{Lines: []string{}})
		return
	}

	offset := int64(0)
	if v := r.URL.Query().Get("offset"); v != "" {
		if n, e := strconv.ParseInt(v, 10, 64); e == nil && n >= 0 {
			offset = n
		}
	}
	lines := logfile.DefaultPageLines
	if v := r.URL.Query().Get("lines"); v != "" {
		if n, e := strconv.Atoi(v); e == nil && n > 0 && n <= logfile.MaxPageLines {
			lines = n
		}
	}

	lr, err := logfile.Open(logPath)
	if err != nil {
		writeJSON(w, http.StatusOK, &logfile.Page{Lines: []string{}})
		return
	}
	defer lr.Close()

	page, err := lr.ReadLines(offset, lines)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, page)
}

// handleTaskLogStream streams new log lines via SSE.
// Query params:
//   - offset: byte offset to start streaming from (default 0)
//
// Sends "lines" events containing logfile.Page JSON as new content appears.
// Stops when the client disconnects (EventSource.close / page navigation).
func (s *Server) handleTaskLogStream(w http.ResponseWriter, r *http.Request) {
	view, err := s.backend.GetTask(r.Context(), r.PathValue("id"))
	if err != nil {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": err.Error()})
		return
	}
	logPath := view.LogPath
	if logPath == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "no log file"})
		return
	}

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming not supported", http.StatusInternalServerError)
		return
	}

	offset := int64(0)
	if v := r.URL.Query().Get("offset"); v != "" {
		if n, e := strconv.ParseInt(v, 10, 64); e == nil && n >= 0 {
			offset = n
		}
	}

	lr, err := logfile.Open(logPath)
	if err != nil {
		http.Error(w, "cannot open log file", http.StatusInternalServerError)
		return
	}
	defer lr.Close()

	ticker := time.NewTicker(1 * time.Second)
	defer ticker.Stop()

	for {
		select {
		case <-r.Context().Done():
			return
		case <-ticker.C:
			if err := lr.Refresh(); err != nil {
				continue
			}
			if lr.Size() <= offset {
				continue
			}
			page, err := lr.ReadLines(offset, logfile.DefaultPageLines)
			if err != nil || len(page.Lines) == 0 {
				continue
			}
			data, _ := json.Marshal(page)
			fmt.Fprintf(w, "event: lines\ndata: %s\n\n", data)
			flusher.Flush()
			offset = page.EndOffset
		}
	}
}

func (s *Server) handleTaskMetrics(w http.ResponseWriter, r *http.Request) {
	points, err := s.backend.TaskMetrics(r.Context(), r.PathValue("id"))
	if err != nil {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": err.Error()})
		return
	}
	if points == nil {
		points = []backend.MetricPoint{}
	}
	writeJSON(w, http.StatusOK, points)
}

// handleJobActivity returns activity.tsv data for all tasks in a job.
// The conversion from bytes to lines is deferred to the logfile package
// once it's implemented (Step 4).
func (s *Server) handleJobActivity(w http.ResponseWriter, r *http.Request) {
	detail, err := s.backend.GetJob(r.Context(), r.PathValue("id"))
	if err != nil {
		writeError(w, err)
		return
	}
	type activityPoint struct {
		TS    int64 `json:"ts"`
		Bytes int64 `json:"bytes"`
		Lines int64 `json:"lines"`
	}
	type taskActivity struct {
		TaskID string          `json:"task_id"`
		Status string          `json:"status"`
		Points []activityPoint `json:"points"`
	}
	tasks := make([]taskActivity, 0, len(detail.Tasks))
	for _, t := range detail.Tasks {
		ta := taskActivity{TaskID: t.ID, Status: t.Status, Points: []activityPoint{}}
		// TODO(p6-step4): read activity.tsv from task dir + lazy bytes→lines
		tasks = append(tasks, ta)
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"tasks": tasks,
	})
}

// handleJobLogSearch searches across all task logs in a job.
// The actual search is deferred to the logfile package (Step 5).
func (s *Server) handleJobLogSearch(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query().Get("q")
	if q == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "q parameter required"})
		return
	}
	// TODO(p6-step5): iterate task log files using logfile.Reader.Search
	writeJSON(w, http.StatusOK, map[string]any{
		"matches":     []any{},
		"next_offset": 0,
		"truncated":   false,
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

func writeError(w http.ResponseWriter, err error) {
	status := http.StatusInternalServerError
	msg := strings.ToLower(err.Error())
	if errors.Is(err, backend.ErrNotSupported) {
		status = http.StatusBadRequest
	} else if backend.IsNotFound(err) {
		status = http.StatusNotFound
	} else if strings.Contains(msg, "already exists") {
		status = http.StatusConflict
	} else if strings.Contains(msg, "required") || strings.Contains(msg, "invalid") {
		status = http.StatusBadRequest
	}
	writeErrorStatus(w, status, err)
}

func writeErrorStatus(w http.ResponseWriter, status int, err error) {
	writeJSON(w, status, backend.ErrorResponse{
		Error: err.Error(),
		Code:  status,
	})
}

func ParsePort(port string) (int, error) {
	p, err := strconv.Atoi(port)
	if err != nil || p < 1 || p > 65535 {
		return 0, fmt.Errorf("invalid port %q", port)
	}
	return p, nil
}
