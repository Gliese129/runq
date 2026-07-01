package api

import (
	"encoding/json"
	"errors"
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
	}

	// Job
	jobs := api.Group("/jobs")
	{
		jobs.POST("", s.handleJobSubmit)
		jobs.POST("/preview", s.handleJobPreview)
		jobs.POST("/resolve-note", s.handleResolveNote)
		jobs.GET("", s.handleJobList)
		jobs.GET("/:id", s.handleJobShow)
		jobs.DELETE("/:id", s.handleJobKill)
		jobs.POST("/:id/pause", s.handleJobPause)
		jobs.POST("/:id/archive", s.handleJobArchive)
		jobs.POST("/:id/unarchive", s.handleJobUnarchive)
		jobs.POST("/:id/resume", s.handleJobResume)
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
	api.POST("/clean", s.handleClean)

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

// ── Job handlers (delegate to JobService) ──

// handleResolveNote previews a note template's resolution ({{version}} scan
// included) without submitting — same code path as submit (U1).
func (s *Server) handleResolveNote(c *gin.Context) {
	var jobCfg job.JobConfig
	if err := c.ShouldBindJSON(&jobCfg); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	resolved, err := s.deps.Local.ResolveNote(c.Request.Context(), jobCfg)
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
	skipPreflight := c.Query("no_preflight") == "1"
	target := c.Query("target") // empty = default target

	opts := backend.SubmitOptions{
		SkipPreflight: skipPreflight,
		Target:        target,
	}
	jobID, taskCount, err := s.deps.Multi.SubmitJob(c.Request.Context(), jobCfg, opts)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}

	s.logger.Info("job submitted", "job_id", jobID, "tasks", taskCount, "target", target)

	resp := gin.H{"job_id": jobID, "total_tasks": taskCount}
	if s.deps.Pool != nil {
		resp["free_gpus"] = s.deps.Pool.FreeCount()
		resp["total_gpus"] = s.deps.Pool.TotalCount()
	}
	c.JSON(http.StatusCreated, resp)
}

// handleJobPreview renders what `submit --dry-run` would produce, routed
// to the correct target backend. HPC targets return a full preview
// (run.sh + submit command); local targets return ErrNotSupported and the
// handler falls back to task-expansion dry-run.
//
// POST /api/jobs/preview?target=X&no_preflight=1
func (s *Server) handleJobPreview(c *gin.Context) {
	body, err := io.ReadAll(c.Request.Body)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "failed to read body"})
		return
	}
	var jobCfg job.JobConfig
	if err := json.Unmarshal(body, &jobCfg); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}

	target := c.Query("target")
	skipPreflight := c.Query("no_preflight") == "1"

	// Try HPC-style full preview first.
	preview, err := s.deps.Multi.PreviewSubmitForTarget(c.Request.Context(), target, jobCfg, skipPreflight)
	if err == nil {
		c.JSON(http.StatusOK, gin.H{"supported": true, "preview": preview})
		return
	}
	if !errors.Is(err, backend.ErrNotSupported) {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}

	// Fall back to task expansion dry-run.
	result, err := s.deps.Multi.DryRunForTarget(c.Request.Context(), target, jobCfg)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, gin.H{"supported": false, "dry_run": result})
}

func (s *Server) handleJobList(c *gin.Context) {
	ctx := c.Request.Context()
	var results []backend.JobSummary
	var err error
	projectScope := c.Query("project")
	targetScope := c.Query("target")

	if c.Query("archived") == "1" {
		if targetScope != "" {
			results, err = s.deps.Multi.ListArchivedJobsForTarget(ctx, targetScope)
		} else {
			results, err = s.deps.Multi.ListArchivedJobs(ctx)
		}
		// Post-filter by project — the Backend interface doesn't accept a
		// project parameter for archived listings.
		if err == nil && projectScope != "" {
			filtered := results[:0]
			for _, j := range results {
				if j.Project == projectScope {
					filtered = append(filtered, j)
				}
			}
			results = filtered
		}
	} else if targetScope != "" {
		results, err = s.deps.Multi.ListJobsForTarget(ctx, targetScope, projectScope)
	} else {
		results, err = s.deps.Multi.ListJobs(ctx, projectScope)
	}
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, results)
}

func (s *Server) handleJobArchive(c *gin.Context) {
	if err := s.deps.Multi.ArchiveJob(c.Request.Context(), c.Param("id")); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, gin.H{"message": "archived"})
}

func (s *Server) handleJobUnarchive(c *gin.Context) {
	if err := s.deps.Multi.UnarchiveJob(c.Request.Context(), c.Param("id")); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, gin.H{"message": "unarchived"})
}

// handleProjectSummaries returns server-assembled project summaries
// (job_count + archived). /api/projects stays plain []project.Config
// (yaml-truth material) for the CLI; this is the dashboard's listing.
func (s *Server) handleProjectSummaries(c *gin.Context) {
	out, err := s.deps.Multi.ListProjects(c.Request.Context())
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
	detail, err := s.deps.Multi.GetJob(c.Request.Context(), c.Param("id"))
	if err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, detail)
}

func (s *Server) handleJobKill(c *gin.Context) {
	if err := s.deps.Multi.KillJob(c.Request.Context(), c.Param("id")); err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, gin.H{"job_id": c.Param("id")})
}

func (s *Server) handleJobPause(c *gin.Context) {
	if err := s.deps.Multi.PauseJob(c.Request.Context(), c.Param("id")); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, gin.H{"message": fmt.Sprintf("job %q paused", c.Param("id"))})
}

func (s *Server) handleJobResume(c *gin.Context) {
	if err := s.deps.Multi.ResumeJob(c.Request.Context(), c.Param("id")); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, gin.H{"message": fmt.Sprintf("job %q resumed", c.Param("id"))})
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
	if err := s.deps.Multi.KillTask(c.Request.Context(), c.Param("id")); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, gin.H{"message": fmt.Sprintf("task %q killed", c.Param("id"))})
}

func (s *Server) handleTaskRetry(c *gin.Context) {
	if err := s.deps.Multi.RetryTask(c.Request.Context(), c.Param("id")); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, gin.H{"message": fmt.Sprintf("task %q re-enqueued", c.Param("id"))})
}

// ── System handlers ──

func (s *Server) handleGPUStatus(c *gin.Context) {
	gpus := s.deps.Pool.Status()

	// Collect unique occupied task IDs for a single batch lookup.
	var taskIDs []string
	seen := make(map[string]bool)
	for _, g := range gpus {
		if g.TaskID != "" && !seen[g.TaskID] {
			seen[g.TaskID] = true
			taskIDs = append(taskIDs, g.TaskID)
		}
	}

	// Enrich with job_id via one DB query instead of N per-task lookups.
	type enrichedGPU struct {
		Index    int    `json:"index"`
		Name     string `json:"name"`
		MemTotal int    `json:"mem_total"`
		MemFree  int    `json:"mem_free"`
		UtilPct  int    `json:"util_pct"`
		TaskID   string `json:"task_id,omitempty"`
		JobID    string `json:"job_id,omitempty"`
	}

	jobMap, _ := s.deps.Store.GetJobIDsForTasks(c.Request.Context(), taskIDs)
	if jobMap == nil {
		jobMap = make(map[string]string)
	}

	out := make([]enrichedGPU, 0, len(gpus))
	for _, g := range gpus {
		out = append(out, enrichedGPU{
			Index:    g.Index,
			Name:     g.Name,
			MemTotal: g.MemTotal,
			MemFree:  g.MemFree,
			UtilPct:  g.UtilPct,
			TaskID:   g.TaskID,
			JobID:    jobMap[g.TaskID],
		})
	}
	c.JSON(http.StatusOK, out)
}

func (s *Server) handleStatus(c *gin.Context) {
	c.JSON(http.StatusOK, gin.H{
		"running":   len(s.deps.Queue.ListByStatus(scheduler.StatusRunning)),
		"pending":   s.deps.Queue.PendingCount(),
		"gpus_free": s.deps.Pool.FreeCount(),
	})
}

// handleClean invokes PerformClean directly (not via a Backend method) because
// the daemon IS the store owner. The CLI's api.Proxy forwards to this
// handler over the Unix socket — inserting a service layer would just add
// indirection.
func (s *Server) handleClean(c *gin.Context) {
	opts := backend.CleanOptions{
		DryRun:   c.Query("dry_run") == "true",
		Orphan:   c.Query("orphan") == "true",
		Archived: c.Query("archived") == "true",
		JobID:    c.Query("job"),
		TaskID:   c.Query("task"),
		Target:   c.Query("target"),
		CkptOnly: c.Query("ckpt_only") == "true",
	}

	if cutoffStr := c.Query("cutoff"); cutoffStr != "" {
		cutoffUnix, err := strconv.ParseInt(cutoffStr, 10, 64)
		if err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": "invalid cutoff timestamp"})
			return
		}
		// Sanity: cutoff must be in the past and no earlier than 2020-01-01.
		now := time.Now().Unix()
		const minCutoff = 1577836800 // 2020-01-01 00:00:00 UTC
		if cutoffUnix > now {
			c.JSON(http.StatusBadRequest, gin.H{"error": "cutoff must be in the past"})
			return
		}
		if cutoffUnix < minCutoff {
			c.JSON(http.StatusBadRequest, gin.H{"error": "cutoff too far in the past (before 2020)"})
			return
		}
		t := time.Unix(cutoffUnix, 0)
		opts.OlderThan = &t
	}

	// At least one selector must be present.
	if !opts.Orphan && !opts.Archived && opts.JobID == "" && opts.TaskID == "" && opts.OlderThan == nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "at least one selector required (cutoff, orphan, archived, job, task)"})
		return
	}

	result, err := backend.PerformClean(c.Request.Context(), s.deps.Store, opts)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, result)
}

// ── Freeze / Thaw lives in handlers_freeze.go ──
//
// Types (FreezeSelfReq / ThawResponse / BlockedDetail / MountMate) and
// the corresponding handlers (handleFreezeSelf / handleThaw + helpers
// scopeOwned / collectMountMates) are in a sibling file so this file
// stays focused on the boring REST CRUD layer.
