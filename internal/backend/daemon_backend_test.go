package backend

import "testing"

func TestDaemonSubmitPathHonorsPreflightOption(t *testing.T) {
	if got := daemonSubmitPath(SubmitOptions{}); got != "/api/jobs" {
		t.Fatalf("default path = %q, want /api/jobs", got)
	}
	if got := daemonSubmitPath(SubmitOptions{SkipPreflight: true}); got != "/api/jobs?no_preflight=1" {
		t.Fatalf("skip path = %q, want /api/jobs?no_preflight=1", got)
	}
}
