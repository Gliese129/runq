package backend

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/gliese129/runq-lab/internal/rfs"
	"github.com/gliese129/runq-lab/internal/store"
)

// writeActivityTSV writes n cumulative 3-column rows: ts=i*60,
// bytes=i*100, lines=i*10 (i from 1..n).
func writeActivityTSV(t *testing.T, path string, n int, cols int) {
	t.Helper()
	var b []byte
	for i := 1; i <= n; i++ {
		if cols == 2 {
			b = append(b, []byte(fmt.Sprintf("%d\t%d\n", int64(i)*60, int64(i)*100))...)
		} else {
			b = append(b, []byte(fmt.Sprintf("%d\t%d\t%d\n", int64(i)*60, int64(i)*100, int64(i)*10))...)
		}
	}
	if err := os.WriteFile(path, b, 0o644); err != nil {
		t.Fatal(err)
	}
}

func runActivityBatch(t *testing.T, paths []string) []TaskActivity {
	t.Helper()
	tasks := make([]TaskActivity, len(paths))
	byPath := make(map[string]int, len(paths))
	for i, p := range paths {
		tasks[i] = TaskActivity{TaskID: fmt.Sprintf("t%d", i), BucketMin: 1, Points: []ActivityPoint{}}
		byPath[p] = i
	}
	out, err := execActivityBatch(context.Background(), rfs.NewLocalFS(), paths)
	if err != nil {
		t.Fatalf("execActivityBatch: %v", err)
	}
	parseActivityOutput(out, byPath, tasks)
	return tasks
}

// A long run is decimated to ≤ activityMaxPoints+2 rows, keeping the
// first and last rows, and every kept row carries its EXACT cumulative
// values — stride sampling of cumulative columns is lossless coarsening.
func TestActivityDecimationLossless(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "activity.tsv")
	const n = 10000
	writeActivityTSV(t, p, n, 3)

	ta := runActivityBatch(t, []string{p})[0]
	if len(ta.Points) == 0 || len(ta.Points) > activityMaxPoints+2 {
		t.Fatalf("point count out of bounds: %d", len(ta.Points))
	}
	if ta.BucketMin != 5 { // ceil(10000/2000)
		t.Fatalf("bucket minutes = %d, want 5", ta.BucketMin)
	}
	first, last := ta.Points[0], ta.Points[len(ta.Points)-1]
	if first.TS != 60 || last.TS != n*60 {
		t.Fatalf("first/last row not kept: %d..%d", first.TS, last.TS)
	}
	// Exactness: every kept row's cumulative values match the source
	// formula — nothing was interpolated or re-aggregated.
	prev := int64(0)
	for _, pt := range ta.Points {
		i := pt.TS / 60
		if pt.Bytes != i*100 || pt.Lines == nil || *pt.Lines != i*10 {
			t.Fatalf("row at ts=%d not exact: bytes=%d lines=%v", pt.TS, pt.Bytes, pt.Lines)
		}
		if pt.TS <= prev {
			t.Fatalf("non-monotonic ts at %d", pt.TS)
		}
		prev = pt.TS
	}
}

// Short files come back raw: every row, stride 1.
func TestActivityShortFileRaw(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "activity.tsv")
	writeActivityTSV(t, p, 10, 3)

	ta := runActivityBatch(t, []string{p})[0]
	if len(ta.Points) != 10 || ta.BucketMin != 1 {
		t.Fatalf("want 10 raw points stride 1, got %d stride %d", len(ta.Points), ta.BucketMin)
	}
}

// Legacy 2-column files (bytes only) report lines as null — bytes must
// not impersonate line counts.
func TestActivityLegacyTwoColumns(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "activity.tsv")
	writeActivityTSV(t, p, 5, 2)

	ta := runActivityBatch(t, []string{p})[0]
	if len(ta.Points) != 5 {
		t.Fatalf("want 5 points, got %d", len(ta.Points))
	}
	for _, pt := range ta.Points {
		if pt.Lines != nil {
			t.Fatalf("legacy row must have nil lines, got %d", *pt.Lines)
		}
	}
}

// A missing file is an answer (empty points), never an error — pending
// tasks and pre-sidecar runs are legal states.
func TestActivityMissingFile(t *testing.T) {
	dir := t.TempDir()
	present := filepath.Join(dir, "activity.tsv")
	writeActivityTSV(t, present, 3, 3)
	absent := filepath.Join(dir, "nope", "activity.tsv")

	tasks := runActivityBatch(t, []string{absent, present})
	if len(tasks[0].Points) != 0 {
		t.Fatalf("missing file must yield empty points, got %d", len(tasks[0].Points))
	}
	if len(tasks[1].Points) != 3 {
		t.Fatalf("present file lost rows: %d", len(tasks[1].Points))
	}
}

// activityWindow: start falls back to job creation, end is nil while
// live and the latest task finish once terminal.
func TestActivityWindow(t *testing.T) {
	created := time.Unix(1000, 0)
	s1 := time.Unix(1100, 0)
	f1 := time.Unix(1500, 0)
	f2 := time.Unix(1700, 0)

	live := &store.JobRow{Status: "running", CreatedAt: created}
	start, end := activityWindow(live, []store.TaskRow{{StartedAt: &s1, FinishedAt: &f1}})
	if start != 1100 || end != nil {
		t.Fatalf("live job: start=%d end=%v", start, end)
	}

	done := &store.JobRow{Status: "done", CreatedAt: created}
	start, end = activityWindow(done, []store.TaskRow{
		{StartedAt: &s1, FinishedAt: &f1},
		{StartedAt: &s1, FinishedAt: &f2},
	})
	if start != 1100 || end == nil || *end != 1700 {
		t.Fatalf("terminal job: start=%d end=%v", start, end)
	}

	start, end = activityWindow(live, nil)
	if start != 1000 || end != nil {
		t.Fatalf("no tasks: start=%d end=%v", start, end)
	}
}
