package executor

import (
	"context"
	"log/slog"
	"os"
	"time"

	"github.com/gliese129/runq/internal/store"
	"github.com/gliese129/runq/internal/utils"
)

// Reclaimer handles daemon-restart recovery.
// It scans DB for tasks that were "running" when the daemon died, checks if
// their processes are still alive, and either re-attaches monitoring or updates
// DB state for retry/failure. Queue/Pool registration is handled by the caller
// (server.go) to avoid circular imports with the scheduler package.
type Reclaimer struct {
	Store  *store.Store
	Exec   *Executor
	Logger *slog.Logger
}

// Reclaim processes all previously-running tasks.
// Alive tasks get reattached; dead tasks get their DB status updated.
// Pending tasks are not touched here (handled by server.go restore path).
func (r *Reclaimer) Reclaim() error {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	tasks, err := r.Store.ListTasks(ctx, store.TaskFilter{Status: "running"})
	if err != nil {
		return err
	}

	for _, t := range tasks {
		alive, _ := r.ReclaimTask(&t)
		if alive {
			// Process still running — reattach monitoring.
			// TODO: when GPU memory monitoring is added (L3), use GPUPool.AllocateSpecific()
			// to reserve the exact GPUs (from t.GPUs) rather than pool.Allocate() which
			// may assign different ones. For now, the alive task's GPU slots are not
			// re-registered in the pool — the caller (server.go) should handle this.
			resCh, err := r.Exec.Reattach(t.ID, t.PID)
			if err != nil {
				r.Logger.Warn("reattach failed, treating as dead", "task", t.ID, "error", err)
				r.markDead(&t)
				continue
			}
			r.Logger.Info("task reattached", "task", t.ID, "pid", t.PID)
			go r.waitReattached(t.ID, resCh)
		} else {
			r.markDead(&t)
		}
	}
	return nil
}

// markDead updates a dead task's DB status: resumable → pending, otherwise → failed.
func (r *Reclaimer) markDead(t *store.TaskRow) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	if t.Resumable && (t.MaxRetry == 0 || t.RetryCount < t.MaxRetry) {
		if err := r.Store.UpdateTaskStatus(ctx, t.ID, "pending", map[string]any{
			"retry_count": t.RetryCount + 1,
			"gpus":        nil,
			"pid":         nil,
			"started_at":  nil,
			"finished_at": nil,
		}); err != nil {
			r.Logger.Warn("requeue dead task failed", "task", t.ID, "error", err)
		} else {
			r.Logger.Info("dead task requeued", "task", t.ID, "retry", t.RetryCount+1)
		}
	} else {
		if err := r.Store.UpdateTaskStatus(ctx, t.ID, "failed", map[string]any{
			"finished_at": time.Now().Unix(),
		}); err != nil {
			r.Logger.Warn("mark dead task failed", "task", t.ID, "error", err)
		} else {
			r.Logger.Info("dead task marked failed", "task", t.ID)
		}
	}
}

// waitReattached waits for a reattached process to exit and updates DB.
func (r *Reclaimer) waitReattached(taskID string, ch <-chan ReattachResult) {
	res, ok := <-ch
	if !ok {
		return
	}

	var status string
	switch {
	case res.Killed:
		status = "killed"
	default:
		// Signal 0 polling can't retrieve real exit code.
		// Treat non-killed exit as failed; user can inspect logs and retry.
		status = "failed"
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := r.Store.UpdateTaskStatus(ctx, taskID, status, map[string]any{
		"finished_at": time.Now().Unix(),
	}); err != nil {
		r.Logger.Warn("update reattached task failed", "task", taskID, "error", err)
	} else {
		r.Logger.Info("reattached task exited", "task", taskID, "status", status)
	}
}

// ReclaimTask checks if a previously-running task's process is still alive.
func (r *Reclaimer) ReclaimTask(row *store.TaskRow) (alive bool, err error) {
	return utils.IsProcessAlive(row.PID, time.Unix(row.StartTime, 0)), nil
}

// Reattach registers an already-running process for monitoring via signal 0 polling.
// Returns a channel that receives a result when the process exits or is killed.
// Stop(taskID) cancels the context, triggering a kill.
func (e *Executor) Reattach(taskID string, pid int) (<-chan ReattachResult, error) {
	p, err := os.FindProcess(pid)
	if err != nil {
		return nil, err
	}

	ctx, cancel := context.WithCancel(context.Background())
	e.mu.Lock()
	e.cancels[taskID] = cancel
	e.mu.Unlock()

	ch := make(chan ReattachResult, 1)

	go func() {
		defer func() {
			cancel()
			e.mu.Lock()
			delete(e.cancels, taskID)
			e.mu.Unlock()
			close(ch)
		}()

		// Poll with signal 0 — can't Wait() on a non-child process.
		ticker := time.NewTicker(2 * time.Second)
		defer ticker.Stop()

		for {
			select {
			case <-ctx.Done():
				// Stop() was called — kill process and its group.
				_ = p.Kill()
				killProcessGroup(pid)
				ch <- ReattachResult{ExitCode: -1, Killed: true}
				return
			case <-ticker.C:
				if !isAlive(p) {
					// Process exited — clean up any lingering children.
					killProcessGroup(pid)
					ch <- ReattachResult{ExitCode: -1, Killed: false}
					return
				}
			}
		}
	}()

	return ch, nil
}

// ReattachResult is sent when a reattached process exits.
type ReattachResult struct {
	ExitCode int  // -1 = unknown (signal 0 poll can't retrieve real exit code)
	Killed   bool // true if killed via Stop()
}

// isAlive checks if a process is still running using signal 0.
func isAlive(p *os.Process) bool {
	return p.Signal(os.Signal(nil)) == nil
}
