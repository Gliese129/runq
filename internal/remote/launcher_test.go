package remote

import "testing"

func TestAttemptHandleIncludesDurableAttemptEpoch(t *testing.T) {
	first := attemptHandle("task-1", 0)
	retry := attemptHandle("task-1", 1)

	if first != "task-1-a0" {
		t.Fatalf("first attempt handle = %q, want task-1-a0", first)
	}
	if retry != "task-1-a1" {
		t.Fatalf("retry attempt handle = %q, want task-1-a1", retry)
	}
	if first == retry {
		t.Fatalf("distinct attempt epochs produced the same handle %q", first)
	}
	if got := attemptHandle("task-1", 1); got != retry {
		t.Fatalf("same attempt epoch is not idempotent: got %q, want %q", got, retry)
	}
}
