package hpc

import (
	"context"
	"fmt"
	"strings"

	"github.com/gliese129/runq/internal/hpccore"
	"github.com/gliese129/runq/internal/store"
)

// Kill cancels a job (all its tasks) or a single task. target is matched as a
// job id first, then as a task id.
//
// It is best-effort but NEVER lies: a task is marked "killed" only after its
// cancel command actually succeeds. Three cases per non-terminal task:
//
//   - already terminal → skipped (nothing to cancel).
//   - has external id → run kill_template; mark killed only on success.
//   - no external id → REFUSED: the submit id was lost, so we have no handle to
//     cancel and the cluster job may be running untracked. We will not record a
//     kill we did not perform — the user must check the cluster manually.
//
// Kill does not abort on the first problem: it cancels every task it can and
// returns how many were killed plus an error summarizing the ones it could not
// (so a single stuck task doesn't block cancelling the rest of a job).
func (b *Backend) Kill(ctx context.Context, target string) (killed int, err error) {
	tasks, jobID, rerr := b.resolveTargets(ctx, target)
	if rerr != nil {
		return 0, rerr
	}

	// Refresh the job aggregate on EVERY exit (incl. the error path) so the job
	// status reflects the tasks we did kill, without waiting for the next poll.
	// Named returns let a refresh error surface on the success path without
	// clobbering a real error from the loop.
	if jobID != "" {
		defer func() {
			if e := b.refreshJobStatus(ctx, jobID); e != nil && err == nil {
				err = fmt.Errorf("refresh job %s status: %w", jobID, e)
			}
		}()
	}

	var problems []string
	for _, tk := range tasks {
		if isTerminal(tk.Status) {
			continue
		}
		if tk.ExternalID == "" {
			problems = append(problems, fmt.Sprintf(
				"%s: no external id (submit id was lost) — cannot cancel; check the cluster (e.g. squeue) manually", tk.ID))
			continue
		}

		cmd, rErr := hpccore.Render(b.Cfg.KillTemplate, map[string]string{"ext_id": tk.ExternalID})
		if rErr != nil {
			problems = append(problems, fmt.Sprintf("%s: render kill_template: %v", tk.ID, rErr))
			continue
		}
		if out, xerr := b.Run(ctx, cmd); xerr != nil {
			opLog("KILL FAIL task=%s ext=%s\ncmd: %s\nerr: %v\noutput: %s", tk.ID, tk.ExternalID, cmd, xerr, out)
			problems = append(problems, fmt.Sprintf(
				"%s (ext id %s): cancel failed (may have already finished): %v %s",
				tk.ID, tk.ExternalID, xerr, strings.TrimSpace(out)))
			continue
		}
		opLog("KILL OK task=%s ext=%s cmd: %s", tk.ID, tk.ExternalID, cmd)

		if uErr := b.Store.UpdateTaskStatus(ctx, tk.ID, "killed",
			map[string]any{"finished_at": nowUnix(), "status_source": hpccore.SourceRunq}); uErr != nil {
			return killed, fmt.Errorf("mark task %s killed: %w", tk.ID, uErr)
		}
		killed++
	}

	if len(problems) > 0 {
		return killed, fmt.Errorf("killed %d task(s); could not cancel %d:\n  %s",
			killed, len(problems), strings.Join(problems, "\n  "))
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
