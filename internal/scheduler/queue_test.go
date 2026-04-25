package scheduler

import (
	"testing"
)

func makeTask(id string, gpus int) *Task {
	return &Task{
		ID:         id,
		JobID:      "job-1",
		GPUsNeeded: gpus,
		Command:    "echo test",
	}
}

func TestPushAndPeek(t *testing.T) {
	q := NewQueue()
	q.Push(makeTask("t1", 1))
	q.Push(makeTask("t2", 2))

	head := q.Peek()
	if head == nil || head.ID != "t1" {
		t.Errorf("expected t1 at head, got %v", head)
	}
	if head.Status != StatusPending {
		t.Errorf("expected pending, got %s", head.Status)
	}
}

func TestPeekEmpty(t *testing.T) {
	q := NewQueue()
	if q.Peek() != nil {
		t.Error("expected nil for empty queue")
	}
}

func TestPeekSchedulable(t *testing.T) {
	q := NewQueue()
	q.Push(makeTask("t-big", 8))   // needs 8 GPUs
	q.Push(makeTask("t-small", 1)) // needs 1 GPU

	// Only 2 GPUs free — big task can't run, but small one can
	task := q.PeekSchedulable(2, nil)
	if task == nil || task.ID != "t-small" {
		t.Errorf("expected t-small, got %v", task)
	}

	// 0 GPUs free — nothing fits
	task = q.PeekSchedulable(0, nil)
	if task != nil {
		t.Errorf("expected nil, got %s", task.ID)
	}
}

func TestMarkRunning(t *testing.T) {
	q := NewQueue()
	q.Push(makeTask("t1", 1))

	if err := q.MarkRunning("t1"); err != nil {
		t.Fatalf("MarkRunning failed: %v", err)
	}

	task := q.Get("t1")
	if task.Status != StatusRunning {
		t.Errorf("expected running, got %s", task.Status)
	}
	if task.StartedAt == nil {
		t.Error("expected StartedAt to be set")
	}

	// Peek should skip running tasks
	if q.Peek() != nil {
		t.Error("Peek should return nil when only task is running")
	}
}

func TestMarkRunningNotFound(t *testing.T) {
	q := NewQueue()
	if err := q.MarkRunning("nope"); err == nil {
		t.Error("expected error for missing task")
	}
}

func TestComplete(t *testing.T) {
	q := NewQueue()
	q.Push(makeTask("t1", 1))
	q.MarkRunning("t1")

	if err := q.Complete("t1", StatusSuccess); err != nil {
		t.Fatalf("Complete failed: %v", err)
	}

	task := q.Get("t1")
	if task.Status != StatusSuccess {
		t.Errorf("expected success, got %s", task.Status)
	}
	if task.FinishedAt == nil {
		t.Error("expected FinishedAt to be set")
	}
}

func TestRequeue(t *testing.T) {
	q := NewQueue()
	q.Push(makeTask("t1", 1))
	q.MarkRunning("t1")
	q.Complete("t1", StatusFailed)

	if err := q.Requeue("t1"); err != nil {
		t.Fatalf("Requeue failed: %v", err)
	}

	task := q.Get("t1")
	if task.Status != StatusPending {
		t.Errorf("expected pending, got %s", task.Status)
	}
	if task.RetryCount != 1 {
		t.Errorf("expected retry count 1, got %d", task.RetryCount)
	}
	if task.GPUs != nil {
		t.Error("expected GPUs to be cleared")
	}
}

func TestRetryExistingUpdatesInPlace(t *testing.T) {
	q := NewQueue()
	task := makeTask("t1", 1)
	q.Push(task)
	q.MarkRunning("t1")
	q.Complete("t1", StatusFailed)

	updated := makeTask("t1", 2)
	updated.JobID = "job-2"
	updated.RetryCount = 3
	updated.MaxRetry = 5
	updated.GPUs = []int{0}
	if !q.RetryExisting(updated) {
		t.Fatal("expected existing task to be updated")
	}
	if q.Len() != 1 {
		t.Fatalf("expected queue length 1, got %d", q.Len())
	}

	got := q.Get("t1")
	if got.Status != StatusPending {
		t.Fatalf("expected pending, got %s", got.Status)
	}
	if got.JobID != "job-2" {
		t.Fatalf("expected job-2, got %s", got.JobID)
	}
	if got.GPUs != nil {
		t.Fatal("expected GPUs to be cleared")
	}
	if got.RetryCount != 3 || got.MaxRetry != 5 || got.GPUsNeeded != 2 {
		t.Fatalf("unexpected retry task: %+v", got)
	}
}

func TestListByStatus(t *testing.T) {
	q := NewQueue()
	q.Push(makeTask("t1", 1))
	q.Push(makeTask("t2", 1))
	q.Push(makeTask("t3", 1))
	q.MarkRunning("t2")

	pending := q.ListByStatus(StatusPending)
	if len(pending) != 2 {
		t.Errorf("expected 2 pending, got %d", len(pending))
	}

	running := q.ListByStatus(StatusRunning)
	if len(running) != 1 || running[0].ID != "t2" {
		t.Errorf("expected 1 running (t2), got %v", running)
	}
}

func TestListByJob(t *testing.T) {
	q := NewQueue()
	t1 := makeTask("t1", 1)
	t1.JobID = "job-A"
	t2 := makeTask("t2", 1)
	t2.JobID = "job-B"
	t3 := makeTask("t3", 1)
	t3.JobID = "job-A"
	q.Push(t1)
	q.Push(t2)
	q.Push(t3)

	jobA := q.ListByJob("job-A")
	if len(jobA) != 2 {
		t.Errorf("expected 2 tasks in job-A, got %d", len(jobA))
	}
}

func TestPushBatch(t *testing.T) {
	q := NewQueue()
	tasks := []*Task{makeTask("t1", 1), makeTask("t2", 2), makeTask("t3", 4)}
	q.PushBatch(tasks)

	if q.Len() != 3 {
		t.Errorf("expected 3 tasks, got %d", q.Len())
	}
	if q.PendingCount() != 3 {
		t.Errorf("expected 3 pending, got %d", q.PendingCount())
	}
}
