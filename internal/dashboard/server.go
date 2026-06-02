package dashboard

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"net/http"
	"strconv"
	"strings"

	"github.com/gliese129/runq/internal/config"
	"github.com/gliese129/runq/internal/job"
)

type Server struct {
	backend Backend
	mode    string
	cfg     *config.GlobalConfig
	mux     *http.ServeMux
	static  fs.FS
}

// ConfigResponse and ErrorResponse are in types.go.

func NewServer(backend Backend, mode string, cfg *config.GlobalConfig) *Server {
	s := &Server{
		backend: backend,
		mode:    mode,
		cfg:     cfg,
		mux:     http.NewServeMux(),
		static:  StaticFS(),
	}
	s.registerRoutes()
	return s
}

func (s *Server) Handler() http.Handler {
	return corsMiddleware(s.mux)
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
	s.mux.HandleFunc("GET /api/dashboard/jobs/{id}/matrix", s.handleMatrix)
	s.mux.HandleFunc("GET /api/dashboard/gpu", s.handleGPU)
	s.mux.HandleFunc("POST /api/dashboard/jobs", s.handleSubmitJob)
	s.mux.HandleFunc("POST /api/dashboard/jobs/dry-run", s.handleDryRun)
	s.mux.HandleFunc("POST /api/dashboard/tasks/{id}/kill", s.handleKillTask)
	s.mux.HandleFunc("POST /api/dashboard/tasks/{id}/retry", s.handleRetryTask)
	s.mux.HandleFunc("POST /api/dashboard/jobs/{id}/kill", s.handleKillJob)
	s.mux.HandleFunc("POST /api/dashboard/jobs/{id}/pause", s.handlePauseJob)
	s.mux.HandleFunc("POST /api/dashboard/jobs/{id}/resume", s.handleResumeJob)
	s.mux.HandleFunc("GET /api/dashboard/fs/list", s.handleFSList)
	s.mux.HandleFunc("POST /api/dashboard/fs/parse-script", s.handleParseScript)
	s.mux.HandleFunc("GET /api/{path...}", s.handleAPINotFound)
	s.mux.HandleFunc("POST /api/{path...}", s.handleAPINotFound)
	s.mux.HandleFunc("PUT /api/{path...}", s.handleAPINotFound)
	s.mux.HandleFunc("DELETE /api/{path...}", s.handleAPINotFound)
	s.mux.HandleFunc("GET /", s.handleSPA)
}

func (s *Server) handleConfig(w http.ResponseWriter, r *http.Request) {
	isDaemon := s.mode == config.ModeDaemon
	writeJSON(w, http.StatusOK, ConfigResponse{
		Mode:       s.mode,
		DataPath:   s.cfg.DataPath,
		ConfigPath: config.ConfigPath(),
		Features: FeatureFlags{
			GPUMap:      isDaemon,
			PauseResume: isDaemon,
		},
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

func (s *Server) handleMatrix(w http.ResponseWriter, r *http.Request) {
	rowKey := r.URL.Query().Get("row")
	colKey := r.URL.Query().Get("col")
	valueKey := r.URL.Query().Get("val")
	if valueKey == "" {
		valueKey = r.URL.Query().Get("value")
	}
	if rowKey == "" || colKey == "" || valueKey == "" {
		writeErrorStatus(w, http.StatusBadRequest, fmt.Errorf("row, col, and val are required"))
		return
	}
	matrix, err := s.backend.EvalMatrix(r.Context(), r.PathValue("id"), rowKey, colKey, valueKey)
	if err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, matrix)
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
	jobID, total, err := s.backend.SubmitJob(r.Context(), cfg)
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
	if errors.Is(err, ErrNotSupported) {
		status = http.StatusBadRequest
	} else if isNotFound(err) {
		status = http.StatusNotFound
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
