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

// blockingPrioritizer lets a test hold a tick mid-flight: Prioritize
// signals `entered` then blocks until `gate` closes — simulating a tick
// that passed the quiesced check and lost the CPU before dispatching.
type blockingPrioritizer struct {
	entered chan struct{}
	gate    chan struct{}
	inner   Prioritizer
}

func (p *blockingPrioritizer) Prioritize(ctx ScheduleContext) []Priority {
	close(p.entered)
	<-p.gate
	return p.inner.Prioritize(ctx)
}
func (p *blockingPrioritizer) Name() string { return "blocking-test" }

// Review #4 follow-up: Quiesce must be a BARRIER against in-flight ticks.
// A tick that already passed the quiesced check holds the read lock;
// Quiesce (write lock) must not return until that tick — including its
// synchronous inflight.Add — has finished. Otherwise Quiesce+Drain can
// observe a zero count and declare drained, and the old tick submits
// afterwards: the double-submit window reopened.
func TestQuiesceBarriersInflightTick(t *testing.T) {
	bp := &blockingPrioritizer{
		entered: make(chan struct{}),
		gate:    make(chan struct{}),
		inner:   FIFOPrioritizer{},
	}
	fl := &fakeRemoteLauncher{}
	q := NewQueue()
	s := New(DefaultConfig(), q, testPool(1), fl, testStore(t), testLogger(), bp, "", nil)
	task := &Task{ID: "t-b", JobID: "j-b", ProjectName: "test", Command: "true", GPUsNeeded: 1}
	seedJob(t, s.store, task.JobID, "test", 1)
	seedTask(t, s.store, task)
	q.Push(task)

	// A tick is mid-flight: past the quiesced check, blocked in Prioritize.
	tickDone := make(chan struct{})
	go func() {
		s.tick()
		close(tickDone)
	}()
	<-bp.entered

	// Quiesce must BLOCK while that tick runs.
	quiesceDone := make(chan struct{})
	go func() {
		s.Quiesce()
		close(quiesceDone)
	}()
	select {
	case <-quiesceDone:
		t.Fatal("Quiesce returned while a tick was mid-flight — no barrier")
	case <-time.After(100 * time.Millisecond):
		// expected: still blocked
	}

	// Let the tick finish; Quiesce is granted; drain then sees the
	// dispatch the old tick made.
	close(bp.gate)
	<-tickDone
	select {
	case <-quiesceDone:
	case <-time.After(2 * time.Second):
		t.Fatal("Quiesce never returned after the tick finished")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if !s.DrainLaunches(ctx) {
		t.Fatal("drain did not observe/settle the old tick's dispatch")
	}
	if len(fl.launched) != 1 {
		t.Fatalf("launched = %v, want exactly the pre-quiesce dispatch", fl.launched)
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
