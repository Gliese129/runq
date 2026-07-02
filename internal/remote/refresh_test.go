package remote

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/gliese129/runq/internal/config"
	"github.com/gliese129/runq/internal/store"
)

// seedTask inserts a project + job + one task with a chosen status/source, for
// exercising EnsureFresh's source-based terminal handling directly.
func seedTask(t *testing.T, st *store.Store, taskDir, status, source string) (jobID, taskID string) {
	t.Helper()
	ctx := context.Background()
	if _, err := st.DB().ExecContext(ctx, `INSERT INTO projects(name, config_json) VALUES('p','{}')`); err != nil {
		t.Fatal(err)
	}
	jobID, taskID = "j1", "t1"
	job := store.JobRow{ID: jobID, ProjectName: "p", ConfigJSON: "{}", Status: "running", TotalTasks: 1, CreatedAt: time.Now()}
	if err := st.InsertJob(ctx, &job); err != nil {
		t.Fatalf("InsertJob: %v", err)
	}
	row := store.TaskRow{
		ID: taskID, JobID: jobID, ProjectName: "p", Command: "c", ParamsJSON: "{}",
		Status: status, StatusSource: source, ExternalID: "55", TaskDir: taskDir, EnqueuedAt: time.Now(),
	}
	if err := st.InsertTask(ctx, &row); err != nil {
		t.Fatalf("InsertTask: %v", err)
	}
	return
}

func nopRunner(ctx context.Context, command string) (string, error) { return "", nil }

// An "inferred" terminal (zombie guess) must be correctable: when the wrapper's
// real terminal later appears, EnsureFresh adopts it (status + source change).
func TestRefreshInferredCorrectedByWrapper(t *testing.T) {

	st, err := store.Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()

	dir := t.TempDir()
	jobID, taskID := seedTask(t, st, dir, "failed", SourceInferred)
	if err := os.WriteFile(filepath.Join(dir, statusFileName),
		[]byte(`{"status":"success","exit_code":0,"finished_at":1730000000}`), 0o644); err != nil {
		t.Fatal(err)
	}

	b := &Backend{Cfg: &config.TargetConfig{}, Store: st, FS: newTestFSFromRunner(nopRunner)}
	if err := b.EnsureFresh(context.Background(), jobID, 0); err != nil {
		t.Fatalf("EnsureFresh: %v", err)
	}
	got, _ := st.GetTask(context.Background(), taskID)
	if got.Status != "success" || got.StatusSource != SourceWrapper {
		t.Fatalf("inferred-failed not corrected: status=%s source=%s", got.Status, got.StatusSource)
	}
}

// Ingest error must not prevent subsequent within-TTL calls from running the
// local reconcile. With two-tier design, the probe cache only controls the
// scheduler probe — local reads (status.json, metrics) always execute.
func TestIngestErrorDoesNotCacheAsFresh(t *testing.T) {

	st, err := store.Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()

	dir := t.TempDir()
	jobID, taskID := seedTask(t, st, dir, "pending", SourceSubmit)

	probes := 0
	runner := func(ctx context.Context, command string) (string, error) {
		probes++
		return "RUNNING", nil
	}
	b := &Backend{
		Cfg:   &config.TargetConfig{StatusTemplate: "checkstat {{ext_id}}"},
		Store: st,
		FS:    newTestFSFromRunner(runner),
	}

	// Write an unreadable metrics.jsonl to trigger an ingest error.
	metricsPath := filepath.Join(dir, "metrics.jsonl")
	if err := os.WriteFile(metricsPath, []byte(`{"type":"metric","key":"loss","value":0.5,"step":1,"ts":1700000000}`), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(metricsPath, 0o000); err != nil {
		t.Fatal(err)
	}
	// If running as root, chmod 0 won't actually block reads. Skip.
	if _, err := os.Open(metricsPath); err == nil {
		_ = os.Chmod(metricsPath, 0o644)
		t.Skip("running as root, chmod test ineffective")
	}

	// First call (force): reconcile runs, hits ingest error, but probe
	// succeeds — probe cache populated. Error message mentions ingest.
	err = b.EnsureFresh(context.Background(), jobID, 0)
	if err == nil || !strings.Contains(err.Error(), "ingest") {
		t.Fatalf("expected ingest error, got: %v", err)
	}
	if probes != 1 {
		t.Fatalf("expected 1 probe, got %d", probes)
	}

	// Fix the metrics file and write status.json.
	_ = os.Chmod(metricsPath, 0o644)
	if err := os.WriteFile(filepath.Join(dir, statusFileName),
		[]byte(`{"status":"success","exit_code":0,"finished_at":1730000000}`), 0o644); err != nil {
		t.Fatal(err)
	}

	// Second call within TTL: must NOT probe again (cache hit), but MUST run
	// local reconcile — picking up status.json + retrying metrics ingest.
	err = b.EnsureFresh(context.Background(), jobID, 30*time.Second)
	if err != nil {
		t.Fatalf("within-TTL call should succeed now: %v", err)
	}
	if probes != 1 {
		t.Errorf("within-TTL call should not probe, got %d total probes", probes)
	}

	got, _ := st.GetTask(context.Background(), taskID)
	if got.Status != "success" {
		t.Errorf("within-TTL call must pick up wrapper status: got %s, want success", got.Status)
	}
	metrics, _ := st.ListMetrics(context.Background(), taskID, "loss")
	if len(metrics) != 1 {
		t.Errorf("within-TTL call must re-ingest fixed metrics: got %d, want 1", len(metrics))
	}
}

// A HARD terminal (source=wrapper) is final: a conflicting late status.json must
// not override it (EnsureFresh skips it).
func TestRefreshHardTerminalNotReevaluated(t *testing.T) {

	st, err := store.Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()

	dir := t.TempDir()
	jobID, taskID := seedTask(t, st, dir, "success", SourceWrapper)
	if err := os.WriteFile(filepath.Join(dir, statusFileName),
		[]byte(`{"status":"failed","exit_code":1}`), 0o644); err != nil {
		t.Fatal(err)
	}

	b := &Backend{Cfg: &config.TargetConfig{}, Store: st, FS: newTestFSFromRunner(nopRunner)}
	if err := b.EnsureFresh(context.Background(), jobID, 0); err != nil {
		t.Fatalf("EnsureFresh: %v", err)
	}
	got, _ := st.GetTask(context.Background(), taskID)
	if got.Status != "success" {
		t.Fatalf("hard terminal was overridden to %s", got.Status)
	}
}
