package service

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/gliese129/runq/internal/executor"
	"github.com/gliese129/runq/internal/scheduler"
	"github.com/gliese129/runq/internal/store"
)

// TaskService handles task-level state transitions.
// All mutations go through here so DB + Queue stay in sync.
type TaskService struct {
	Store *store.Store
	Queue *scheduler.Queue
	Exec  *executor.Executor
}

// KillTask terminates a running or pending task.
// Running: cancels the process via executor. Pending: marks killed in queue + DB.
func (s *TaskService) KillTask(ctx context.Context, taskID string) error {
	task := s.Queue.Get(taskID)
	if task == nil {
		return fmt.Errorf("task %q not found in queue", taskID)
	}

	switch task.Status {
	case scheduler.StatusRunning:
		s.Exec.Stop(taskID)
		// DB update happens in scheduler's runTask → completeTask path.
	case scheduler.StatusPending:
		s.Queue.Complete(taskID, scheduler.StatusKilled)
		_ = s.Store.UpdateTaskStatus(ctx, taskID, "killed", map[string]any{
			"finished_at": time.Now().Unix(),
		})
	default:
		return fmt.Errorf("task %q is %s, cannot kill", taskID, task.Status)
	}
	return nil
}

// RetryTask re-enqueues a failed or killed task.
// Resets DB state to pending and pushes back to queue.
func (s *TaskService) RetryTask(ctx context.Context, taskID string) error {
	row, err := s.Store.GetTask(ctx, taskID)
	if err != nil {
		return err
	}
	if row == nil {
		return fmt.Errorf("task %q not found", taskID)
	}
	if row.Status != "failed" && row.Status != "killed" {
		return fmt.Errorf("task %q is %s, only failed/killed tasks can be retried", taskID, row.Status)
	}

	// Reset task state in DB.
	if err := s.Store.UpdateTaskStatus(ctx, taskID, "pending", map[string]any{
		"gpus":        nil,
		"pid":         nil,
		"started_at":  nil,
		"finished_at": nil,
	}); err != nil {
		return err
	}

	// Re-read updated row, convert to scheduler.Task, push to queue.
	row, _ = s.Store.GetTask(ctx, taskID)
	task := TaskRowToSchedulerTask(row)
	s.Queue.Push(task)
	return nil
}

// TaskRowToSchedulerTask converts a store.TaskRow to a scheduler.Task.
// JSON fields (params, env) are decoded back to maps.
func TaskRowToSchedulerTask(row *store.TaskRow) *scheduler.Task {
	var params map[string]any
	if row.ParamsJSON != "" {
		_ = json.Unmarshal([]byte(row.ParamsJSON), &params)
	}
	var env map[string]string
	if row.EnvJSON != "" {
		_ = json.Unmarshal([]byte(row.EnvJSON), &env)
	}
	return &scheduler.Task{
		ID:          row.ID,
		JobID:       row.JobID,
		ProjectName: row.ProjectName,
		Command:     row.Command,
		Params:      params,
		GPUsNeeded:  row.GPUsNeeded,
		Status:      scheduler.StatusPending,
		RetryCount:  row.RetryCount,
		MaxRetry:    row.MaxRetry,
		LogPath:     row.LogPath,
		WorkingDir:  row.WorkingDir,
		Env:         env,
		EnqueuedAt:  row.EnqueuedAt,
		Resumable:   row.Resumable,
		ExtraArgs:   row.ExtraArgs,
	}
}
