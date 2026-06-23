package api

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/gliese129/runq/internal/backend"
	"github.com/gliese129/runq/internal/job"
	"github.com/gliese129/runq/internal/logfile"
	"github.com/gliese129/runq/internal/project"
	"github.com/gliese129/runq/internal/scheduler"
	"github.com/gliese129/runq/internal/service"
	"github.com/gliese129/runq/internal/store"
	"gopkg.in/yaml.v3"
)

// registerRoutes wires up all API endpoints with route grouping.
func (s *Server) registerRoutes() {
	api := s.router.Group("/api")

	// Project
	projects := api.Group("/projects")
	{
		projects.POST("", s.handleProjectAdd)
		projects.GET("", s.handleProjectList)
		projects.GET("/match", s.handleProjectMatch) // before /:name
		projects.GET("/:name", s.handleProjectGet)
		projects.PUT("/:name", s.handleProjectUpdate)
		projects.POST("/:name/rename", s.handleProjectRename)
		// Static segment alongside /:name — gin's radix tree gives static
		// routes precedence over params regardless of registration order
		// (same coexistence as /match above).
		projects.GET("/summaries", s.handleProjectSummaries)
		projects.POST("/:name/archive", s.handleProjectArchive)
		projects.POST("/:name/unarchive", s.handleProjectUnarchive)
		projects.DELETE("/:name", s.handleProjectDelete)
	}

	// Job
	jobs := api.Group("/jobs")
	{
		jobs.POST("", s.handleJobSubmit)
		jobs.POST("/resolve-note", s.handleResolveNote)
		jobs.GET("", s.handleJobList)
		jobs.GET("/:id", s.handleJobShow)
		jobs.DELETE("/:id", s.handleJobKill)
		jobs.POST("/:id/pause", s.handleJobPause)
		jobs.POST("/:id/archive", s.handleJobArchive)
		jobs.POST("/:id/unarchive", s.handleJobUnarchive)
		jobs.POST("/:id/resume", s.handleJobResume)
		jobs.POST("/:id/rm", s.handleJobRm)
	}

	// Task
	tasks := api.Group("/tasks")
	{
		tasks.GET("", s.handleTaskList)
		tasks.GET("/:id", s.handleTaskGet)
		tasks.GET("/:id/log", s.handleTaskLog)
		tasks.POST("/:id/kill", s.handleTaskKill)
		tasks.POST("/:id/retry", s.handleTaskRetry)
	}

	// System
	api.GET("/gpu", s.handleGPUStatus)
	api.GET("/status", s.handleStatus)
	api.POST("/thaw", s.handleThaw)

	// Internal — SDK-only control plane. Not for human / CLI use.
	// Auth model matches Linux file permissions on the unix socket: any
	// process under the daemon's UID can call these. We don't try to be
	// stronger than the OS.
	internal := api.Group("/internal")
	{
		internal.POST("/freeze-self", s.handleFreezeSelf)
	}

	// Log SSE
	tasks.GET("/:id/log/stream", s.handleTaskLogStream)
}

// ── Project handlers (thin — Registry is already a clean service) ──

func (s *Server) handleProjectAdd(c *gin.Context) {
	var cfg project.Config
	if err := c.ShouldBindJSON(&cfg); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	if cfg.ProjectName == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "project_name is required"})
		return
	}
	if err := s.deps.Registry.Add(c.Request.Context(), cfg); err != nil {
		status := http.StatusBadRequest
		if strings.Contains(err.Error(), "already exists") {
			status = http.StatusConflict
		}
		c.JSON(status, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusCreated, gin.H{"message": fmt.Sprintf("project %q registered", cfg.ProjectName)})
}

func (s *Server) handleProjectMatch(c *gin.Context) {
	dir := c.Query("dir")
	if dir == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "dir query parameter is required"})
		return
	}
	configs, err := s.deps.Registry.Match(c.Request.Context(), dir)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	if configs == nil {
		configs = []project.Config{}
	}
	c.JSON(http.StatusOK, configs)
}

func (s *Server) handleProjectList(c *gin.Context) {
	configs, err := s.deps.Registry.List(c.Request.Context())
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, configs)
}

func (s *Server) handleProjectGet(c *gin.Context) {
	cfg, err := s.deps.Registry.Get(c.Request.Context(), c.Param("name"))
	if err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, cfg)
}

func (s *Server) handleProjectUpdate(c *gin.Context) {
	name := c.Param("name")
	var cfg project.Config
	if err := c.ShouldBindJSON(&cfg); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	cfg.ProjectName = name
	if err := s.deps.Registry.Update(c.Request.Context(), cfg); err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, gin.H{"message": fmt.Sprintf("project %q updated", name)})
}

func (s *Server) handleProjectRename(c *gin.Context) {
	oldName := c.Param("name")
	var body struct {
		NewName string `json:"new_name" binding:"required"`
	}
	if err := c.ShouldBindJSON(&body); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "new_name is required"})
		return
	}
	if err := s.deps.Registry.Rename(c.Request.Context(), oldName, body.NewName); err != nil {
		status := http.StatusBadRequest
		if strings.Contains(err.Error(), "not found") {
			status = http.StatusNotFound
		} else if strings.Contains(err.Error(), "already exists") {
			status = http.StatusConflict
		}
		c.JSON(status, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, gin.H{"message": fmt.Sprintf("project %q renamed to %q", oldName, body.NewName)})
}

func (s *Server) handleProjectDelete(c *gin.Context) {
	if err := s.deps.Registry.Remove(c.Request.Context(), c.Param("name")); err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, gin.H{"message": fmt.Sprintf("project %q removed", c.Param("name"))})
}

// ── Job handlers (delegate to JobService) ──

// handleResolveNote previews a note template's resolution ({{version}} scan
// included) without submitting — same code path as submit (U1).
func (s *Server) handleResolveNote(c *gin.Context) {
	var jobCfg job.JobConfig
	if err := c.ShouldBindJSON(&jobCfg); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	resolved, err := s.deps.JobService.ResolveNote(c.Request.Context(), jobCfg)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, gin.H{"resolved": resolved})
}

func (s *Server) handleJobSubmit(c *gin.Context) {
	// Parse job config — support both YAML and JSON.
	var jobCfg job.JobConfig
	if ct := c.ContentType(); ct == "application/x-yaml" || ct == "text/yaml" {
		body, err := io.ReadAll(c.Request.Body)
		if err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": "failed to read body"})
			return
		}
		if err := yaml.Unmarshal(body, &jobCfg); err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": fmt.Sprintf("invalid YAML: %v", err)})
			return
		}
	} else {
		// Accept both {"job_config": {...}} (wrapped) and {...} (raw JobConfig).
		body, err := io.ReadAll(c.Request.Body)
		if err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": "failed to read body"})
			return
		}
		// Try wrapped format first.
		var wrapped struct {
			JobConfig job.JobConfig `json:"job_config"`
		}
		if err := json.Unmarshal(body, &wrapped); err == nil && wrapped.JobConfig.Project != "" {
			jobCfg = wrapped.JobConfig
		} else {
			// Fall back to raw JobConfig.
			if err := json.Unmarshal(body, &jobCfg); err != nil {
				c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
				return
			}
		}
	}

	// F8 preflight bypass — CLI's --no-preflight propagates as the
	// ``no_preflight`` query parameter on POST /api/jobs.
	opts := service.SubmitJobOpts{
		SkipPreflight: c.Query("no_preflight") == "1",
	}
	jobID, taskCount, err := s.deps.JobService.SubmitJobWithOpts(c.Request.Context(), jobCfg, opts)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}

	s.logger.Info("job submitted", "job_id", jobID, "tasks", taskCount)

	resp := gin.H{"job_id": jobID, "total_tasks": taskCount}
	if s.deps.Pool != nil {
		resp["free_gpus"] = s.deps.Pool.FreeCount()
		resp["total_gpus"] = s.deps.Pool.TotalCount()
	}
	c.JSON(http.StatusCreated, resp)
}

func (s *Server) handleJobList(c *gin.Context) {
	var results []backend.JobSummary
	var err error
	if c.Query("archived") == "1" {
		results, err = s.deps.JobService.ListArchivedJobs(c.Request.Context(), c.Query("project"))
	} else {
		results, err = s.deps.JobService.ListJobs(c.Request.Context(), c.Query("project"))
	}
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, results)
}

func (s *Server) handleJobArchive(c *gin.Context) {
	if err := s.deps.JobService.ArchiveJob(c.Request.Context(), c.Param("id")); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, gin.H{"message": "archived"})
}

func (s *Server) handleJobUnarchive(c *gin.Context) {
	if err := s.deps.JobService.UnarchiveJob(c.Request.Context(), c.Param("id")); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, gin.H{"message": "unarchived"})
}

// handleProjectSummaries returns server-assembled project summaries
// (job_count + archived). /api/projects stays plain []project.Config
// (yaml-truth material) for the CLI; this is the dashboard's listing.
func (s *Server) handleProjectSummaries(c *gin.Context) {
	out, err := s.deps.JobService.ProjectSummaries(c.Request.Context())
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, out)
}

func (s *Server) handleProjectArchive(c *gin.Context) {
	if err := s.deps.Registry.Archive(c.Request.Context(), c.Param("name")); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, gin.H{"message": "archived"})
}

func (s *Server) handleProjectUnarchive(c *gin.Context) {
	if err := s.deps.Registry.Unarchive(c.Request.Context(), c.Param("name")); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, gin.H{"message": "unarchived"})
}

func (s *Server) handleJobShow(c *gin.Context) {
	detail, err := s.deps.JobService.ShowJob(c.Request.Context(), c.Param("id"))
	if err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, detail)
}

func (s *Server) handleJobKill(c *gin.Context) {
	killed, err := s.deps.JobService.KillJob(c.Request.Context(), c.Param("id"))
	if err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, gin.H{"job_id": c.Param("id"), "tasks_killed": killed})
}

func (s *Server) handleJobPause(c *gin.Context) {
	if err := s.deps.JobService.PauseJob(c.Request.Context(), c.Param("id")); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, gin.H{"message": fmt.Sprintf("job %q paused", c.Param("id"))})
}

func (s *Server) handleJobResume(c *gin.Context) {
	if err := s.deps.JobService.ResumeJob(c.Request.Context(), c.Param("id")); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, gin.H{"message": fmt.Sprintf("job %q resumed", c.Param("id"))})
}

func (s *Server) handleJobRm(c *gin.Context) {
	if err := s.deps.JobService.RemoveJob(c.Request.Context(), c.Param("id")); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, gin.H{"message": fmt.Sprintf("job %q removed", c.Param("id"))})
}

// ── Task handlers (delegate to TaskService for mutations) ──

func (s *Server) handleTaskList(c *gin.Context) {
	ctx := c.Request.Context()
	status := c.Query("status")
	jobID := c.Query("job")
	includeAll := status == "all"
	if includeAll {
		status = ""
	}

	tasks, err := s.deps.Store.ListTasks(ctx, store.TaskFilter{Status: status, JobID: jobID})
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}

	// Default: return only active tasks.
	if status == "" && jobID == "" && !includeAll {
		active := make([]store.TaskRow, 0, len(tasks))
		for _, t := range tasks {
			if t.Status == "pending" || t.Status == "running" {
				active = append(active, t)
			}
		}
		tasks = active
	}

	c.JSON(http.StatusOK, tasks)
}

func (s *Server) handleTaskGet(c *gin.Context) {
	task, err := s.deps.Store.GetTask(c.Request.Context(), c.Param("id"))
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	if task == nil {
		c.JSON(http.StatusNotFound, gin.H{"error": fmt.Sprintf("task %q not found", c.Param("id"))})
		return
	}
	c.JSON(http.StatusOK, task)
}

// handleTaskLog reads a page of log lines starting at a byte offset.
// Query params:
//   - offset: byte offset into the raw log file (default 0)
//   - lines:  number of lines to return (default 200, max 5000)
//
// Returns a logfile.Page JSON.
func (s *Server) handleTaskLog(c *gin.Context) {
	task, err := s.deps.Store.GetTask(c.Request.Context(), c.Param("id"))
	if err != nil || task == nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "task not found"})
		return
	}
	logPath := task.LogPath
	if logPath == "" {
		c.JSON(http.StatusOK, &logfile.Page{Lines: []string{}})
		return
	}

	offset := int64(0)
	if v := c.Query("offset"); v != "" {
		if n, err := strconv.ParseInt(v, 10, 64); err == nil && n >= 0 {
			offset = n
		}
	}
	lines := logfile.DefaultPageLines
	if v := c.Query("lines"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 && n <= logfile.MaxPageLines {
			lines = n
		}
	}

	lr, err := logfile.Open(logPath)
	if err != nil {
		c.JSON(http.StatusOK, &logfile.Page{Lines: []string{}})
		return
	}
	defer lr.Close()

	page, err := lr.ReadLines(offset, lines)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, page)
}

func (s *Server) handleTaskLogStream(c *gin.Context) {
	task, err := s.deps.Store.GetTask(c.Request.Context(), c.Param("id"))
	if err != nil || task == nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "task not found"})
		return
	}
	logPath := task.LogPath
	if logPath == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "no log file"})
		return
	}

	w := c.Writer
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming not supported", http.StatusInternalServerError)
		return
	}

	offset := int64(0)
	if v := c.Query("offset"); v != "" {
		if n, err := strconv.ParseInt(v, 10, 64); err == nil && n >= 0 {
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
		case <-c.Request.Context().Done():
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

func (s *Server) handleTaskKill(c *gin.Context) {
	if err := s.deps.TaskService.KillTask(c.Request.Context(), c.Param("id")); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, gin.H{"message": fmt.Sprintf("task %q killed", c.Param("id"))})
}

func (s *Server) handleTaskRetry(c *gin.Context) {
	if err := s.deps.TaskService.RetryTask(c.Request.Context(), c.Param("id")); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, gin.H{"message": fmt.Sprintf("task %q re-enqueued", c.Param("id"))})
}

// ── System handlers ──

func (s *Server) handleGPUStatus(c *gin.Context) {
	c.JSON(http.StatusOK, s.deps.Pool.Status())
}

func (s *Server) handleStatus(c *gin.Context) {
	c.JSON(http.StatusOK, gin.H{
		"running":   len(s.deps.Queue.ListByStatus(scheduler.StatusRunning)),
		"pending":   s.deps.Queue.PendingCount(),
		"gpus_free": s.deps.Pool.FreeCount(),
	})
}

// ── Freeze / Thaw lives in handlers_freeze.go ──
//
// Types (FreezeSelfReq / ThawResponse / BlockedDetail / MountMate) and
// the corresponding handlers (handleFreezeSelf / handleThaw + helpers
// scopeOwned / collectMountMates) are in a sibling file so this file
// stays focused on the boring REST CRUD layer.
