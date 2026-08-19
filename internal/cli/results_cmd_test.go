package cli

// Tests for the results table's latest-slice index selection (Codex r2):
// x-based slices operate on the group's x-bearing prefix; off-axis
// records (no primary x) never win "latest".

import (
	"testing"

	"github.com/gliese129/runq/internal/backend"
)

func resultsFixture(stepCol []any) *backend.JobResults {
	return &backend.JobResults{
		N: len(stepCol),
		Schema: backend.ResultSchema{
			Groups: []backend.ResultRange{{Key: "a", Offset: 0, Count: len(stepCol)}},
			Axes:   map[string]backend.ResultAxis{"step": {Type: "num", Role: "x"}},
			XAxes:  []string{"step"},
		},
		Cols: backend.ResultCols{Axes: map[string][]any{"step": stepCol}},
	}
}

func TestLatestIdxSkipsOffAxisTail(t *testing.T) {
	// Codex r2 fixture: step=[100, 200, null] → latest is the max-x
	// record (index 1), not the group's last record.
	res := resultsFixture([]any{100.0, 200.0, nil})
	if got := latestIdx(res, res.Schema.Groups[0], "step"); got != 1 {
		t.Errorf("latestIdx = %d, want 1 (max step 200)", got)
	}
}

func TestLatestIdxAllOffAxisDegradesToSequenceOrder(t *testing.T) {
	// No x-bearing records in the group → ts order, i.e. the last record.
	res := resultsFixture([]any{nil, nil, nil})
	if got := latestIdx(res, res.Schema.Groups[0], "step"); got != 2 {
		t.Errorf("latestIdx = %d, want 2 (ts-order fallback)", got)
	}
}

func TestLatestIdxNoXAxis(t *testing.T) {
	// Job has no x axis at all → the group's last record.
	res := resultsFixture([]any{nil, nil})
	res.Schema.XAxes = []string{}
	if got := latestIdx(res, res.Schema.Groups[0], ""); got != 1 {
		t.Errorf("latestIdx = %d, want 1", got)
	}
}

func TestLatestIdxVocabIndexNotMistakenForX(t *testing.T) {
	// Vocab indices travel as int in-process; toFloatCell accepts both
	// int and float64 — latest must still resolve on a fully-numeric
	// prefix regardless of the concrete number type.
	res := resultsFixture([]any{0, 1, nil})
	if got := latestIdx(res, res.Schema.Groups[0], "step"); got != 1 {
		t.Errorf("latestIdx = %d, want 1", got)
	}
}
