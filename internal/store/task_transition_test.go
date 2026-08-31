package store

import (
	"context"
	"errors"
	"testing"
)

func TestTaskStatusAttemptFencesPreserveManualRetry(t *testing.T) {
	st, err := Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	seedJobAndTask(t, st, "t-fence", "j-fence")
	if err := st.UpdateTaskStatus(context.Background(), "t-fence", "failed", map[string]any{
		"status_source":     "inferred",
		"external_id":       "attempt-0",
		"target_generation": "gen-old",
	}); err != nil {
		t.Fatalf("stamp old attempt: %v", err)
	}
	old, err := st.GetTask(context.Background(), "t-fence")
	if err != nil || old == nil {
		t.Fatalf("load old attempt: %v", err)
	}
	if err := st.BeginTaskRetry(context.Background(), old.ID, old.RetryCount, "gen-new"); err != nil {
		t.Fatalf("begin manual retry: %v", err)
	}

	fields := map[string]any{"status_source": "wrapper"}
	FenceTaskStatusUpdate(fields, *old)
	if err := st.UpdateTaskStatus(context.Background(), old.ID, "success", fields); !errors.Is(err, ErrTaskStatusConflict) {
		t.Fatalf("stale fenced transition error = %v, want conflict", err)
	}
	// The scheduler's automatic retry branch cannot carry the evidence map,
	// so its retry_count advancement independently fences the prior epoch.
	if err := st.UpdateTaskStatus(context.Background(), old.ID, "pending", map[string]any{
		"retry_count": old.RetryCount + 1,
		"external_id": nil,
	}); !errors.Is(err, ErrTaskStatusConflict) {
		t.Fatalf("stale automatic retry error = %v, want conflict", err)
	}
	got, err := st.GetTask(context.Background(), old.ID)
	if err != nil || got == nil {
		t.Fatalf("get retry intent: %v", err)
	}
	if got.Status != "submitting" || got.StatusSource != "retry" ||
		got.RetryCount != old.RetryCount+1 || got.TargetGeneration != "gen-new" {
		t.Fatalf("stale transition overwrote manual retry: %#v", got)
	}
}
