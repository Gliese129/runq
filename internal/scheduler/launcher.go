package scheduler

import (
	"context"
	"errors"
)

// ErrLaunchUntracked is returned (wrapped) by remote launchers when the
// submission STARTED but its outcome is unknown: the submit command was
// interrupted mid-flight, or it succeeded but runq failed to record the
// external id. The task must NOT be retried — a retry could submit a second
// cluster job while the first may be running untracked. The scheduler marks
// such tasks `unknown` (RQ-74) with the error as visible evidence; reconcile
// settles them from facts (status.json marker / scheduler probe).
var ErrLaunchUntracked = errors.New("task launch outcome unknown")

// ErrLaunchTransient is returned (wrapped) by remote launchers when the
// submission NEVER REACHED the scheduler (transport/file layer: SSH down,
// runqd not yet listening, workspace unreadable). This is not the task's
// failure: it returns to pending without consuming retry budget and the next
// tick tries again. Contrast with a scheduler REJECTION (command ran, exit
// non-zero) — that is deterministic and fails the task permanently.
var ErrLaunchTransient = errors.New("scheduler unreachable")

// Launcher hands a task to a remote execution service and cancels it.
// Launch returns after handoff; reconcile later supplies the terminal
// transition through Scheduler.FinishTask.
type Launcher interface {
	Launch(ctx context.Context, t *Task) error

	// Kill reports whether the remote cancellation request was accepted.
	Kill(taskID string) error
}
