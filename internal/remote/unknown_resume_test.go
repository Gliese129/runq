package remote

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/gliese129/runq-lab/internal/config"
	"github.com/gliese129/runq-lab/internal/scheduler"
	"github.com/gliese129/runq-lab/internal/store"
)

// resumeRecorder is a TaskFinisher that also records ResumeUnknown calls —
// the shape persistDecision's optional queue-sync assertion looks for.
type resumeRecorder struct {
	finished []string
	resumed  []string
}

func (r *resumeRecorder) FinishTask(t *scheduler.Task, status scheduler.TaskStatus, extra map[string]any) {
	r.finished = append(r.finished, t.ID+":"+string(status))
}
func (r *resumeRecorder) ResumeUnknown(taskID string) { r.resumed = append(r.resumed, taskID) }

// RQ-74 review finding 1: an `unknown` task that the wrapper proves RUNNING
// must come out clean — DB status running, submit-era failure_detail
// cleared, and the scheduler queue notified (ResumeUnknown) so DB and queue
// leave unknown together.
func TestReconcileResumesUnknownToRunning(t *testing.T) {
	st, err := store.Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()

	dir := t.TempDir()
	jobID, taskID := seedTask(t, st, dir, "unknown", SourceSubmit)
	ctx := context.Background()
	// unknown tasks carry submit-era evidence and no external id.
	if err := st.UpdateTaskStatus(ctx, taskID, "unknown", map[string]any{
		"failure_detail": "submit interrupted mid-flight",
		"external_id":    nil,
	}); err != nil {
		t.Fatal(err)
	}
	// The wrapper is alive on the cluster and says so.
	if err := os.WriteFile(filepath.Join(dir, statusFileName),
		[]byte(`{"status":"running"}`), 0o644); err != nil {
		t.Fatal(err)
	}

	rec := &resumeRecorder{}
	b := &Backend{Cfg: &config.TargetConfig{}, Store: st, FS: newTestFSFromRunner(nopRunner), Finisher: rec}
	if err := b.EnsureFresh(ctx, jobID, 0); err != nil {
		t.Fatalf("EnsureFresh: %v", err)
	}

	got, _ := st.GetTask(ctx, taskID)
	if got.Status != "running" || got.StatusSource != SourceWrapper {
		t.Fatalf("status = %s/%s, want running/wrapper", got.Status, got.StatusSource)
	}
	if got.FailureDetail != "" {
		t.Errorf("submit-era failure_detail survived into running: %q", got.FailureDetail)
	}
	if len(rec.resumed) != 1 || rec.resumed[0] != taskID {
		t.Errorf("queue not synced out of unknown: resumed=%v", rec.resumed)
	}
	if len(rec.finished) != 0 {
		t.Errorf("non-terminal transition must not go through FinishTask: %v", rec.finished)
	}
}
