package scheduler

import (
	"context"
	"errors"
	"time"

	"github.com/gliese129/runq/internal/executor"
)

// ErrLaunchUntracked is returned (wrapped) by unsupervised launchers when the
// task WAS handed to the external scheduler but runq failed to record its
// external id. The task must NOT be retried — a retry would submit a second
// cluster job while the first may be running untracked. The scheduler leaves
// such tasks in flight; reconcile heals them via status.json if they run.
var ErrLaunchUntracked = errors.New("task submitted but external id unknown")

// ErrLaunchTransient is returned (wrapped) by unsupervised launchers when the
// submission NEVER REACHED the scheduler (transport/file layer: SSH down,
// runqd not yet listening, workspace unreadable). This is not the task's
// failure: it returns to pending without consuming retry budget and the next
// tick tries again. Contrast with a scheduler REJECTION (command ran, exit
// non-zero) — that is deterministic and fails the task permanently.
var ErrLaunchTransient = errors.New("scheduler unreachable")

// Launcher abstracts how a dispatched task is started and killed. It is the
// single divergence point between execution models:
//
//   - LocalLauncher (supervised): fork a process via the executor and block
//     until it exits. The LaunchResult carries the real exit code.
//   - RemoteLauncher (unsupervised, Step 2): hand the task to an external
//     scheduler (e.g. sbatch over SSH) and return immediately. The terminal
//     state arrives later through Scheduler.FinishTask, fed by reconcile.
//
// The scheduler's queue/tick/retry/kill lifecycle is identical either way.
type Launcher interface {
	// Launch starts the task. For supervised launchers this blocks until the
	// task exits; for unsupervised launchers it returns once the task has
	// been handed off. env is the fully-merged task environment; onStart is
	// invoked as soon as the task is running (PID known) and may be nil.
	Launch(ctx context.Context, t *Task, env map[string]string, onStart func(StartInfo)) (LaunchResult, error)

	// Kill best-effort terminates a running task.
	Kill(taskID string)

	// Supervised reports whether Launch blocks until task exit.
	Supervised() bool
}

// StartInfo is delivered to the onStart callback when the task begins running.
type StartInfo struct {
	PID       int
	StartTime time.Time
}

// LaunchResult is returned by Launch. For supervised launchers ExitCode is
// the process exit code; unsupervised launchers report the handoff — ExtID
// is the external scheduler's id for this attempt (informational; the
// launcher has already persisted it to the store).
type LaunchResult struct {
	ExitCode  int
	PID       int
	StartTime time.Time
	ExtID     string
}

// LocalLauncher runs tasks as local child processes via the executor.
type LocalLauncher struct {
	exec *executor.Executor
}

// NewLocalLauncher wraps an executor in the Launcher interface.
func NewLocalLauncher(exec *executor.Executor) *LocalLauncher {
	return &LocalLauncher{exec: exec}
}

// Supervised is true: Launch blocks until the process exits.
func (l *LocalLauncher) Supervised() bool { return true }

// Kill stops the task's process group via the executor.
func (l *LocalLauncher) Kill(taskID string) { l.exec.Stop(taskID) }

// Launch translates the Task into an executor.RunSpec and blocks until the
// process exits (or ctx is cancelled).
func (l *LocalLauncher) Launch(ctx context.Context, t *Task, env map[string]string, onStart func(StartInfo)) (LaunchResult, error) {
	spec := executor.RunSpec{
		TaskID:     t.ID,
		Command:    t.Command,
		WorkingDir: t.WorkingDir,
		Env:        env,
		GPUs:       t.GPUs,
		LogPath:    t.LogPath,
		TaskDir:    t.TaskDir,
		OnStart: func(r executor.Result) {
			if onStart != nil {
				onStart(StartInfo{PID: r.PID, StartTime: r.StartTime})
			}
		},
	}
	res, err := l.exec.Start(ctx, spec)
	if err != nil {
		return LaunchResult{}, err
	}
	return LaunchResult{ExitCode: res.ExitCode, PID: res.PID, StartTime: res.StartTime}, nil
}

// compile-time interface check
var _ Launcher = (*LocalLauncher)(nil)
