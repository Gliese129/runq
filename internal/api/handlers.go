package api

import (
	"context"
	"fmt"
	"io"
	"net/http"

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
	if err := s.deps.Registry.Add(cfg); err != nil {
		c.JSON(http.StatusConflict, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusCreated, gin.H{"message": fmt.Sprintf("project %q registered", cfg.ProjectName)})
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
	cfg, err := s.deps.Registry.Get(c.Param("name"))
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
	c.JSON(http.StatusOK, gin.H{"message": fmt.Sprintf("project %q updated", name)})
}

func (s *Server) handleProjectDelete(c *gin.Context) {
	if err := s.deps.Registry.Remove(c.Param("name")); err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, gin.H{"message": fmt.Sprintf("project %q removed", c.Param("name"))})
}

// ── Job handlers (delegate to JobService) ──

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
		var req struct {
			JobConfig job.JobConfig `json:"job_config"`
		}
		if err := c.ShouldBindJSON(&req); err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
			return
		}
		jobCfg = req.JobConfig
	}

	jobID, taskCount, err := s.deps.JobService.SubmitJob(context.Background(), jobCfg)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}

	s.logger.Info("job submitted", "job_id", jobID, "tasks", taskCount)
	c.JSON(http.StatusCreated, gin.H{"job_id": jobID, "total_tasks": taskCount})
}

func (s *Server) handleJobList(c *gin.Context) {
	results, err := s.deps.JobService.ListJobs(context.Background(), c.Query("project"))
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, results)
}

func (s *Server) handleJobShow(c *gin.Context) {
	detail, err := s.deps.JobService.ShowJob(context.Background(), c.Param("id"))
	if err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, detail)
}

func (s *Server) handleJobKill(c *gin.Context) {
	killed, err := s.deps.JobService.KillJob(context.Background(), c.Param("id"))
	if err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, gin.H{"job_id": c.Param("id"), "tasks_killed": killed})
}

func (s *Server) handleJobPause(c *gin.Context) {
	if err := s.deps.JobService.PauseJob(context.Background(), c.Param("id")); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, gin.H{"message": fmt.Sprintf("job %q paused", c.Param("id"))})
}

func (s *Server) handleJobResume(c *gin.Context) {
	if err := s.deps.JobService.ResumeJob(context.Background(), c.Param("id")); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, gin.H{"message": fmt.Sprintf("job %q resumed", c.Param("id"))})
}

func (s *Server) handleJobRm(c *gin.Context) {
	if err := s.deps.JobService.RemoveJob(context.Background(), c.Param("id")); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, gin.H{"message": fmt.Sprintf("job %q removed", c.Param("id"))})
}

// ── Task handlers (delegate to TaskService for mutations) ──

func (s *Server) handleTaskList(c *gin.Context) {
	ctx := context.Background()
	status := c.Query("status")
	jobID := c.Query("job")

	tasks, err := s.deps.Store.ListTasks(ctx, store.TaskFilter{Status: status, JobID: jobID})
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}

	// Default: return only active tasks.
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

func (s *Server) handleTaskGet(c *gin.Context) {
	task, err := s.deps.Store.GetTask(context.Background(), c.Param("id"))
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

func (s *Server) handleTaskKill(c *gin.Context) {
	if err := s.deps.TaskService.KillTask(context.Background(), c.Param("id")); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, gin.H{"message": fmt.Sprintf("task %q killed", c.Param("id"))})
}

func (s *Server) handleTaskRetry(c *gin.Context) {
	if err := s.deps.TaskService.RetryTask(context.Background(), c.Param("id")); err != nil {
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
