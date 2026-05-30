package hpc

import (
	"context"
	"fmt"

	"github.com/gliese129/runq/internal/hpccore"
	"github.com/gliese129/runq/internal/store"
)

// Kill cancels a job (all its tasks) or a single task. target is matched as a
// job id first, then as a task id. For each non-terminal task it runs the
// kill_template against the external id (best-effort) and marks the task killed
// in the DB (the source of truth). Returns how many tasks were killed.
func (b *Backend) Kill(ctx context.Context, target string) (int, error) {
	tasks, jobID, err := b.resolveTargets(ctx, target)
	if err != nil {
		return 0, err
	}

	killed := 0
	for _, tk := range tasks {
		if isTerminal(tk.Status) {
			continue
		}
		if tk.ExternalID != "" {
			if cmd, rerr := hpccore.Render(b.Cfg.KillTemplate, map[string]string{"ext_id": tk.ExternalID}); rerr == nil {
				_, _ = b.Run(ctx, cmd) // best-effort; DB mark below is authoritative
			}
		}
		if err := b.Store.UpdateTaskStatus(ctx, tk.ID, "killed", map[string]any{"finished_at": nowUnix()}); err != nil {
			return killed, fmt.Errorf("mark task %s killed: %w", tk.ID, err)
		}
		killed++
	}

	if jobID != "" {
		if err := b.refreshJobStatus(ctx, jobID); err != nil {
			return killed, err
		}
	}
	return killed, nil
}

// resolveTargets interprets target as a job id (returns all its tasks) or, if
// no such job, a single task id. The returned jobID is non-empty only for the
// job case, so the caller knows whether to refresh job aggregate status.
func (b *Backend) resolveTargets(ctx context.Context, target string) ([]store.TaskRow, string, error) {
	if job, err := b.Store.GetJob(ctx, target); err == nil && job != nil {
		tasks, err := b.Store.ListTasks(ctx, store.TaskFilter{JobID: target})
		if err != nil {
			return nil, "", err
		}
		return tasks, target, nil
	}

	tk, err := b.Store.GetTask(ctx, target)
	if err != nil {
		return nil, "", err
	}
	if tk == nil {
		return nil, "", fmt.Errorf("no job or task with id %q", target)
	}
	return []store.TaskRow{*tk}, tk.JobID, nil
}
