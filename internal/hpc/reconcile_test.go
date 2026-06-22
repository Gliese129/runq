package hpc

import "testing"

func TestReconcile(t *testing.T) {
	cases := []struct {
		name       string
		curStatus  string
		curSource  string
		obs        Observed
		wantStatus string
		wantSource string
	}{
		{"user kill", "running", SourceWrapper, Observed{WrapperStatus: "running", KillRequested: true}, "killed", SourceRunq},
		{"wrapper success", "running", SourceWrapper, Observed{WrapperStatus: "success"}, "success", SourceWrapper},
		{"wrapper failed", "running", SourceWrapper, Observed{WrapperStatus: "failed"}, "failed", SourceWrapper},
		{"wrapper success beats scheduler failed", "running", SourceWrapper, Observed{WrapperStatus: "success", Scheduler: SchedFailed}, "success", SourceWrapper},

		{"scheduler terminal success", "running", "", Observed{Scheduler: SchedSuccess}, "success", SourceScheduler},
		{"scheduler terminal failed", "running", "", Observed{Scheduler: SchedFailed}, "failed", SourceScheduler},
		{"scheduler terminal killed", "running", "", Observed{Scheduler: SchedKilled}, "killed", SourceScheduler},

		{"zombie: running but gone -> inferred", "running", SourceWrapper, Observed{WrapperStatus: "running", Scheduler: SchedGone}, "failed", SourceInferred},
		{"running + active", "pending", SourceScheduler, Observed{WrapperStatus: "running", Scheduler: SchedActive}, "running", SourceWrapper},
		{"running + sched running", "pending", "", Observed{WrapperStatus: "started", Scheduler: SchedRunning}, "running", SourceWrapper},
		{"running + unknown", "running", SourceWrapper, Observed{WrapperStatus: "running", Scheduler: SchedUnknown}, "running", SourceWrapper},

		{"no wrapper + sched running", "pending", "", Observed{Scheduler: SchedRunning}, "running", SourceScheduler},
		{"no wrapper + active", "pending", "", Observed{Scheduler: SchedActive}, "pending", SourceScheduler},
		{"no wrapper + pending", "pending", "", Observed{Scheduler: SchedPending}, "pending", SourceScheduler},

		// The important change: "no wrapper + gone" must NOT infer failure — keep
		// current (a never-started task isn't killed just because it's not listed).
		{"no wrapper + gone -> keep", "pending", "", Observed{Scheduler: SchedGone}, "pending", ""},

		// No new fact → keep current status AND source (no spurious downgrade).
		{"keep running", "running", SourceWrapper, Observed{Scheduler: SchedUnknown}, "running", SourceWrapper},
		{"keep pending", "pending", "", Observed{Scheduler: SchedUnknown}, "pending", ""},

		// An inferred terminal is correctable by a later wrapper terminal.
		{"inferred failed corrected by wrapper success", "failed", SourceInferred, Observed{WrapperStatus: "success"}, "success", SourceWrapper},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := Reconcile(c.curStatus, c.curSource, c.obs)
			if got.Status != c.wantStatus || got.Source != c.wantSource {
				t.Errorf("Reconcile(%q,%q,%+v) = {%q,%q}, want {%q,%q}",
					c.curStatus, c.curSource, c.obs, got.Status, got.Source, c.wantStatus, c.wantSource)
			}
		})
	}
}

func TestParseSignal(t *testing.T) {
	cases := map[string]SchedulerSignal{
		"PENDING":       SchedPending,
		"running":       SchedRunning,
		"  RUNNING  ":   SchedRunning,
		"COMPLETED":     SchedSuccess,
		"success":       SchedSuccess,
		"FAILED":        SchedFailed,
		"TIMEOUT":       SchedFailed,
		"OUT_OF_MEMORY": SchedFailed,
		"NODE_FAIL":     SchedFailed,
		"CANCELLED":     SchedKilled,
		"gone":          SchedGone,
		"R":             SchedUnknown, // PBS code — needs a status_parser hook
		"":              SchedUnknown,
		"whatever":      SchedUnknown,
	}
	for in, want := range cases {
		if got := ParseSignal(in); got != want {
			t.Errorf("ParseSignal(%q) = %q, want %q", in, got, want)
		}
	}
}
