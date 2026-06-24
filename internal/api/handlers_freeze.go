package api

import (
	"context"
	"fmt"
	"net/http"
	"sort"
	"strconv"

	"github.com/gin-gonic/gin"
	"github.com/gliese129/runq/internal/backend"
	"github.com/gliese129/runq/internal/scheduler"
	"github.com/gliese129/runq/internal/utils"
	"github.com/shirou/gopsutil/v4/disk"
)

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
		c.JSON(http.StatusOK, backend.ThawResponse{Thawed: thawed})
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

	resp := backend.ThawResponse{Thawed: result.Thawed}
	if len(result.Blocked) > 0 {
		resp.Blocked = make(map[string]backend.BlockedDetail, len(result.Blocked))

		// Cache mount mates by mount — multiple blocked siblings on the
		// same disk would otherwise re-query store.checkpoints N times.
		// Also load partition table once and reuse across all mounts.
		parts, _ := utils.LoadMountTable()
		matesCache := make(map[string][]backend.MountMate, len(result.Blocked))
		ctx := c.Request.Context()

		for tid, br := range result.Blocked {
			mates, ok := matesCache[br.Mount]
			if !ok {
				mates = s.collectMountMates(ctx, br.Mount, parts)
				matesCache[br.Mount] = mates
			}
			resp.Blocked[tid] = backend.BlockedDetail{
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
) []backend.MountMate {
	running := s.deps.Queue.ListByStatus(scheduler.StatusRunning)
	mates := make([]backend.MountMate, 0, len(running))
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
		mates = append(mates, backend.MountMate{
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
