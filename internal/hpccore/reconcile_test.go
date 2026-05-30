package hpccore

import "testing"

func TestReconcile(t *testing.T) {
	cases := []struct {
		name    string
		current string
		obs     Observed
		want    string
	}{
		{"user kill wins", "running", Observed{WrapperStatus: "running", KillRequested: true}, "killed"},
		{"wrapper success", "running", Observed{WrapperStatus: "success"}, "success"},
		{"wrapper failed", "running", Observed{WrapperStatus: "failed"}, "failed"},
		{"wrapper success beats scheduler failed", "running", Observed{WrapperStatus: "success", Scheduler: SchedFailed}, "success"},

		{"scheduler terminal success", "running", Observed{WrapperStatus: "", Scheduler: SchedSuccess}, "success"},
		{"scheduler terminal failed", "running", Observed{WrapperStatus: "", Scheduler: SchedFailed}, "failed"},
		{"scheduler terminal killed", "running", Observed{WrapperStatus: "", Scheduler: SchedKilled}, "killed"},

		{"zombie: running but gone", "running", Observed{WrapperStatus: "running", Scheduler: SchedGone}, "failed"},
		{"running + active", "pending", Observed{WrapperStatus: "running", Scheduler: SchedActive}, "running"},
		{"running + scheduler running", "pending", Observed{WrapperStatus: "started", Scheduler: SchedRunning}, "running"},
		{"running + scheduler unknown", "running", Observed{WrapperStatus: "running", Scheduler: SchedUnknown}, "running"},

		{"no wrapper + scheduler running", "pending", Observed{WrapperStatus: "", Scheduler: SchedRunning}, "running"},
		{"no wrapper + active", "pending", Observed{WrapperStatus: "", Scheduler: SchedActive}, "pending"},
		{"no wrapper + pending", "pending", Observed{WrapperStatus: "", Scheduler: SchedPending}, "pending"},
		{"no wrapper + gone", "pending", Observed{WrapperStatus: "", Scheduler: SchedGone}, "failed"},

		// No new fact → keep current (no spurious downgrade).
		{"keep current running", "running", Observed{WrapperStatus: "", Scheduler: SchedUnknown}, "running"},
		{"keep current pending", "pending", Observed{WrapperStatus: "", Scheduler: SchedUnknown}, "pending"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := Reconcile(c.current, c.obs); got != c.want {
				t.Errorf("Reconcile(%q, %+v) = %q, want %q", c.current, c.obs, got, c.want)
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
