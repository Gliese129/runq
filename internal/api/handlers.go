package api

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strconv"

	"github.com/gin-gonic/gin"
	"github.com/gliese129/runq/internal/job"
	"github.com/gliese129/runq/internal/project"
	"github.com/gliese129/runq/internal/scheduler"
	"github.com/gliese129/runq/internal/store"
	"github.com/gliese129/runq/internal/utils"
	"github.com/shirou/gopsutil/v4/disk"
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
	api.POST("/thaw", s.handleThaw)

	// Internal — SDK-only control plane. Not for human / CLI use.
	// Auth model matches Linux file permissions on the unix socket: any
	// process under the daemon's UID can call these. We don't try to be
	// stronger than the OS.
	internal := api.Group("/internal")
	{
		internal.POST("/freeze-self", s.handleFreezeSelf)
	}
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

	jobID, taskCount, err := s.deps.JobService.SubmitJob(context.Background(), jobCfg)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
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

// ── Freeze / Thaw types ──

// FreezeSelfReq is the JSON body for POST /api/internal/freeze-self.
// SDK posts this from inside @runq.safe_save when it detects upcoming-ckpt
// size would exceed free disk. Daemon SIGSTOPs the task's pgroup; the HTTP
// recv inside the SDK blocks (its own process is now paused) until thaw.
type FreezeSelfReq struct {
	TaskID    string `json:"task_id" binding:"required"`
	FreeBytes int64  `json:"free_bytes"`
	NeededEst int64  `json:"needed_est" binding:"required"`
	Mount     string `json:"mount" binding:"required"`
}

// ThawResponse is the JSON body of POST /api/thaw.
//
// Thawed lists task IDs that successfully resumed.
//
// Blocked entries each carry the mount they're on, current free, the
// per-task threshold (== FrozenTask.NeededBytes == SDK-reported
// upcoming-ckpt × safety factor), and the list of OTHER running tasks
// sharing that mount so the user knows whose ckpts to ask about deleting.
type ThawResponse struct {
	Thawed  []string                 `json:"thawed"`
	Blocked map[string]BlockedDetail `json:"blocked,omitempty"`
}

// BlockedDetail enriches scheduler.BlockReason with per-mount mate info.
type BlockedDetail struct {
	Mount     string      `json:"mount"`
	FreeBytes int64       `json:"free_bytes"` // -1 if disk.Usage failed
	Threshold int64       `json:"threshold"`  // per-task NeededBytes
	DiskUsers []MountMate `json:"disk_users,omitempty"`
}

// MountMate is one other running task sharing the same mount as a blocked
// task — sorted by total ckpt bytes desc so the disk-hog stands out.
// Includes the user's own tasks too: a job with 4 tasks on the same disk
// will see itself listed.
type MountMate struct {
	TaskID          string `json:"task_id"`
	User            string `json:"user"`
	JobID           string `json:"job_id"`
	LatestCkptBytes int64  `json:"latest_ckpt_bytes"`
	TotalCkptBytes  int64  `json:"total_ckpt_bytes"`
}

// handleFreezeSelf is the SDK-only endpoint that registers a task as frozen
// and SIGSTOPs its pgroup.
//
// Threading note: after Freeze() returns the task's pgroup is SIGSTOPped,
// but THIS handler runs in the daemon process (different pgroup) and so
// continues executing normally. The 200 response is written into the
// socket buffer; the SDK's recv() reads it only after a SIGCONT.
//
// If Freeze fails to actually stop the pgroup (e.g. process died between
// the SDK's check and this call landing), the FrozenTask isn't registered
// — daemon logs warn inside Freeze, SDK gets 200 but its process isn't
// paused. SDK will then either retry the save and crash on ENOSPC, or
// proceed normally if conditions changed. We don't try to detect this
// here; the failure mode is the same as if the SDK were absent.
func (s *Server) handleFreezeSelf(c *gin.Context) {
	if s.deps.Freeze == nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "freeze state not configured"})
		return
	}
	var req FreezeSelfReq
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}

	task := s.deps.Queue.Get(req.TaskID)
	if task == nil {
		c.JSON(http.StatusNotFound, gin.H{
			"error": fmt.Sprintf("task %q not found in queue", req.TaskID),
		})
		return
	}
	if task.Status != scheduler.StatusRunning {
		c.JSON(http.StatusBadRequest, gin.H{
			"error": fmt.Sprintf("task %q is %s, not running", req.TaskID, task.Status),
		})
		return
	}
	if task.PID <= 0 {
		c.JSON(http.StatusBadRequest, gin.H{
			"error": fmt.Sprintf("task %q has no pid; cannot freeze", req.TaskID),
		})
		return
	}

	ft := scheduler.FrozenTask{
		PID:         task.PID,
		Mount:       req.Mount,
		NeededBytes: req.NeededEst,
		JobID:       task.JobID,
	}
	ev := scheduler.FreezeEvent{
		Reason:        "disk_low",
		TriggerTaskID: req.TaskID,
		DiskMount:     req.Mount,
		FreeBytes:     req.FreeBytes,
		NeededEst:     req.NeededEst,
	}
	s.deps.Freeze.Freeze(ev, map[string]scheduler.FrozenTask{req.TaskID: ft})

	s.deps.Logger.Info("sdk self-freeze accepted",
		"task", req.TaskID, "job", task.JobID, "mount", req.Mount,
		"free_bytes", req.FreeBytes, "needed_est", req.NeededEst)
	c.JSON(http.StatusOK, gin.H{"frozen": true})
}

// handleThaw releases a disk-freeze.
//
//	POST /api/thaw                — checked thaw (per-task NeededBytes vs free)
//	POST /api/thaw?force=true     — bypass disk check (caller accepts ENOSPC risk)
//	POST /api/thaw?owner=<uid>    — restrict to tasks owned by uid
//
// Idempotent: 200 with empty Thawed/Blocked when daemon is not frozen.
// 503 only when FreezeState was never wired (test setups).
//
// Owner scoping in stage 1 is opt-in via ?owner=. Without the param, all
// frozen tasks are eligible regardless of their UID — fine for single-user
// labs, listed as a known limitation in stage1_backend_prep.md.
func (s *Server) handleThaw(c *gin.Context) {
	if s.deps.Freeze == nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "freeze state not configured"})
		return
	}

	force := c.Query("force") == "true"
	owned := s.scopeOwned(s.deps.Freeze.FrozenTaskIDs(), c.Query("owner"))

	if force {
		thawed := s.deps.Freeze.ThawForce(owned)
		s.deps.Logger.Info("force-thaw via API",
			"requested", len(owned), "thawed", len(thawed))
		c.JSON(http.StatusOK, ThawResponse{Thawed: thawed})
		return
	}

	// Wrap disk.Usage so FreezeState stays gopsutil-agnostic.
	freeFn := func(mount string) (uint64, error) {
		u, err := disk.Usage(mount)
		if err != nil {
			return 0, err
		}
		return u.Free, nil
	}
	result := s.deps.Freeze.ThawTasks(owned, freeFn)

	resp := ThawResponse{Thawed: result.Thawed}
	if len(result.Blocked) > 0 {
		resp.Blocked = make(map[string]BlockedDetail, len(result.Blocked))

		// Cache mount mates by mount — multiple blocked siblings on the
		// same disk would otherwise re-query store.checkpoints N times.
		// Also load partition table once and reuse across all mounts.
		parts, _ := utils.LoadMountTable()
		matesCache := make(map[string][]MountMate, len(result.Blocked))
		ctx := c.Request.Context()

		for tid, br := range result.Blocked {
			mates, ok := matesCache[br.Mount]
			if !ok {
				mates = s.collectMountMates(ctx, br.Mount, parts)
				matesCache[br.Mount] = mates
			}
			resp.Blocked[tid] = BlockedDetail{
				Mount:     br.Mount,
				FreeBytes: br.FreeBytes,
				Threshold: br.Threshold,
				DiskUsers: mates,
			}
		}
	}

	s.deps.Logger.Info("thaw via API",
		"thawed", len(result.Thawed), "blocked", len(result.Blocked))
	c.JSON(http.StatusOK, resp)
}

// scopeOwned filters the given task IDs to those owned by the caller.
// Stage 1 reads `?owner=<uid>` from the request; missing/invalid → no
// filter. Stage 2 will replace this with Unix-socket peer credentials.
//
// Uses Queue.Get rather than Store.GetTask — frozen tasks must be running,
// which means they're in queue. Avoids a DB hit per task.
func (s *Server) scopeOwned(ids []string, ownerStr string) []string {
	if ownerStr == "" {
		return ids
	}
	uid, err := strconv.Atoi(ownerStr)
	if err != nil {
		return ids // bad ?owner=, ignore filter
	}
	out := make([]string, 0, len(ids))
	for _, id := range ids {
		t := s.deps.Queue.Get(id)
		if t == nil {
			continue
		}
		if t.UID == uid {
			out = append(out, id)
		}
	}
	return out
}

// collectMountMates returns the running tasks sharing the given mount,
// sorted by total ckpt bytes desc — so the CLI can show "bob's task t9
// has written 350 GB to /data1" prominently when explaining why a thaw
// was blocked.
//
// Includes the caller's own tasks too. A job with 4 sibling tasks on the
// same disk will see itself listed; that's intentional (the user might
// not realize their own job is the disk hog).
//
// Tasks with no checkpoint history (total = 0) end up at the bottom of
// the list — visible as "task X — 0 GB" signaling "this one isn't writing
// checkpoints, probably not the culprit".
//
// `parts` is the partition table from utils.LoadMountTable(); caller passes
// it in so we don't re-enumerate per call.
func (s *Server) collectMountMates(
	ctx context.Context,
	mount string,
	parts []disk.PartitionStat,
) []MountMate {
	running := s.deps.Queue.ListByStatus(scheduler.StatusRunning)
	mates := make([]MountMate, 0, len(running))
	for _, t := range running {
		// Skip tasks whose ckpt dir doesn't resolve to a mount (unmounted
		// path, weird overlay). Don't lump them under "" — different
		// unmounted dirs may live on different unknown disks.
		taskMount := utils.MountOf(t.CheckpointDir, parts)
		if taskMount == "" || taskMount != mount {
			continue
		}
		var latestBytes int64
		if latest, _ := s.deps.Store.LatestCheckpoint(ctx, t.ID); latest != nil {
			latestBytes = latest.SizeBytes
		}
		total, _ := s.deps.Store.TotalCheckpointSize(ctx, t.ID)
		mates = append(mates, MountMate{
			TaskID:          t.ID,
			User:            t.User,
			JobID:           t.JobID,
			LatestCkptBytes: latestBytes,
			TotalCkptBytes:  total,
		})
	}
	sort.Slice(mates, func(i, j int) bool {
		return mates[i].TotalCkptBytes > mates[j].TotalCkptBytes
	})
	return mates
}
