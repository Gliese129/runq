package api

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"path/filepath"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/gliese129/runq/internal/job"
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
		projects.GET("/:name", s.handleProjectGet)
		projects.PUT("/:name", s.handleProjectUpdate)
		projects.DELETE("/:name", s.handleProjectDelete)
	}

	// Job
	jobs := api.Group("/jobs")
	{
		jobs.POST("", s.handleJobSubmit)
		jobs.GET("", s.handleJobList)
		jobs.GET("/:id", s.handleJobShow)
		jobs.DELETE("/:id", s.handleJobKill)
		jobs.POST("/:id/pause", s.handleJobPause)
		jobs.POST("/:id/resume", s.handleJobResume)
		jobs.POST("/:id/rm", s.handleJobRm)
	}

	// Task
	tasks := api.Group("/tasks")
	{
		tasks.GET("", s.handleTaskList)
		tasks.GET("/:id", s.handleTaskGet)
		tasks.POST("/:id/kill", s.handleTaskKill)
		tasks.POST("/:id/retry", s.handleTaskRetry)
	}

	// System
	api.GET("/gpu", s.handleGPUStatus)
	api.GET("/status", s.handleStatus)
}

// ── Project handlers ──

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
	if err := s.deps.Registry.Add(cfg); err != nil {
		c.JSON(http.StatusConflict, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusCreated, gin.H{
		"message": fmt.Sprintf("project %q registered", cfg.ProjectName),
	})
}

func (s *Server) handleProjectList(c *gin.Context) {
	configs, err := s.deps.Registry.List()
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, configs)
}

func (s *Server) handleProjectGet(c *gin.Context) {
	name := c.Param("name")
	cfg, err := s.deps.Registry.Get(name)
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
	if err := s.deps.Registry.Update(cfg); err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, gin.H{
		"message": fmt.Sprintf("project %q updated", name),
	})
}

func (s *Server) handleProjectDelete(c *gin.Context) {
	name := c.Param("name")
	if err := s.deps.Registry.Remove(name); err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, gin.H{
		"message": fmt.Sprintf("project %q removed", name),
	})
}

// ── Job handlers ──

// JobSubmitRequest is the JSON body for POST /api/jobs.
type JobSubmitRequest struct {
	JobConfig job.JobConfig `json:"job_config"`
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
		var req JobSubmitRequest
		if err := c.ShouldBindJSON(&req); err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
			return
		}
		jobCfg = req.JobConfig
	}

	// Validate project exists.
	proj, err := s.deps.Registry.Get(jobCfg.Project)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": fmt.Sprintf("project %q not found", jobCfg.Project)})
		return
	}

	// Expand sweep into parameter combinations.
	taskParams, err := job.Expand(&jobCfg)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": fmt.Sprintf("sweep expansion failed: %v", err)})
		return
	}

	// Determine effective settings (job overrides > project defaults).
	gpusPerTask := proj.Defaults.GPUsPerTask
	maxRetry := proj.Defaults.MaxRetry
	if jobCfg.Overrides != nil {
		if jobCfg.Overrides.GPUsPerTask != nil {
			gpusPerTask = *jobCfg.Overrides.GPUsPerTask
		}
		if jobCfg.Overrides.MaxRetry != nil {
			maxRetry = *jobCfg.Overrides.MaxRetry
		}
	}
	// Enforce minimum 1 GPU — 0 would skip CUDA_VISIBLE_DEVICES and expose all GPUs.
	if gpusPerTask <= 0 {
		gpusPerTask = 1
	}

	// Merge env: project env + job override env.
	env := make(map[string]string)
	for k, v := range proj.Environment {
		env[k] = v
	}
	if jobCfg.Overrides != nil {
		for k, v := range jobCfg.Overrides.Env {
			env[k] = v
		}
	}

	// Generate job ID and build task list.
	jobID := generateID()
	tasks := make([]*scheduler.Task, 0, len(taskParams))
	for _, params := range taskParams {
		cmd, err := job.Render(proj.CmdTemplate, params)
		if err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": fmt.Sprintf("render command failed: %v", err)})
			return
		}
		taskID := generateID()
		tasks = append(tasks, &scheduler.Task{
			ID:          taskID,
			JobID:       jobID,
			ProjectName: proj.ProjectName,
			Command:     cmd,
			Params:      params,
			GPUsNeeded:  gpusPerTask,
			MaxRetry:    maxRetry,
			LogPath:     filepath.Join(proj.WorkingDir, "logs", taskID+".log"),
			WorkingDir:  proj.WorkingDir,
			Env:         env,
			Resumable:   proj.Resume.Enabled,
			ExtraArgs:   proj.Resume.ExtraArgs,
		})
	}

	// Persist job + all tasks to DB atomically (single transaction).
	// If DB write fails, we do NOT push to Queue — no partial state.
	ctx := context.Background()
	cfgJSON, err := json.Marshal(jobCfg)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": fmt.Sprintf("failed to marshal job config: %v", err)})
		return
	}

	now := time.Now()
	jobRow := store.JobRow{
		ID:          jobID,
		ProjectName: jobCfg.Project,
		Description: jobCfg.Description,
		ConfigJSON:  string(cfgJSON),
		Status:      "pending",
		TotalTasks:  len(tasks),
		CreatedAt:   now,
	}

	// Convert scheduler.Task → store.TaskRow for persistence.
	taskRows := make([]store.TaskRow, 0, len(tasks))
	for _, t := range tasks {
		paramsJSON, _ := json.Marshal(t.Params)
		envJSON, _ := json.Marshal(t.Env)
		taskRows = append(taskRows, store.TaskRow{
			ID:          t.ID,
			JobID:       t.JobID,
			ProjectName: t.ProjectName,
			Command:     t.Command,
			ParamsJSON:  string(paramsJSON),
			GPUsNeeded:  t.GPUsNeeded,
			Status:      "pending",
			MaxRetry:    t.MaxRetry,
			LogPath:     t.LogPath,
			WorkingDir:  t.WorkingDir,
			EnvJSON:     string(envJSON),
			Resumable:   t.Resumable,
			ExtraArgs:   t.ExtraArgs,
			EnqueuedAt:  now,
		})
	}

	// Atomic insert: job + tasks in one transaction.
	// On failure the entire batch rolls back — no orphan job rows.
	if err := s.deps.Store.InsertJobWithTasks(ctx, &jobRow, taskRows); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": fmt.Sprintf("failed to persist job: %v", err)})
		return
	}

	// DB succeeded — now push to in-memory Queue for scheduling.
	s.deps.Queue.PushBatch(tasks)

	s.logger.Info("job submitted",
		"job_id", jobID,
		"project", proj.ProjectName,
		"tasks", len(tasks),
	)

	c.JSON(http.StatusCreated, gin.H{
		"job_id":      jobID,
		"total_tasks": len(tasks),
	})
}

// handleJobList returns jobs from DB with per-task status breakdown.
func (s *Server) handleJobList(c *gin.Context) {
	ctx := context.Background()
	jobs, err := s.deps.Store.ListJobs(ctx, c.Query("project"))
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}

	type JobSummary struct {
		JobID       string         `json:"job_id"`
		Project     string         `json:"project"`
		Status      string         `json:"status"`
		TotalTasks  int            `json:"total_tasks"`
		StatusCount map[string]int `json:"status_count"`
		CreatedAt   time.Time      `json:"created_at"`
	}

	results := make([]JobSummary, 0, len(jobs))
	for _, j := range jobs {
		// Aggregate task statuses for this job.
		tasks, _ := s.deps.Store.ListTasks(ctx, store.TaskFilter{JobID: j.ID})
		counts := map[string]int{"pending": 0, "running": 0, "success": 0, "failed": 0, "killed": 0}
		for _, t := range tasks {
			counts[t.Status]++
		}
		results = append(results, JobSummary{
			JobID:       j.ID,
			Project:     j.ProjectName,
			Status:      j.Status,
			TotalTasks:  j.TotalTasks,
			StatusCount: counts,
			CreatedAt:   j.CreatedAt,
		})
	}
	c.JSON(http.StatusOK, results)
}

func (s *Server) handleJobKill(c *gin.Context) {
	jobID := c.Param("id")
	tasks := s.deps.Queue.ListByJob(jobID)
	if len(tasks) == 0 {
		c.JSON(http.StatusNotFound, gin.H{"error": fmt.Sprintf("job %q not found", jobID)})
		return
	}

	ctx := context.Background()
	killed := 0
	for _, t := range tasks {
		if t.Status == scheduler.StatusRunning {
			s.deps.Executor.Stop(t.ID)
			killed++
		} else if t.Status == scheduler.StatusPending {
			s.deps.Queue.Complete(t.ID, scheduler.StatusKilled)
			// Persist to DB so the kill survives daemon restart.
			_ = s.deps.Store.UpdateTaskStatus(ctx, t.ID, "killed", map[string]any{
				"finished_at": time.Now().Unix(),
			})
			killed++
		}
	}

	c.JSON(http.StatusOK, gin.H{
		"job_id":       jobID,
		"tasks_killed": killed,
	})
}

// handleJobShow returns job details along with all its tasks.
func (s *Server) handleJobShow(c *gin.Context) {
	ctx := context.Background()
	jobID := c.Param("id")

	job, err := s.deps.Store.GetJob(ctx, jobID)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	if job == nil {
		c.JSON(http.StatusNotFound, gin.H{"error": fmt.Sprintf("job %q not found", jobID)})
		return
	}

	tasks, err := s.deps.Store.ListTasks(ctx, store.TaskFilter{JobID: jobID})
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}

	c.JSON(http.StatusOK, gin.H{"job": job, "tasks": tasks})
}

// handleJobPause pauses a job — scheduler skips its pending tasks.
func (s *Server) handleJobPause(c *gin.Context) {
	ctx := context.Background()
	jobID := c.Param("id")

	job, err := s.deps.Store.GetJob(ctx, jobID)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	if job == nil {
		c.JSON(http.StatusNotFound, gin.H{"error": fmt.Sprintf("job %q not found", jobID)})
		return
	}
	if job.Status == "done" {
		c.JSON(http.StatusBadRequest, gin.H{"error": fmt.Sprintf("job %q is already done", jobID)})
		return
	}

	s.deps.Scheduler.PauseJob(jobID)
	if err := s.deps.Store.UpdateJobStatus(ctx, jobID, "paused"); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, gin.H{"message": fmt.Sprintf("job %q paused", jobID)})
}

// handleJobResume resumes a paused job.
func (s *Server) handleJobResume(c *gin.Context) {
	ctx := context.Background()
	jobID := c.Param("id")

	job, err := s.deps.Store.GetJob(ctx, jobID)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	if job == nil {
		c.JSON(http.StatusNotFound, gin.H{"error": fmt.Sprintf("job %q not found", jobID)})
		return
	}
	if job.Status != "paused" {
		c.JSON(http.StatusBadRequest, gin.H{"error": fmt.Sprintf("job %q is %s, not paused", jobID, job.Status)})
		return
	}

	s.deps.Scheduler.ResumeJob(jobID)
	if err := s.deps.Store.UpdateJobStatus(ctx, jobID, "running"); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, gin.H{"message": fmt.Sprintf("job %q resumed", jobID)})
}

// handleJobRm removes a completed job and its tasks from DB.
func (s *Server) handleJobRm(c *gin.Context) {
	ctx := context.Background()
	jobID := c.Param("id")

	job, err := s.deps.Store.GetJob(ctx, jobID)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	if job == nil {
		c.JSON(http.StatusNotFound, gin.H{"error": fmt.Sprintf("job %q not found", jobID)})
		return
	}
	if job.Status != "done" {
		c.JSON(http.StatusBadRequest, gin.H{"error": fmt.Sprintf("job %q is %s, only completed jobs can be removed", jobID, job.Status)})
		return
	}

	if err := s.deps.Store.DeleteJob(ctx, jobID); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, gin.H{"message": fmt.Sprintf("job %q removed", jobID)})
}

// ── Task handlers ──

// handleTaskList queries tasks from DB. Supports ?status= and ?job= filters.
// Default (no filter): returns running + pending tasks.
func (s *Server) handleTaskList(c *gin.Context) {
	ctx := context.Background()
	status := c.Query("status")
	jobID := c.Query("job")

	filter := store.TaskFilter{Status: status, JobID: jobID}
	// Default: show active tasks only.
	if status == "" && jobID == "" {
		filter.Status = "" // ListTasks with empty filter returns all; we want active only
	}

	tasks, err := s.deps.Store.ListTasks(ctx, filter)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}

	// If no explicit filter, return only active tasks (backward compatible).
	if status == "" && jobID == "" {
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

// handleTaskGet returns a single task from DB with all fields.
func (s *Server) handleTaskGet(c *gin.Context) {
	ctx := context.Background()
	id := c.Param("id")
	task, err := s.deps.Store.GetTask(ctx, id)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	if task == nil {
		c.JSON(http.StatusNotFound, gin.H{"error": fmt.Sprintf("task %q not found", id)})
		return
	}
	c.JSON(http.StatusOK, task)
}

func (s *Server) handleTaskKill(c *gin.Context) {
	id := c.Param("id")
	task := s.deps.Queue.Get(id)
	if task == nil {
		c.JSON(http.StatusNotFound, gin.H{"error": fmt.Sprintf("task %q not found", id)})
		return
	}

	if task.Status == scheduler.StatusRunning {
		s.deps.Executor.Stop(id)
	} else if task.Status == scheduler.StatusPending {
		s.deps.Queue.Complete(id, scheduler.StatusKilled)
		_ = s.deps.Store.UpdateTaskStatus(context.Background(), id, "killed", map[string]any{
			"finished_at": time.Now().Unix(),
		})
	} else {
		c.JSON(http.StatusBadRequest, gin.H{"error": fmt.Sprintf("task %q is %s, cannot kill", id, task.Status)})
		return
	}

	c.JSON(http.StatusOK, gin.H{
		"message": fmt.Sprintf("task %q killed", id),
	})
}

// handleTaskRetry re-enqueues a failed or killed task for another attempt.
func (s *Server) handleTaskRetry(c *gin.Context) {
	ctx := context.Background()
	id := c.Param("id")

	row, err := s.deps.Store.GetTask(ctx, id)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	if row == nil {
		c.JSON(http.StatusNotFound, gin.H{"error": fmt.Sprintf("task %q not found", id)})
		return
	}
	if row.Status != "failed" && row.Status != "killed" {
		c.JSON(http.StatusBadRequest, gin.H{"error": fmt.Sprintf("task %q is %s, only failed/killed tasks can be retried", id, row.Status)})
		return
	}

	// Reset task state in DB.
	if err := s.deps.Store.UpdateTaskStatus(ctx, id, "pending", map[string]any{
		"gpus":        nil,
		"pid":         nil,
		"started_at":  nil,
		"finished_at": nil,
	}); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}

	// Re-read the updated row and push to Queue.
	row, _ = s.deps.Store.GetTask(ctx, id)
	task := taskRowToSchedulerTask(row)
	s.deps.Queue.Push(task)

	c.JSON(http.StatusOK, gin.H{"message": fmt.Sprintf("task %q re-enqueued", id)})
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

// generateID returns a short, human-readable unique ID (hex timestamp + random suffix).
// Panics if the OS entropy source is unavailable.
func generateID() string {
	ts := time.Now().Unix()
	rd := make([]byte, 4) // 4 bytes = 8 hex chars
	_, err := rand.Read(rd)
	if err != nil {
		panic("Cannot generate random number! Please consider restart the daemon")
	}

	// Simple hex format: timestamp + random suffix.
	return fmt.Sprintf("%x%x", ts, rd)
}
