package scheduler

// SchedulePolicy scores how well a task fits on available GPUs.
// Higher score = better fit. Used by L2 group-based scheduling
// to replace the default FIFO + aging policy.
type SchedulePolicy interface {
	Score(task *Task, available []GPUState) float64
}
