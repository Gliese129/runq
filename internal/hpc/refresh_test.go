package hpc

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/gliese129/runq/internal/hpcconfig"
	"github.com/gliese129/runq/internal/store"
)

// seedTask inserts a project + job + one task with a chosen status/source, for
// exercising Refresh's source-based terminal handling directly.
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
// real terminal later appears, Refresh adopts it (status + source change).
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

	b := &Backend{Cfg: &hpcconfig.Config{}, Store: st, Run: nopRunner}
	if err := b.Refresh(context.Background(), jobID); err != nil {
		t.Fatalf("Refresh: %v", err)
	}
	got, _ := st.GetTask(context.Background(), taskID)
	if got.Status != "success" || got.StatusSource != SourceWrapper {
		t.Fatalf("inferred-failed not corrected: status=%s source=%s", got.Status, got.StatusSource)
	}
}

// A HARD terminal (source=wrapper) is final: a conflicting late status.json must
// not override it (Refresh skips it).
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

	b := &Backend{Cfg: &hpcconfig.Config{}, Store: st, Run: nopRunner}
	if err := b.Refresh(context.Background(), jobID); err != nil {
		t.Fatalf("Refresh: %v", err)
	}
	got, _ := st.GetTask(context.Background(), taskID)
	if got.Status != "success" {
		t.Fatalf("hard terminal was overridden to %s", got.Status)
	}
}
