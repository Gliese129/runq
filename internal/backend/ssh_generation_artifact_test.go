package backend

import (
	"context"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gliese129/runq-lab/internal/config"
	"github.com/gliese129/runq-lab/internal/logfile"
	"github.com/gliese129/runq-lab/internal/rfs"
	"github.com/gliese129/runq-lab/internal/store"
)

func seedArtifactTask(t *testing.T, st *store.Store, jobID, taskID, generation, taskDir, logPath string) {
	t.Helper()
	ctx := context.Background()
	if err := st.InsertJob(ctx, &store.JobRow{
		ID: jobID, ProjectName: "p", Status: "done", TotalTasks: 1,
		Target: "hpc", CreatedAt: time.Now(),
	}); err != nil {
		t.Fatal(err)
	}
	finished := time.Now()
	if err := st.InsertTask(ctx, &store.TaskRow{
		ID: taskID, JobID: jobID, ProjectName: "p", Command: "true", ParamsJSON: "{}",
		Status: "success", Target: "hpc", TargetGeneration: generation,
		TaskDir: taskDir, LogPath: logPath, EnqueuedAt: time.Now(), FinishedAt: &finished,
	}); err != nil {
		t.Fatal(err)
	}
}

func TestSSHPointArtifactReadsRejectMovedGeneration(t *testing.T) {
	st, err := store.Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	oldLane := newFreshnessSSHBackend(t, st, "old-submit {{run_sh}}")
	newLane := newFreshnessSSHBackend(t, st, "new-submit {{run_sh}}")
	if oldLane.Generation() == newLane.Generation() {
		t.Fatal("test lanes unexpectedly share a generation")
	}
	dir := t.TempDir()
	logPath := dir + "/stdout.log"
	seedArtifactTask(t, st, "job-moved-read", "task-moved-read", newLane.Generation(), dir, logPath)
	ctx := context.Background()

	reads := []struct {
		name string
		call func() error
	}{
		{name: "task detail", call: func() error { _, err := oldLane.GetTask(ctx, "task-moved-read"); return err }},
		{name: "metrics", call: func() error { _, err := oldLane.TaskMetrics(ctx, "task-moved-read", 0); return err }},
		{name: "metric buckets", call: func() error {
			_, _, err := oldLane.TaskMetricBuckets(ctx, "task-moved-read", "loss", 0, 0, 10)
			return err
		}},
		{name: "log read", call: func() error { _, err := oldLane.TaskLogRead(ctx, "task-moved-read", 0, 20); return err }},
		{name: "log tail", call: func() error { _, err := oldLane.TaskLogTail(ctx, "task-moved-read", 20); return err }},
		{name: "log page", call: func() error {
			_, err := oldLane.TaskLogPage(ctx, "task-moved-read", logfile.PageRequest{Offset: 0})
			return err
		}},
		{name: "log follow", call: func() error {
			follower, err := oldLane.TaskLogFollow(ctx, "task-moved-read", 0)
			if follower != nil {
				_ = follower.Close()
			}
			return err
		}},
	}
	for _, read := range reads {
		t.Run(read.name, func(t *testing.T) {
			err := read.call()
			if err == nil || !strings.Contains(err.Error(), "no longer owns") {
				t.Fatalf("read error = %v, want generation ownership rejection", err)
			}
		})
	}
}

type delayedMetricsLane struct {
	Backend
	inner      *SSHBackend
	generation string
	entered    chan struct{}
	proceed    chan struct{}
}

func (l *delayedMetricsLane) Generation() string { return l.generation }
func (l *delayedMetricsLane) FS() rfs.FS         { return l.inner.FS() }
func (l *delayedMetricsLane) TaskMetrics(ctx context.Context, taskID string, afterTS int64) ([]MetricPoint, error) {
	close(l.entered)
	<-l.proceed
	return l.inner.TaskMetrics(ctx, taskID, afterTS)
}

func TestMultiPointReadRechecksOwnershipAfterResolution(t *testing.T) {
	st, err := store.Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	oldLane := newFreshnessSSHBackend(t, st, "old-submit {{run_sh}}")
	newLane := newFreshnessSSHBackend(t, st, "new-submit {{run_sh}}")
	dir := t.TempDir()
	seedArtifactTask(t, st, "job-raced-read", "task-raced-read", oldLane.Generation(), dir, dir+"/stdout.log")
	delayed := &delayedMetricsLane{
		Backend: oldLane, inner: oldLane, generation: oldLane.Generation(),
		entered: make(chan struct{}), proceed: make(chan struct{}),
	}
	m, err := NewMultiBackend(map[string]Backend{"hpc": newLane}, st, "hpc")
	if err != nil {
		t.Fatal(err)
	}
	m.SetHistoricalLane("hpc", oldLane.Generation(), delayed)

	errCh := make(chan error, 1)
	go func() {
		_, err := m.TaskMetrics(context.Background(), "task-raced-read", 0)
		errCh <- err
	}()
	<-delayed.entered // Multi already resolved the old generation.
	if err := st.WithTaskAttemptLock("task-raced-read", func() error {
		if err := st.RestampTask(context.Background(), "task-raced-read", newLane.Generation()); err != nil {
			return err
		}
		close(delayed.proceed)
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-errCh:
		if err == nil || !strings.Contains(err.Error(), "no longer owns") {
			t.Fatalf("raced read error = %v, want ownership rejection", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("raced point read did not complete")
	}
}

type closeCountingFS struct {
	rfs.FS
	closes atomic.Int32
}

func (f *closeCountingFS) Close() error {
	f.closes.Add(1)
	return nil
}

func TestLogFollowerPinsLaneTransportUntilClose(t *testing.T) {
	st, err := store.Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	fsys := &closeCountingFS{FS: rfs.NewLocalFS()}
	be, err := NewSSHBackend(SSHBackendConfig{
		Target: config.TargetConfig{
			Name: "hpc", SubmitTemplate: "submit {{run_sh}}", SubmitIDRegex: `([0-9]+)`, MaxInflight: 1,
		},
		Store: st,
		FS:    fsys,
	})
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	logPath := dir + "/stdout.log"
	seedArtifactTask(t, st, "job-follow-lease", "task-follow-lease", be.Generation(), dir, logPath)
	follower, err := be.TaskLogFollow(context.Background(), "task-follow-lease", 0)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = follower.Close()
		_ = be.Close()
	})

	if err := be.Close(); err != nil {
		t.Fatal(err)
	}
	if got := fsys.closes.Load(); got != 0 {
		t.Fatalf("transport closed with live follower: %d", got)
	}
	if err := follower.Close(); err != nil {
		t.Fatal(err)
	}
	if got := fsys.closes.Load(); got != 1 {
		t.Fatalf("transport close count after follower release = %d, want 1", got)
	}
	if err := be.Close(); err != nil {
		t.Fatal(err)
	}
	if got := fsys.closes.Load(); got != 1 {
		t.Fatalf("idempotent close count = %d, want 1", got)
	}
}
