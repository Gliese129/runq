package api

import (
	"fmt"
	"net/http"

	"github.com/gin-gonic/gin"
	"github.com/gliese129/runq/internal/backend"
	"github.com/gliese129/runq/internal/scheduler"
	"github.com/gliese129/runq/internal/store"
)

// registerRoutes wires the runqd EXECUTOR LANE only (protocol spec §9, D1).
// The public dashboard-style Gin surface is retired: humans and the WebUI
// speak /api/v1/* to the CLIENT daemon; runqd is driven exclusively by
// the client daemon's backend and the CLI plumbing commands (⚙).
//
// Lane inventory:
//   - sbatch/squeue isomorphs + task kill (= scancel) — the runq preset's
//     scheduler dialect
//   - gpu / status / thaw — machine plumbing probes
//   - /internal/freeze-self — SDK-only control plane
func (s *Server) registerRoutes() {
	api := s.router.Group("/api")

	// Task intake lane (runq preset): a runq client
	// drives THIS server like an external scheduler via sbatch/squeue
	// isomorphs. scancel maps onto the task kill endpoint.
	api.POST("/sbatch", s.handleSbatch)
	api.GET("/squeue", s.handleSqueue)
	api.POST("/tasks/:id/kill", s.handleTaskKill) // scancel isomorph

	// Machine plumbing
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

// ── Task intake handlers (runq preset, executor lane) ──

func (s *Server) handleSbatch(c *gin.Context) {
	var spec backend.TaskSpec
	if err := c.ShouldBindJSON(&spec); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	id, err := s.deps.Local.Enqueue(c.Request.Context(), spec)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusCreated, gin.H{"task_id": id})
}

// handleSqueue lists this server's non-terminal tasks as (id, status) pairs
// — the batch status probe of the runq preset. Status vocabulary is runq's
// own, which is already remote.ParseSignal's canonical vocabulary.
func (s *Server) handleSqueue(c *gin.Context) {
	rows, err := s.deps.Store.ListTasks(c.Request.Context(), store.TaskFilter{})
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	out := make([]backend.QueueEntry, 0, len(rows))
	for _, r := range rows {
		if store.IsActiveStatus(r.Status) {
			out = append(out, backend.QueueEntry{ID: r.ID, Status: r.Status})
		}
	}
	c.JSON(http.StatusOK, out)
}

func (s *Server) handleTaskKill(c *gin.Context) {
	if err := s.deps.Multi.KillTask(c.Request.Context(), c.Param("id")); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, gin.H{"message": fmt.Sprintf("task %q killed", c.Param("id"))})
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

// ── Freeze / Thaw lives in handlers_freeze.go ──
//
// Types (FreezeSelfReq / ThawResponse / BlockedDetail / MountMate) and
// the corresponding handlers (handleFreezeSelf / handleThaw + helpers
// scopeOwned / collectMountMates) are in a sibling file so this file
// stays focused on the boring REST CRUD layer.
