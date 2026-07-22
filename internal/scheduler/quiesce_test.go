package scheduler

import (
	"context"
	"testing"
	"time"
)

// RQ-75 #4: Quiesce turns tick into a no-op — a fenced lane must not
// dispatch NEW tasks while its replacement is being brought up.
func TestQuiesceStopsDispatch(t *testing.T) {
	fl := &fakeRemoteLauncher{}
	s, q, _ := newKillRaceHarness(t, fl)
	task := &Task{ID: "t-q", JobID: "j-q", ProjectName: "test", Command: "true", GPUsNeeded: 1}
	seedJob(t, s.store, task.JobID, "test", 1)
	seedTask(t, s.store, task)
	q.Push(task)

	s.Quiesce()
	s.tick()

	if qt := q.Get(task.ID); qt == nil || qt.Status != StatusPending {
		t.Fatalf("quiesced tick dispatched: %+v", qt)
	}
	if len(fl.launched) != 0 {
		t.Fatalf("quiesced tick launched %v", fl.launched)
	}
}

// DrainLaunches waits for the in-flight dispatch goroutine to settle and
// then reports drained; a blocked launch beyond the deadline reports false.
func TestDrainLaunchesWaitsForInflight(t *testing.T) {
	gate := make(chan struct{})
	fl := &fakeRemoteLauncher{gate: gate}
	s, q, _ := newKillRaceHarness(t, fl)
	task := &Task{ID: "t-d", JobID: "j-d", ProjectName: "test", Command: "true", GPUsNeeded: 1}
	seedJob(t, s.store, task.JobID, "test", 1)
	seedTask(t, s.store, task)
	q.Push(task)

	s.tick() // dispatches; launch goroutine blocks on the gate
	s.Quiesce()

	// Launch still in flight → bounded drain must time out honestly.
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	if s.DrainLaunches(ctx) {
		t.Fatal("drain reported success while a launch was still in flight")
	}
	cancel()

	// Gate opens (submission completes) → drain succeeds.
	close(gate)
	ctx2, cancel2 := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel2()
	if !s.DrainLaunches(ctx2) {
		t.Fatal("drain did not complete after the launch settled")
	}
	if len(fl.launched) != 1 {
		t.Fatalf("launched = %v, want the one in-flight task", fl.launched)
	}
}
