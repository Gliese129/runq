package remote

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/gliese129/runq-lab/internal/config"
	"github.com/gliese129/runq-lab/internal/store"
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
	if err := os.WriteFile(metricsPath, []byte("{\"type\":\"metric\",\"key\":\"loss\",\"value\":0.5,\"step\":1,\"ts\":1700000000}\n"), 0o644); err != nil {
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
	metrics, _ := st.ListMetricSummaries(context.Background(), jobID, "loss")
	if len(metrics) != 1 {
		t.Errorf("within-TTL call must re-ingest fixed metrics: got %d summaries, want 1", len(metrics))
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

func TestPreHandoffSubmittingSkipsAllIngestionVariants(t *testing.T) {
	variants := []struct {
		name string
		run  func(context.Context, *Backend, string) error
	}{
		{
			name: "per_task",
			run: func(ctx context.Context, b *Backend, jobID string) error {
				return b.reconcileWith(ctx, jobID, false, memoRunner(b.shellRunClassified))
			},
		},
		{
			name: "batch",
			run: func(ctx context.Context, b *Backend, jobID string) error {
				return b.reconcileWithBatch(ctx, jobID, map[string]ProbeResult{})
			},
		},
	}
	phases := []struct {
		name   string
		source string
	}{
		{name: "retry_reset_intent", source: "retry"},
		{name: "scheduler_dispatch_intent", source: "submit"},
	}
	for _, variant := range variants {
		for _, phase := range phases {
			t.Run(variant.name+"/"+phase.name, func(t *testing.T) {
				st, err := store.Open(":memory:")
				if err != nil {
					t.Fatal(err)
				}
				t.Cleanup(func() { _ = st.Close() })
				dir := t.TempDir()
				jobID, taskID := seedTask(t, st, dir, "failed", SourceWrapper)
				if err := st.BeginTaskRetry(context.Background(), taskID, 0, "gen-next"); err != nil {
					t.Fatalf("begin retry: %v", err)
				}
				if phase.source == "submit" {
					if err := st.UpdateTaskStatus(context.Background(), taskID, "submitting", map[string]any{
						"status_source": "submit",
					}); err != nil {
						t.Fatalf("publish dispatch intent: %v", err)
					}
				}
				writeFile(t, filepath.Join(dir, statusFileName),
					`{"status":"failed","exit_code":1,"finished_at":1730000000}`)
				writeFile(t, filepath.Join(dir, "metrics.jsonl"),
					"{\"type\":\"metric\",\"key\":\"old_loss\",\"value\":9,\"step\":1,\"ts\":1}\n")
				writeFile(t, filepath.Join(dir, "results.jsonl"),
					"{\"ts\":1,\"axes\":{\"attempt\":\"old\"},\"metrics\":{\"score\":9}}\n")
				writeFile(t, filepath.Join(dir, "events.jsonl"),
					"{\"type\":\"checkpoint\",\"path\":\"old.pt\",\"size_bytes\":9,\"step\":1,\"ts\":1}\n")

				b := &Backend{Cfg: &config.TargetConfig{}, Store: st, FS: newTestFSFromRunner(nopRunner)}
				if err := variant.run(context.Background(), b, jobID); err != nil {
					t.Fatalf("reconcile: %v", err)
				}
				got, err := st.GetTask(context.Background(), taskID)
				if err != nil || got == nil {
					t.Fatalf("get task: %v", err)
				}
				if got.Status != "submitting" || got.StatusSource != phase.source || got.RetryCount != 1 {
					t.Fatalf("old status evidence changed pre-handoff intent: %#v", got)
				}
				var summaries, results, checkpoints, metricMarks, fileMarks int
				for _, check := range []struct {
					name string
					dest *int
					q    string
				}{
					{"summaries", &summaries, `SELECT COUNT(*) FROM metric_summary WHERE task_id = ?`},
					{"results", &results, `SELECT COUNT(*) FROM result_records WHERE task_id = ?`},
					{"checkpoints", &checkpoints, `SELECT COUNT(*) FROM checkpoints WHERE task_id = ?`},
					{"metric marks", &metricMarks, `SELECT COUNT(*) FROM metrics_ingest WHERE task_id = ?`},
					{"file marks", &fileMarks, `SELECT COUNT(*) FROM file_ingest WHERE task_id = ?`},
				} {
					if err := st.DB().QueryRow(check.q, taskID).Scan(check.dest); err != nil {
						t.Fatalf("count %s: %v", check.name, err)
					}
				}
				if summaries != 0 || results != 0 || checkpoints != 0 || metricMarks != 0 || fileMarks != 0 {
					t.Fatalf("pre-handoff evidence was ingested: summaries=%d results=%d checkpoints=%d metric_marks=%d file_marks=%d",
						summaries, results, checkpoints, metricMarks, fileMarks)
				}
			})
		}
	}
}

func TestScopedRefreshAndSchedulerProbeIgnoreUnownedJob(t *testing.T) {
	st, err := store.Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	dir := t.TempDir()
	jobID, taskID := seedTask(t, st, dir, "running", SourceScheduler)
	if _, err := st.DB().Exec(`UPDATE jobs SET target = 'lab' WHERE id = ?`, jobID); err != nil {
		t.Fatalf("stamp job target: %v", err)
	}
	if _, err := st.DB().Exec(`
		UPDATE tasks SET target = 'lab', target_generation = 'gen-owned'
		WHERE id = ?`, taskID); err != nil {
		t.Fatalf("stamp task generation: %v", err)
	}
	scope := store.NewLaneScope("lab", "gen-other")
	scope.MarkRetiring()
	probeCalls := 0
	b := &Backend{
		Cfg: &config.TargetConfig{
			Name: "lab", StatusTemplate: "status {{ext_id}}", StatusListTemplate: "status-list",
		},
		Scope: scope,
		Store: st,
		FS: newTestFSFromRunner(func(context.Context, string) (string, error) {
			probeCalls++
			return "RUNNING", nil
		}),
	}

	if err := b.EnsureFresh(context.Background(), jobID, 0); err != nil {
		t.Fatalf("scoped refresh: %v", err)
	}
	if err := b.SchedulerProbe(context.Background(), 0); err != nil {
		t.Fatalf("scoped scheduler probe: %v", err)
	}
	job, err := st.GetJob(context.Background(), jobID)
	if err != nil {
		t.Fatalf("get job: %v", err)
	}
	if job.RefreshedAt != nil {
		t.Fatalf("unowned lane touched refreshed_at: %v", job.RefreshedAt)
	}
	if probeCalls != 0 {
		t.Fatalf("unowned lane issued %d scheduler probes", probeCalls)
	}
	if b.probeIsFresh(jobID, time.Hour) || !b.lastBatchProbe.IsZero() {
		t.Fatal("unowned lane advanced its probe cache")
	}
}
