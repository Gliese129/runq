package remote

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gliese129/runq-lab/internal/config"
	"github.com/gliese129/runq-lab/internal/job"
	"github.com/gliese129/runq-lab/internal/project"
	"github.com/gliese129/runq-lab/internal/store"
)

// TestPrepareRefreshKill exercises durable admission, refresh, and cancellation
// against an in-memory store and a fake cluster control surface.
func TestPrepareRefreshKill(t *testing.T) {

	st, err := store.Open(":memory:")
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	defer st.Close()

	dataDir := t.TempDir()
	t.Setenv("RUNQ_DATA_DIR", dataDir) // config.ConfigDir() reads this

	cfg := &config.TargetConfig{
		SubmitTemplate: "submit {{run_sh}} gpus={{gpus}}",
		SubmitIDRegex:  `job ([0-9]+)`,
		KillTemplate:   "cancel {{ext_id}}",
		// StatusTemplate empty → status derived from status.json alone.
	}

	// The fake cluster records cancellation calls. Canonical submission itself
	// is covered by Launcher tests; this test seeds its resulting external IDs.
	var calls []string
	runner := func(ctx context.Context, command string) (string, error) {
		calls = append(calls, command)
		return "", nil
	}
	b := &Backend{Cfg: cfg, Store: st, FS: newTestFSFromRunner(runner)}

	workDir := t.TempDir()
	proj := &project.Config{
		ProjectName: "p",
		WorkingDir:  workDir,
		CmdTemplate: "python x.py --lr {{lr}}",
	}
	jobCfg := job.JobConfig{
		Project: "p",
		Sweep: []job.SweepBlock{{
			Method:     "grid",
			Parameters: map[string]job.ParameterSpec{"lr": {Values: []any{0.1, 0.2}}},
		}},
	}

	ctx := context.Background()
	jobID, rows, err := b.Prepare(ctx, jobCfg, proj, SubmitOpts{SkipPreflight: true})
	if err != nil {
		t.Fatalf("Prepare: %v", err)
	}
	if len(rows) != 2 {
		t.Fatalf("prepared %d tasks, want 2", len(rows))
	}
	externalIDs := []string{"100", "101"}
	for i, row := range rows {
		if err := st.UpdateTaskStatus(ctx, row.ID, "pending", map[string]any{"external_id": externalIDs[i]}); err != nil {
			t.Fatalf("seed external id: %v", err)
		}
	}

	tasks, err := st.ListTasks(ctx, store.TaskFilter{JobID: jobID})
	if err != nil {
		t.Fatal(err)
	}
	if len(tasks) != 2 {
		t.Fatalf("ListTasks = %d, want 2", len(tasks))
	}
	jobRoot := filepath.Join(workDir, ".runq", jobID)
	for _, tk := range tasks {
		if tk.ExternalID == "" {
			t.Errorf("task %s missing ExternalID", tk.ID)
		}
		if tk.Status != "pending" {
			t.Errorf("task %s status=%s, want pending", tk.ID, tk.Status)
		}
		if !strings.HasPrefix(tk.TaskDir, jobRoot) {
			t.Errorf("task dir %q not under %q", tk.TaskDir, jobRoot)
		}
		if _, err := os.Stat(filepath.Join(tk.TaskDir, runScriptName)); err != nil {
			t.Errorf("run.sh missing for %s: %v", tk.ID, err)
		}
	}

	// Simulate task[0] finishing successfully and emitting one metric.
	done := tasks[0]
	writeFile(t, filepath.Join(done.TaskDir, statusFileName),
		`{"status":"success","exit_code":0,"finished_at":1730000000}`)
	writeFile(t, filepath.Join(done.TaskDir, "metrics.jsonl"),
		"{\"type\":\"metric\",\"key\":\"loss\",\"value\":0.5,\"step\":1,\"ts\":1730000000}\n")

	if err := b.EnsureFresh(ctx, jobID, 0); err != nil {
		t.Fatalf("EnsureFresh: %v", err)
	}

	got, err := st.GetTask(ctx, done.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != "success" {
		t.Fatalf("after refresh, task %s status=%s, want success", done.ID, got.Status)
	}
	if got.FinishedAt == nil {
		t.Errorf("finished_at not stamped on terminal task")
	}

	metrics, err := st.ListMetricSummaries(ctx, jobID, "loss")
	if err != nil {
		t.Fatal(err)
	}
	if len(metrics) != 1 || metrics[0].Last != 0.5 || metrics[0].Count != 1 {
		t.Fatalf("summaries = %+v, want one loss last=0.5", metrics)
	}

	// EnsureFresh is idempotent: running again must not duplicate metrics.
	if err := b.EnsureFresh(ctx, jobID, 0); err != nil {
		t.Fatalf("EnsureFresh #2: %v", err)
	}
	if m, _ := st.ListMetricSummaries(ctx, jobID, "loss"); len(m) != 1 || m[0].Count != 1 {
		t.Fatalf("summaries duplicated after second refresh: %+v", m)
	}

	// Kill the other task: scancel issued with its ext id, DB marked killed.
	other := tasks[1]
	killed, err := b.Kill(ctx, other.ID)
	if err != nil {
		t.Fatalf("Kill: %v", err)
	}
	if killed != 1 {
		t.Fatalf("killed %d, want 1", killed)
	}
	gotOther, _ := st.GetTask(ctx, other.ID)
	if gotOther.Status != "killed" {
		t.Fatalf("task %s status=%s, want killed", other.ID, gotOther.Status)
	}
	foundCancel := false
	for _, c := range calls {
		if strings.HasPrefix(c, "cancel") && strings.Contains(c, other.ExternalID) {
			foundCancel = true
		}
	}
	if !foundCancel {
		t.Errorf("kill_template not invoked with ext id %q; calls=%v", other.ExternalID, calls)
	}
}

func writeFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}
