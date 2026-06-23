package scheduler

import (
	"cmp"
	"context"
	"maps"
	"slices"
	"time"

	"github.com/gliese129/runq/internal/store"
	"github.com/gliese129/runq/internal/utils"
)

// FairSharePrioritizer ranks tasks by per-user GPU-hours fairness.
// Users who have consumed fewer GPU-hours in the sliding window get higher priority.
type FairSharePrioritizer struct {
	Store  *store.Store
	Window time.Duration // sliding window for GPU-hour accounting (e.g. 24h)
}

func (f *FairSharePrioritizer) Name() string { return "fair" }

func (f *FairSharePrioritizer) Prioritize(ctx ScheduleContext) []Priority {
	// Score each user on three normalized dimensions:
	//   1. Pending demand   (weight -0.1): total GPUs requested by queued tasks
	//   2. Running occupation (weight -0.3): GPU-seconds consumed by in-flight tasks
	//   3. Historical usage   (weight -0.5): GPU-seconds consumed in sliding window (finished only)
	//
	// Combined score = -0.1*pending - 0.3*running - 0.5*consumed
	// Lower historical consumption → higher score → scheduled first.
	// Within the same user, tasks are ordered FIFO by EnqueuedAt.
	userPending, userRunning := make(map[int]float64), make(map[int]float64)
	userConsumed := f.computeUsage(ctx.Ctx, ctx.Now)
	for _, task := range ctx.Pending {
		_, ok := userPending[task.UID]
		if !ok {
			userPending[task.UID] = 0
		}
		userPending[task.UID] += float64(task.GPUsNeeded)
	}
	for _, task := range ctx.Running {
		_, ok := userRunning[task.UID]
		if !ok {
			userRunning[task.UID] = 0
		}
		userRunning[task.UID] += float64(task.GPUsNeeded) * (ctx.Now.Sub(task.StartTime)).Seconds()
	}
	// Normalize all dimensions to [0,1] so weights are comparable.
	userPending = utils.Normalization(userPending)
	userRunning = utils.Normalization(userRunning)
	userConsumed = utils.Normalization(userConsumed)

	users := slices.Collect(maps.Keys(userPending))
	userScores := make(map[int]float64)
	for _, k := range users {
		userScores[k] = -0.1*userPending[k] - 0.3*userRunning[k] - 0.5*userConsumed[k]
	}
	// Sort tasks: inter-user by score (descending), intra-user by enqueue time (FIFO).
	tasks := make([]*Task, len(ctx.Pending))
	copy(tasks, ctx.Pending)
	slices.SortFunc(tasks, func(a, b *Task) int {
		if a.UID != b.UID {
			return cmp.Compare(userScores[b.UID], userScores[a.UID])
		}
		return cmp.Compare(a.EnqueuedAt.Unix(), b.EnqueuedAt.Unix())
	})

	result := make([]Priority, len(ctx.Pending))
	for i, t := range tasks {
		result[i] = Priority{
			TaskID: t.ID,
			Score:  float64(len(tasks) - i), // descending: first task gets highest score
		}
	}
	return result
}

// computeUsage calculates per-UID GPU-seconds consumed by FINISHED tasks in [now-window, now].
// Running task occupation is handled separately by Prioritize() via ctx.Running (userRunning dimension),
// so this method intentionally excludes running tasks to avoid double-counting.
func (f *FairSharePrioritizer) computeUsage(ctx context.Context, now time.Time) map[int]float64 {
	usage := make(map[int]float64)
	if f.Store == nil {
		return usage
	}

	cutoff := now.Add(-f.Window)
	dbCtx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()

	// Finished tasks in window: sum(gpus_needed * duration) per UID.
	tasks, err := f.Store.ListFinishedTasksAfter(dbCtx, cutoff)
	if err != nil {
		return usage
	}
	for _, t := range tasks {
		if t.StartedAt == nil || t.FinishedAt == nil {
			continue
		}
		start := *t.StartedAt
		if start.Before(cutoff) {
			start = cutoff // clamp to window boundary
		}
		duration := t.FinishedAt.Sub(start).Seconds()
		usage[t.UID] += float64(t.GPUsNeeded) * duration
	}

	return usage
}
