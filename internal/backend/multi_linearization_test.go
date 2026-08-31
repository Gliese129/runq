package backend

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/gliese129/runq-lab/internal/store"
)

type blockingControlLane struct {
	*UnavailableBackend
	store        *store.Store
	generation   string
	retryEntered chan struct{}
	retryRelease chan struct{}
	killCalls    int
}

func (l *blockingControlLane) Generation() string { return l.generation }

func (l *blockingControlLane) RetryTask(ctx context.Context, taskID string) error {
	close(l.retryEntered)
	<-l.retryRelease
	return l.store.RestampTask(ctx, taskID, l.generation)
}

func (l *blockingControlLane) KillTask(context.Context, string) error {
	l.killCalls++
	return nil
}

func TestRetryAndKillReResolveUnderOneControlOrder(t *testing.T) {
	st, err := store.Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	ctx := context.Background()
	if err := st.InsertJob(ctx, &store.JobRow{
		ID: "job-control-order", ProjectName: "p", Status: "failed", TotalTasks: 1,
		Target: "hpc", CreatedAt: time.Now(),
	}); err != nil {
		t.Fatal(err)
	}
	if err := st.InsertTask(ctx, &store.TaskRow{
		ID: "task-control-order", JobID: "job-control-order", ProjectName: "p",
		Command: "true", ParamsJSON: "{}", Status: "failed", Target: "hpc",
		TargetGeneration: "old", EnqueuedAt: time.Now(),
	}); err != nil {
		t.Fatal(err)
	}
	active := &blockingControlLane{
		UnavailableBackend: NewUnavailableBackend(errors.New("unused")), store: st, generation: "new",
		retryEntered: make(chan struct{}), retryRelease: make(chan struct{}),
	}
	old := &blockingControlLane{
		UnavailableBackend: NewUnavailableBackend(errors.New("unused")), store: st, generation: "old",
		retryEntered: make(chan struct{}), retryRelease: make(chan struct{}),
	}
	m, err := NewMultiBackend(map[string]Backend{"hpc": active}, st, "hpc")
	if err != nil {
		t.Fatal(err)
	}
	m.SetRetiringLane("hpc", "old", old)

	retryDone := make(chan error, 1)
	go func() { retryDone <- m.RetryTaskGen(ctx, "task-control-order", true) }()
	<-active.retryEntered
	killDone := make(chan error, 1)
	go func() { killDone <- m.KillTask(ctx, "task-control-order") }()
	select {
	case err := <-killDone:
		t.Fatalf("kill bypassed in-flight retry fence: %v", err)
	case <-time.After(30 * time.Millisecond):
	}
	close(active.retryRelease)
	if err := <-retryDone; err != nil {
		t.Fatal(err)
	}
	if err := <-killDone; err != nil {
		t.Fatal(err)
	}
	if active.killCalls != 1 || old.killCalls != 0 {
		t.Fatalf("kill calls active/old = %d/%d, want post-retry active owner", active.killCalls, old.killCalls)
	}
}

func TestRoutingUpdateLinearizesForceRefreshToReplacement(t *testing.T) {
	st, err := store.Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	old := newGenerationTestLane()
	old.forceReceipt = &RefreshReceipt{Refreshed: true}
	replacement := newGenerationTestLane()
	replacement.forceReceipt = &RefreshReceipt{Refreshed: true}
	m := newGenerationMulti(t, st, old)

	release := m.BeginRoutingUpdate()
	done := make(chan error, 1)
	go func() {
		_, err := m.ForceRefreshTarget(context.Background(), "hpc")
		done <- err
	}()
	select {
	case err := <-done:
		t.Fatalf("refresh crossed an unfinished routing update: %v", err)
	case <-time.After(30 * time.Millisecond):
	}
	m.SetTarget("hpc", replacement)
	release()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if old.forceCalls != 0 || replacement.forceCalls != 1 {
		t.Fatalf("force calls old/replacement = %d/%d, want replacement-only snapshot", old.forceCalls, replacement.forceCalls)
	}
}
