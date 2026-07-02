package remote

import (
	"context"
	"fmt"
	"path"

	"github.com/gliese129/runq/internal/scheduler"
	"github.com/gliese129/runq/internal/utils"
)

// submitCmdFileName is the rendered submit command, persisted at submit time
// next to run.sh. The launcher replays it verbatim, which makes launches
// deterministic across retries — a requeued task re-runs the exact command
// that was validated at submit time. Written 0o600: like run.sh it can embed
// project environment values.
const submitCmdFileName = "submit.cmd"

// Launcher adapts Backend to scheduler.Launcher — the unsupervised lane.
// Launch hands one task to the external scheduler (sbatch/qsub/... rendered
// at submit time) and returns immediately; terminal states arrive later via
// reconcile → Scheduler.FinishTask.
type Launcher struct {
	b *Backend
}

// NewLauncher wraps a remote Backend in the scheduler.Launcher interface.
func NewLauncher(b *Backend) *Launcher { return &Launcher{b: b} }

// Supervised is false: Launch returns on handoff, not on task exit.
func (l *Launcher) Supervised() bool { return false }

// Kill forwards a cancel (scancel/qdel via kill_template) for the task.
// Best-effort: errors are logged to the oplog; the task only becomes
// "killed" in the DB once the cancel actually succeeds (Backend.Kill).
func (l *Launcher) Kill(taskID string) {
	if _, err := l.b.Kill(context.Background(), taskID); err != nil {
		opLog("KILL FAIL task=%s err=%v", taskID, err)
	}
}

// Launch replays the task's persisted submit.cmd and records the external id.
//
// Error contract:
//   - read/submit failure → plain error. The command never ran (or the
//     scheduler rejected it): no cluster job exists, retry is safe.
//   - id-parse or id-persist failure → wrapped scheduler.ErrLaunchUntracked.
//     A cluster job may exist untracked: the scheduler must NOT retry.
func (l *Launcher) Launch(ctx context.Context, t *scheduler.Task, _ map[string]string, _ func(scheduler.StartInfo)) (scheduler.LaunchResult, error) {
	cmdFile := path.Join(t.TaskDir, submitCmdFileName)
	cmdBytes, err := l.b.FS.ReadFile(cmdFile)
	if err != nil {
		return scheduler.LaunchResult{}, fmt.Errorf("read %s: %w", cmdFile, err)
	}

	// New attempt = fresh wrapper state, BEFORE the submit so the new job
	// can't race the reset.
	if werr := l.b.ResetWrapperState(ctx, t.TaskDir, t.ID); werr != nil {
		return scheduler.LaunchResult{}, werr
	}

	out, err := l.b.shellRun(ctx, string(cmdBytes))
	if err != nil {
		opLog("SUBMIT FAIL task=%s job=%s\ncmd file: %s\nerr: %v\noutput: %s", t.ID, t.JobID, cmdFile, err, out)
		return scheduler.LaunchResult{}, fmt.Errorf("submit %s failed: %w\noutput:\n%s", t.ID, err, out)
	}

	extID, err := utils.ExtractSubmitID(out, l.b.Cfg.SubmitIDRegex)
	if err != nil {
		opLog("SUBMIT NOID task=%s job=%s\noutput: %s", t.ID, t.JobID, out)
		return scheduler.LaunchResult{}, fmt.Errorf(
			"%w: %s — check submit_id_regex (the cluster job may be running untracked and is not killable without its id): %v\noutput:\n%s",
			scheduler.ErrLaunchUntracked, t.ID, err, out)
	}

	if uerr := l.b.Store.UpdateTaskStatus(ctx, t.ID, "pending", map[string]any{"external_id": extID}); uerr != nil {
		return scheduler.LaunchResult{}, fmt.Errorf(
			"%w: record external id %s for %s: %v", scheduler.ErrLaunchUntracked, extID, t.ID, uerr)
	}
	opLog("SUBMIT OK task=%s job=%s ext=%s", t.ID, t.JobID, extID)
	return scheduler.LaunchResult{}, nil
}

// compile-time interface check
var _ scheduler.Launcher = (*Launcher)(nil)
