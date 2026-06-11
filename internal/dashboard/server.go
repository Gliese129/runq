package dashboard

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"net/http"
	"os"
	"strconv"
	"strings"

	"github.com/gliese129/runq/internal/config"
	"github.com/gliese129/runq/internal/job"
	"github.com/gliese129/runq/internal/project"
)

type Server struct {
	backend      Backend
	mode         string
	cfg          *config.GlobalConfig
	mux          *http.ServeMux
	static       fs.FS
	staticSource string
	staticErr    error
}

// ConfigResponse and ErrorResponse are in types.go.

func NewServer(backend Backend, mode string, cfg *config.GlobalConfig) *Server {
	return NewServerWithAssets(backend, mode, cfg, "")
}

func NewServerWithAssets(backend Backend, mode string, cfg *config.GlobalConfig, assetsDir string) *Server {
	static := ResolveStaticAssets(assetsDir)
	s := &Server{
		backend:      backend,
		mode:         mode,
		cfg:          cfg,
		mux:          http.NewServeMux(),
		static:       static.FS,
		staticSource: static.Source,
		staticErr:    static.Err,
	}
	s.registerRoutes()
	return s
}

func (s *Server) Handler() http.Handler {
	return corsMiddleware(s.mux)
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
	s.mux.HandleFunc("POST /api/dashboard/jobs/resolve-note", s.handleResolveNote)
	s.mux.HandleFunc("GET /api/dashboard/tasks/{id}", s.handleGetTask)
	s.mux.HandleFunc("GET /api/dashboard/tasks/{id}/log", s.handleTaskLog)
	s.mux.HandleFunc("GET /api/dashboard/tasks/{id}/metrics", s.handleTaskMetrics)
	s.mux.HandleFunc("POST /api/dashboard/tasks/{id}/kill", s.handleKillTask)
	s.mux.HandleFunc("POST /api/dashboard/tasks/{id}/retry", s.handleRetryTask)
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
	writeJSON(w, http.StatusOK, ConfigResponse{
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
	writeJSON(w, http.StatusOK, ActionResponse{OK: true})
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
		projects = []ProjectSummary{}
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
	jobs, err := s.backend.ListJobs(r.Context())
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
	opts := SubmitOptions{
		SkipPreflight: r.URL.Query().Get("no_preflight") == "1",
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
	tasks, err := s.backend.DryRun(r.Context(), cfg)
	if err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, tasks)
}

func (s *Server) handleGetTask(w http.ResponseWriter, r *http.Request) {
	view, _, err := s.backend.GetTask(r.Context(), r.PathValue("id"))
	if err != nil {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, view)
}

func (s *Server) handleTaskLog(w http.ResponseWriter, r *http.Request) {
	_, logPath, err := s.backend.GetTask(r.Context(), r.PathValue("id"))
	if err != nil {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": err.Error()})
		return
	}
	if logPath == "" {
		writeJSON(w, http.StatusOK, map[string]any{"lines": []string{}, "total_lines": 0, "start": 0, "end": 0})
		return
	}

	tail := 500
	if v := r.URL.Query().Get("tail"); v != "" {
		if n, e := strconv.Atoi(v); e == nil && n > 0 && n <= 5000 {
			tail = n
		}
	}
	offset := 0
	if v := r.URL.Query().Get("offset"); v != "" {
		if n, e := strconv.Atoi(v); e == nil && n >= 0 {
			offset = n
		}
	}

	f, err := os.Open(logPath)
	if err != nil {
		writeJSON(w, http.StatusOK, map[string]any{"lines": []string{}, "total_lines": 0, "start": 0, "end": 0, "error": "log file not accessible"})
		return
	}
	defer f.Close()

	var allLines []string
	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	for scanner.Scan() {
		allLines = append(allLines, scanner.Text())
	}
	total := len(allLines)
	end := total - offset
	if end < 0 {
		end = 0
	}
	start := end - tail
	if start < 0 {
		start = 0
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"lines":       allLines[start:end],
		"total_lines": total,
		"start":       start,
		"end":         end,
	})
}

func (s *Server) handleTaskMetrics(w http.ResponseWriter, r *http.Request) {
	points, err := s.backend.TaskMetrics(r.Context(), r.PathValue("id"))
	if err != nil {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": err.Error()})
		return
	}
	if points == nil {
		points = []MetricPoint{}
	}
	writeJSON(w, http.StatusOK, points)
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
	writeJSON(w, http.StatusOK, ActionResponse{OK: true})
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
	if errors.Is(err, ErrNotSupported) {
		status = http.StatusBadRequest
	} else if isNotFound(err) {
		status = http.StatusNotFound
	} else if strings.Contains(msg, "already exists") {
		status = http.StatusConflict
	} else if strings.Contains(msg, "required") || strings.Contains(msg, "invalid") {
		status = http.StatusBadRequest
	}
	writeErrorStatus(w, status, err)
}

func writeErrorStatus(w http.ResponseWriter, status int, err error) {
	writeJSON(w, status, ErrorResponse{
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
