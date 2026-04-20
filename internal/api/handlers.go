package api

import (
	"crypto/rand"
	"fmt"
	"io"
	"maps"
	"net/http"
	"slices"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/gliese129/runq/internal/job"
	"github.com/gliese129/runq/internal/project"
	"github.com/gliese129/runq/internal/scheduler"
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
		jobs.DELETE("/:id", s.handleJobKill)
	}

	// Task
	tasks := api.Group("/tasks")
	{
		tasks.GET("", s.handleTaskList)
		tasks.GET("/:id", s.handleTaskGet)
		tasks.POST("/:id/kill", s.handleTaskKill)
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
	if gpusPerTask == 0 {
		gpusPerTask = 1
	}
	maxRetry := proj.Defaults.MaxRetry
	if jobCfg.Overrides != nil {
		if jobCfg.Overrides.GPUsPerTask != nil {
			gpusPerTask = *jobCfg.Overrides.GPUsPerTask
		}
		if jobCfg.Overrides.MaxRetry != nil {
			maxRetry = *jobCfg.Overrides.MaxRetry
		}
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
			LogPath:     fmt.Sprintf("logs/%s.log", taskID),
			WorkingDir:  proj.WorkingDir,
			Env:         env,
			Resumable:   proj.Resume.Enabled,
			ExtraArgs:   proj.Resume.ExtraArgs,
		})
	}

	// Enqueue all tasks.
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

// handleJobList returns a per-job summary with status breakdown.
func (s *Server) handleJobList(c *gin.Context) {
	tasks := s.deps.Queue.FetchAll()
	type Res struct {
		JobID       string         `json:"job_id"`
		Project     string         `json:"project"`
		StatusCount map[string]int `json:"status_count"`
		CreatedAt   time.Time      `json:"created_at"`
	}
	mp := map[string]Res{}

	for _, t := range tasks {
		if _, ok := mp[t.JobID]; !ok {
			mp[t.JobID] = Res{
				JobID:   t.JobID,
				Project: t.ProjectName,
				StatusCount: map[string]int{
					"running": 0,
					"pending": 0,
					"failed":  0,
					"killed":  0,
					"success": 0,
				},
				CreatedAt: t.EnqueuedAt,
			}
			mp[t.JobID].StatusCount[string(t.Status)] = 1
		} else {
			mp[t.JobID].StatusCount[string(t.Status)]++
		}
	}

	res := slices.Collect(maps.Values(mp))
	c.JSON(http.StatusOK, res)
}

func (s *Server) handleJobKill(c *gin.Context) {
	jobID := c.Param("id")
	tasks := s.deps.Queue.ListByJob(jobID)
	if len(tasks) == 0 {
		c.JSON(http.StatusNotFound, gin.H{"error": fmt.Sprintf("job %q not found", jobID)})
		return
	}

	killed := 0
	for _, t := range tasks {
		if t.Status == scheduler.StatusRunning {
			s.deps.Executor.Stop(t.ID)
			killed++
		} else if t.Status == scheduler.StatusPending {
			s.deps.Queue.Complete(t.ID, scheduler.StatusKilled)
			killed++
		}
	}

	c.JSON(http.StatusOK, gin.H{
		"job_id":       jobID,
		"tasks_killed": killed,
	})
}

// ── Task handlers ──

func (s *Server) handleTaskList(c *gin.Context) {
	status := c.Query("status")
	jobID := c.Query("job")

	var tasks []*scheduler.Task
	if jobID != "" {
		tasks = s.deps.Queue.ListByJob(jobID)
	} else if status != "" {
		tasks = s.deps.Queue.ListByStatus(scheduler.TaskStatus(status))
	} else {
		// Default: running + pending.
		tasks = append(
			s.deps.Queue.ListByStatus(scheduler.StatusRunning),
			s.deps.Queue.ListByStatus(scheduler.StatusPending)...,
		)
	}

	c.JSON(http.StatusOK, tasks)
}

func (s *Server) handleTaskGet(c *gin.Context) {
	id := c.Param("id")
	task := s.deps.Queue.Get(id)
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
	} else {
		c.JSON(http.StatusBadRequest, gin.H{"error": fmt.Sprintf("task %q is %s, cannot kill", id, task.Status)})
		return
	}

	c.JSON(http.StatusOK, gin.H{
		"message": fmt.Sprintf("task %q killed", id),
	})
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
