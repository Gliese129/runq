package app

import (
	"context"
	"io"
	"log/slog"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gliese129/runq-lab/internal/backend"
	"github.com/gliese129/runq-lab/internal/store"
)

type shutdownLane struct {
	backend.Backend
	closeOnce sync.Once
	closed    chan struct{}
	closes    atomic.Int32
}

func newShutdownLane() *shutdownLane {
	return &shutdownLane{closed: make(chan struct{})}
}

func (l *shutdownLane) Close() error {
	l.closes.Add(1)
	l.closeOnce.Do(func() { close(l.closed) })
	return nil
}

type blockingQuiesceLane struct {
	*shutdownLane
	generation     string
	quiesceStarted chan struct{}
	allowQuiesce   chan struct{}
}

func (l *blockingQuiesceLane) Generation() string          { return l.generation }
func (l *blockingQuiesceLane) MarkRetiring()               {}
func (l *blockingQuiesceLane) HasInFlightAdmissions() bool { return false }

func (l *blockingQuiesceLane) QuiesceForHistory() {
	close(l.quiesceStarted)
	<-l.allowQuiesce
}

func newShutdownTestDaemon(t *testing.T) *Daemon {
	t.Helper()
	st, err := store.Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	return &Daemon{
		Logger:          slog.New(slog.NewTextHandler(io.Discard, nil)),
		Store:           st,
		lanes:           map[string]backend.Backend{},
		retiringLanes:   map[string]backend.Backend{},
		historicalLanes: map[string]backend.Backend{},
	}
}

func waitForShutdown(t *testing.T, done <-chan struct{}) {
	t.Helper()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("shutdown did not complete")
	}
}

func TestShutdownWaitsForReconcilePublication(t *testing.T) {
	d := newShutdownTestDaemon(t)
	lane := newShutdownLane()
	transitionStarted := make(chan struct{})
	allowPublish := make(chan struct{})

	go func() {
		d.reconcileMu.Lock()
		close(transitionStarted)
		<-allowPublish
		d.laneMu.Lock()
		d.lanes["late"] = lane
		d.laneMu.Unlock()
		d.reconcileMu.Unlock()
	}()
	<-transitionStarted

	shutdownDone := make(chan struct{})
	go func() {
		d.Shutdown(context.Background())
		close(shutdownDone)
	}()

	select {
	case <-shutdownDone:
		t.Fatal("shutdown returned before the in-flight reconcile published its lane")
	case <-lane.closed:
		t.Fatal("shutdown closed a lane before the reconcile published it")
	case <-time.After(50 * time.Millisecond):
	}

	close(allowPublish)
	waitForShutdown(t, shutdownDone)
	if got := lane.closes.Load(); got != 1 {
		t.Fatalf("late-published lane close count = %d, want 1", got)
	}

	// Shutdown is terminal but remains idempotent for callers racing to stop.
	d.Shutdown(context.Background())
}

func TestShutdownDoesNotCloseLaneDuringRetirementQuiesce(t *testing.T) {
	dir := t.TempDir()
	writeCfg(t, dir, baseCfg)
	d, _, _ := newReconcilerHarness(t, dir)
	original := d.lanes["a"].(*fakeLane)
	lane := &blockingQuiesceLane{
		shutdownLane:   newShutdownLane(),
		generation:     original.gen,
		quiesceStarted: make(chan struct{}),
		allowQuiesce:   make(chan struct{}),
	}
	d.lanes["a"] = lane
	d.multiBe.SetTarget("a", lane)

	writeCfg(t, dir, `default_target: a
targets:
  - name: a
    scheduler: slurm
    submit_template: changed
`)
	if err := d.ReconcileConfig("test"); err != nil {
		t.Fatal(err)
	}
	// ReconcileConfig's final sweep supplied the first zero and armed the
	// confirmation window. This sweep performs the quiesce transition.

	sweepDone := make(chan struct{})
	go func() {
		d.SweepRetiringLanes()
		close(sweepDone)
	}()
	<-lane.quiesceStarted

	shutdownDone := make(chan struct{})
	go func() {
		d.Shutdown(context.Background())
		close(shutdownDone)
	}()

	select {
	case <-lane.closed:
		t.Fatal("shutdown closed a lane concurrently with retirement quiesce")
	case <-shutdownDone:
		t.Fatal("shutdown returned while retirement quiesce was in flight")
	case <-time.After(50 * time.Millisecond):
	}

	close(lane.allowQuiesce)
	select {
	case <-sweepDone:
	case <-time.After(2 * time.Second):
		t.Fatal("retirement sweep did not complete")
	}
	waitForShutdown(t, shutdownDone)
	if got := lane.closes.Load(); got != 1 {
		t.Fatalf("quiesced lane close count = %d, want 1", got)
	}
}
