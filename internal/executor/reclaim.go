package executor

import (
	"context"
	"log/slog"
	"os"
	"time"

	"github.com/gliese129/runq/internal/store"
	"github.com/gliese129/runq/internal/utils"
)

// AliveTask pairs a DB row with its reattach monitoring channel.
// The caller (daemon.go) is responsible for:
//  1. Reserving GPUs and registering the task in the Queue.
//  2. Passing DoneCh to the scheduler so it owns the lifecycle (cleanup on exit).
type AliveTask struct {
	Row    store.TaskRow
	DoneCh <-chan ReattachResult
}

// Reclaimer handles daemon-restart recovery.
// It scans DB for tasks that were "running" when the daemon died, checks if
// their processes are still alive, and either re-attaches monitoring or updates
// DB state for retry/failure.
//
// Reclaimer does NOT manage task lifecycle after reattach — it returns the
// monitoring channels and lets the scheduler handle cleanup (GPU release, queue
// update, job status refresh) through the same path as normally dispatched tasks.
type Reclaimer struct {
	Store  *store.Store
	Exec   *Executor
	Logger *slog.Logger
}

// Reclaim processes all previously-running tasks.
// Alive tasks get reattached and returned with their monitoring channels.
// Dead tasks get their DB status updated (pending for retry, or failed).
// Pending tasks are not touched here (handled by daemon.go restore path).
func (r *Reclaimer) Reclaim() ([]AliveTask, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	tasks, err := r.Store.ListTasks(ctx, store.TaskFilter{Status: "running"})
	if err != nil {
		return nil, err
	}

	var alive []AliveTask
	for _, t := range tasks {
		ok, _ := r.ReclaimTask(&t)
		if ok {
			// Process still running — reattach monitoring.
			ch, err := r.Exec.Reattach(t.ID, t.PID)
			if err != nil {
				r.Logger.Warn("reattach failed, treating as dead", "task", t.ID, "error", err)
				r.markDead(&t)
				continue
			}
			r.Logger.Info("task reattached", "task", t.ID, "pid", t.PID)

			// If the previous daemon had SIGSTOPped this task (L2-C freeze),
			// the in-memory FreezeState is gone now. Reattach treats the task
			// as alive (signal 0 succeeds on stopped processes), but the
			// new daemon won't know to thaw it — `runq thaw` will be a no-op.
			// WARN so the operator notices, and recommend manual recovery.
			if state, err := utils.ReadProcessState(t.PID); err == nil && state == "T" {
				r.Logger.Warn("reclaimed task is in stopped state (T); "+
					"freeze metadata was lost when daemon restarted — "+
					"use `runq kill` to terminate or `kill -CONT <pid>` to resume manually",
					"task", t.ID, "pid", t.PID)
			}

			alive = append(alive, AliveTask{Row: t, DoneCh: ch})
		} else {
			r.markDead(&t)
		}
	}
	return alive, nil
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
