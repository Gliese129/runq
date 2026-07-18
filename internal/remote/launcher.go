package remote

import (
	"context"
	"errors"
	"fmt"
	"path"
	"strings"

	"github.com/gliese129/runq/internal/rfs"
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
	if err := l.KillErr(taskID); err != nil {
		opLog("KILL FAIL task=%s err=%v", taskID, err)
	}
}

// KillErr is the error-reporting cancel the scheduler's kill/submit race
// path uses (RQ-69): a task may be recorded killed ONLY after the cancel
// command actually succeeded, so the caller needs the verdict.
func (l *Launcher) KillErr(taskID string) error {
	_, err := l.b.Kill(context.Background(), taskID)
	return err
}

// Launch replays the task's persisted submit.cmd and records the external id.
//
// Error contract:
//   - read/submit failure → plain error. The command never ran (or the
//     scheduler rejected it): no cluster job exists, retry is safe.
//   - id-parse or id-persist failure → wrapped scheduler.ErrLaunchUntracked.
//     A cluster job may exist untracked: the scheduler must NOT retry.
func (l *Launcher) Launch(ctx context.Context, t *scheduler.Task, _ map[string]string, _ func(scheduler.StartInfo)) (scheduler.LaunchResult, error) {
	// Pre-submit file operations are pure FS work: their failures are
	// transport-class (SSH down, workspace unreachable) — TRANSIENT, the
	// launch never happened.
	cmdFile := path.Join(t.TaskDir, submitCmdFileName)
	cmdBytes, err := l.b.FS.ReadFile(cmdFile)
	if err != nil {
		return scheduler.LaunchResult{}, fmt.Errorf("%w: read %s: %v", scheduler.ErrLaunchTransient, cmdFile, err)
	}

	// New attempt = fresh wrapper state, BEFORE the submit so the new job
	// can't race the reset.
	if werr := l.b.ResetWrapperState(ctx, t.TaskDir, t.ID); werr != nil {
		return scheduler.LaunchResult{}, fmt.Errorf("%w: %v", scheduler.ErrLaunchTransient, werr)
	}

	// RQ-69: deterministic per-attempt cancel handle, known BEFORE the
	// submit. The runqd preset's `runq sbatch` adopts it as the remote task
	// id (external_id = handle — the "submit id was lost" class dies for
	// that lane); dialect schedulers (sbatch/qdel) ignore the variable.
	// The attempt suffix keeps a stale verdict from a previous attempt from
	// being attributed to this one — identity stays the task id alone.
	handle := fmt.Sprintf("%s-a%d", t.ID, t.RetryCount)
	cmd := "export RUNQ_SUBMIT_HANDLE=" + utils.ShellQuote(handle) + "\n" + string(cmdBytes)

	out, exitCode, rerr := l.b.shellRunClassified(ctx, cmd)
	if rerr != nil {
		// Two transport classes, split on FACTS not heuristics (RQ-74):
		//   - the command never started (session/dial failure) → TRANSIENT,
		//     retry is safe;
		//   - the command STARTED but the verdict was lost (connection died
		//     mid-flight) → outcome unknown: the scheduler may have accepted
		//     the job. Resubmitting risks a duplicate cluster job, so this
		//     maps to ErrLaunchUntracked and the task waits for reconcile.
		if errors.Is(rerr, rfs.ErrExecInterrupted) {
			opLog("SUBMIT INTERRUPTED task=%s job=%s err=%v\npartial output: %s", t.ID, t.JobID, rerr, out)
			return scheduler.LaunchResult{}, fmt.Errorf(
				"%w: submit command interrupted mid-flight — a cluster job may exist: %v\npartial output:\n%s",
				scheduler.ErrLaunchUntracked, rerr, strings.TrimSpace(out))
		}
		opLog("SUBMIT TRANSPORT task=%s job=%s err=%v", t.ID, t.JobID, rerr)
		return scheduler.LaunchResult{}, fmt.Errorf("%w: %v", scheduler.ErrLaunchTransient, rerr)
	}
	if exitCode != 0 {
		// The scheduler RAN and said no — deterministic rejection; its own
		// words go into the permanent failure. The error carries the full
		// evidence (output + exit code + the rendered command that was run):
		// the scheduler persists it as the task's failure_detail (RQ-74), so
		// the death cause reaches `runq task show` / the dashboard verbatim
		// instead of living only in the oplog.
		opLog("SUBMIT REJECTED task=%s job=%s exit=%d\ncmd file: %s\noutput: %s", t.ID, t.JobID, exitCode, cmdFile, out)
		return scheduler.LaunchResult{}, fmt.Errorf(
			"submit %s rejected (exit %d):\n%s\n\nsubmit command (%s):\n%s",
			t.ID, exitCode, strings.TrimSpace(out), cmdFile, strings.TrimSpace(string(cmdBytes)))
	}

	extID, err := utils.ExtractSubmitID(out, l.b.Cfg.SubmitIDRegex)
	if err != nil {
		opLog("SUBMIT NOID task=%s job=%s\noutput: %s", t.ID, t.JobID, out)
		return scheduler.LaunchResult{}, fmt.Errorf(
			"%w: %s — check submit_id_regex (the cluster job may be running untracked and is not killable without its id): %v\noutput:\n%s",
			scheduler.ErrLaunchUntracked, t.ID, err, out)
	}

	// failure_detail: nil clears a transient "will retry" note from earlier
	// ticks (RQ-74) — the handoff has now succeeded, the wait is over.
	if uerr := l.b.Store.UpdateTaskStatus(ctx, t.ID, "pending", map[string]any{
		"external_id":    extID,
		"failure_detail": nil,
	}); uerr != nil {
		return scheduler.LaunchResult{}, fmt.Errorf(
			"%w: record external id %s for %s: %v", scheduler.ErrLaunchUntracked, extID, t.ID, uerr)
	}
	opLog("SUBMIT OK task=%s job=%s ext=%s", t.ID, t.JobID, extID)
	return scheduler.LaunchResult{ExtID: extID}, nil
}

// compile-time interface check
var _ scheduler.Launcher = (*Launcher)(nil)
