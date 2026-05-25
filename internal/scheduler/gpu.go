package scheduler

import (
	"strconv"
	"strings"
	"time"

	"github.com/gliese129/runq/internal/resource"
)

// checkGPUResidual uses nvidia-smi pmon to detect processes still occupying
// the GPUs that were assigned to a task after it exited. Logs a warning for
// each residual process found. Best-effort: errors are logged, never fatal.
func (s *Scheduler) checkGPUResidual(task *Task) {
	if len(task.GPUs) == 0 {
		return
	}
	procs, err := resource.CheckResidualProcesses(task.GPUs)
	if err != nil {
		s.logger.Warn("GPU residual check failed", "task", task.ID, "error", err)
		return
	}
	for _, p := range procs {
		s.logger.Warn("GPU residual process detected after task exit",
			"task", task.ID, "gpu", p.GPUIndex,
			"residual_pid", p.PID, "mem_mb", p.MemMB, "command", p.Command,
		)
	}
}

// gpuRefreshLoop periodically scans for external GPU usage and updates the pool.
func (s *Scheduler) gpuRefreshLoop() {
	ticker := time.NewTicker(s.cfg.GPURefreshInterval)
	defer ticker.Stop()

	for {
		select {
		case <-s.ctx.Done():
			return
		case <-ticker.C:
			s.refreshGPUUsage()
		}
	}
}

// refreshGPUUsage scans all GPUs via pmon and marks externally-occupied ones.
func (s *Scheduler) refreshGPUUsage() {
	pool, ok := s.pool.(*resource.GPUPool)
	if !ok {
		return // MockAllocator — skip
	}

	status := pool.Status()
	gpuIndices := make([]int, len(status))
	for i, g := range status {
		gpuIndices[i] = g.Index
	}

	procs, err := resource.CheckResidualProcesses(gpuIndices)
	if err != nil {
		s.logger.Debug("GPU refresh: pmon failed", "error", err)
		return
	}

	// Build set of task IDs managed by runq.
	managedIDs := make(map[string]bool)
	for _, t := range s.queue.ListRunning() {
		managedIDs[t.ID] = true
	}

	blocked, unblocked := pool.RefreshExternalUsage(procs, managedIDs)
	if len(blocked) > 0 {
		s.logger.Warn("GPUs blocked by external processes", "gpus", blocked)
	}
	if len(unblocked) > 0 {
		s.logger.Info("GPUs unblocked (external processes gone)", "gpus", unblocked)
	}
}

// gpuString converts a GPU index slice to a comma-separated string (e.g. "0,1,3").
func gpuString(gpus []int) string {
	s := make([]string, len(gpus))
	for i, g := range gpus {
		s[i] = strconv.Itoa(g)
	}
	return strings.Join(s, ",")
}
