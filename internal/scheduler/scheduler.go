package scheduler

// Scheduler is the core scheduling loop.
//
// Responsibilities:
//   - Pull tasks from the queue
//   - Request GPUs from the pool
//   - Dispatch tasks to the executor
//   - Handle reservation + aging for large tasks
//
// TODO: implement
type Scheduler struct {
	// TODO: add dependencies (queue, gpu pool, executor, store)
}
