package scheduler

// FIFOPrioritizer ranks tasks purely by enqueue time (earliest first).
// This is the default/baseline strategy — no fairness, no prediction.
type FIFOPrioritizer struct{}

func (FIFOPrioritizer) Name() string { return "fifo" }

func (FIFOPrioritizer) Prioritize(ctx ScheduleContext) []Priority {
	// Pending tasks are already ordered by enqueue time in the Queue.
	// Just assign descending scores so the first task has the highest score.
	result := make([]Priority, len(ctx.Pending))
	for i, t := range ctx.Pending {
		result[i] = Priority{
			TaskID: t.ID,
			Score:  float64(len(ctx.Pending) - i), // first = highest
		}
	}
	return result
}
