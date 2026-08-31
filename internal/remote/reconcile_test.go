package remote

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
		{"inferred failed corrected by scheduler running", "failed", SourceInferred, Observed{Scheduler: SchedRunning}, "running", SourceScheduler},

		// Scheduler terminals are soft (a hard terminal may correct them), but
		// same/lower-strength live evidence must not reopen them.
		{"scheduler success not reopened by running", "success", SourceScheduler, Observed{Scheduler: SchedRunning}, "success", SourceScheduler},
		{"scheduler failed not reopened by pending", "failed", SourceScheduler, Observed{Scheduler: SchedPending}, "failed", SourceScheduler},
		{"scheduler killed not reopened by wrapper live", "killed", SourceScheduler, Observed{WrapperStatus: "running"}, "killed", SourceScheduler},
		{"scheduler terminal corrected by wrapper terminal", "failed", SourceScheduler, Observed{WrapperStatus: "success"}, "success", SourceWrapper},

		// Same-strength scheduler live evidence can advance, not regress.
		{"scheduler pending advances to running", "pending", SourceScheduler, Observed{Scheduler: SchedRunning}, "running", SourceScheduler},
		{"scheduler running does not regress to pending", "running", SourceScheduler, Observed{Scheduler: SchedPending}, "running", SourceScheduler},

		// Pure Reconcile also enforces hard-terminal finality even though refresh
		// normally skips these rows before calling it.
		{"wrapper terminal is hard", "success", SourceWrapper, Observed{KillRequested: true}, "success", SourceWrapper},
		{"runq terminal is hard", "killed", SourceRunq, Observed{WrapperStatus: "success"}, "killed", SourceRunq},
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

func TestAcceptCandidateEvidenceMatrix(t *testing.T) {
	fixtures := []struct {
		name     string
		decision Decision
		class    evidenceClass
	}{
		{"none", Decision{"pending", ""}, evidenceNone},
		{"submit", Decision{"failed", SourceSubmit}, evidenceSubmit},
		{"inferred terminal", Decision{"failed", SourceInferred}, evidenceInferredTerminal},
		{"scheduler live", Decision{"pending", SourceScheduler}, evidenceSchedulerLive},
		{"wrapper live", Decision{"running", SourceWrapper}, evidenceWrapperLive},
		{"scheduler terminal", Decision{"success", SourceScheduler}, evidenceSchedulerTerminal},
		{"wrapper terminal", Decision{"success", SourceWrapper}, evidenceWrapperTerminal},
		{"runq terminal", Decision{"killed", SourceRunq}, evidenceRunqTerminal},
	}

	// Rows are current evidence; columns are candidate evidence in fixture
	// order. This is intentionally exhaustive so adding an evidence class
	// requires an explicit acceptance decision for every current class.
	want := [][]bool{
		// none, submit, inferred, sched-live, wrapper-live, sched-term, wrapper-term, runq-term
		{false, false, true, true, true, true, true, true},       // none
		{false, false, true, true, true, true, true, true},       // submit
		{false, false, true, true, true, true, true, true},       // inferred terminal
		{false, false, true, true, true, true, true, true},       // scheduler live (pending)
		{false, false, true, false, true, true, true, true},      // wrapper live
		{false, false, false, false, false, false, true, true},   // scheduler terminal
		{false, false, false, false, false, false, false, false}, // wrapper terminal
		{false, false, false, false, false, false, false, false}, // runq terminal
	}

	if len(want) != len(fixtures) {
		t.Fatalf("acceptance matrix has %d rows, want %d", len(want), len(fixtures))
	}
	for i, current := range fixtures {
		if got := classifyEvidence(current.decision); got != current.class {
			t.Fatalf("classifyEvidence(%s) = %d, want %d", current.name, got, current.class)
		}
		if len(want[i]) != len(fixtures) {
			t.Fatalf("acceptance matrix row %s has %d columns, want %d", current.name, len(want[i]), len(fixtures))
		}
		for j, candidate := range fixtures {
			t.Run(current.name+"/"+candidate.name, func(t *testing.T) {
				if got := acceptCandidate(current.decision, candidate.decision); got != want[i][j] {
					t.Errorf("acceptCandidate(%+v, %+v) = %t, want %t", current.decision, candidate.decision, got, want[i][j])
				}
			})
		}
	}

	// The scheduler-live matrix fixture is pending. Cover its only
	// status-sensitive edges explicitly.
	for _, tc := range []struct {
		name      string
		current   Decision
		candidate Decision
		want      bool
	}{
		{"pending to running", Decision{"pending", SourceScheduler}, Decision{"running", SourceScheduler}, true},
		{"running stays running", Decision{"running", SourceScheduler}, Decision{"running", SourceScheduler}, true},
		{"running to pending rejected", Decision{"running", SourceScheduler}, Decision{"pending", SourceScheduler}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := acceptCandidate(tc.current, tc.candidate); got != tc.want {
				t.Errorf("acceptCandidate(%+v, %+v) = %t, want %t", tc.current, tc.candidate, got, tc.want)
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
