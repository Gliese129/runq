package backend

// Assembly tests for jobResultsFromDB (RQ2-1 §A): key classification,
// sorting/ranges, vocab encoding, mixed-type nulling, identity fallback,
// and the skipped/truncated plumbing.

import (
	"context"
	"testing"
	"time"

	"github.com/gliese129/runq/internal/store"
)

func seedResultsStore(t *testing.T, taskIDs ...string) *store.Store {
	t.Helper()
	st, err := store.Open(":memory:")
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { st.Close() })
	ctx := context.Background()
	if _, err := st.DB().Exec(`INSERT INTO projects (name, config_json) VALUES ('p', '{}')`); err != nil {
		t.Fatalf("insert project: %v", err)
	}
	if err := st.InsertJob(ctx, &store.JobRow{
		ID: "j1", ProjectName: "p", ConfigJSON: "{}",
		Status: "running", TotalTasks: len(taskIDs), CreatedAt: time.Now(),
	}); err != nil {
		t.Fatalf("insert job: %v", err)
	}
	for _, id := range taskIDs {
		if err := st.InsertTask(ctx, &store.TaskRow{
			ID: id, JobID: "j1", ProjectName: "p",
			Command: "echo", ParamsJSON: "{}", GPUsNeeded: 1,
			Status: "running", EnqueuedAt: time.Now(),
		}); err != nil {
			t.Fatalf("insert task %s: %v", id, err)
		}
	}
	return st
}

func insertRecords(t *testing.T, st *store.Store, taskID string, rows [][2]string, tsBase int64) {
	t.Helper()
	recs := make([]store.ResultRecordRow, len(rows))
	for i, r := range rows {
		recs[i] = store.ResultRecordRow{
			TaskID: taskID, JobID: "j1", TS: tsBase + int64(i),
			AxesJSON: r[0], MetricsJSON: r[1],
		}
	}
	if err := st.ApplyResultsIngestDelta(context.Background(), taskID, false, recs, len(recs), store.FileIngestMark{}); err != nil {
		t.Fatalf("insert records: %v", err)
	}
}

func TestResultsAssemblyBasic(t *testing.T) {
	st := seedResultsStore(t, "t1", "t2")
	insertRecords(t, st, "t1", [][2]string{
		{`{"model":"baseline","step":2000,"data":"dclm"}`, `{"math":24.8}`},
		{`{"model":"baseline","step":4000,"data":"dclm"}`, `{"math":26.1,"val_loss":1.9}`},
	}, 100)
	insertRecords(t, st, "t2", [][2]string{
		{`{"model":"math-heavy","step":2000,"data":"owm"}`, `{"math":31.2}`},
	}, 200)

	res, err := jobResultsFromDB(context.Background(), st, "j1")
	if err != nil {
		t.Fatalf("jobResultsFromDB: %v", err)
	}
	if res.Source != ResultSource || res.N != 3 || res.Parsed != 3 {
		t.Errorf("header fields: %+v", res)
	}
	if res.UpdatedAt != 200 {
		t.Errorf("updated_at = %d, want 200", res.UpdatedAt)
	}
	if res.Skipped != 0 || res.Truncated {
		t.Errorf("skipped/truncated should be zero: %+v", res)
	}

	// model → identity; step numeric+varying → x; data str → label.
	if ax := res.Schema.Axes["model"]; ax.Role != "identity" || ax.Type != "str" {
		t.Errorf("model axis = %+v", ax)
	}
	if ax := res.Schema.Axes["step"]; ax.Role != "x" || ax.Type != "num" {
		t.Errorf("step axis = %+v", ax)
	}
	if ax := res.Schema.Axes["data"]; ax.Role != "label" || ax.Type != "str" {
		t.Errorf("data axis = %+v", ax)
	}
	if len(res.Schema.XAxes) != 1 || res.Schema.XAxes[0] != "step" {
		t.Errorf("x_axes = %v", res.Schema.XAxes)
	}

	// Sorted by identity: baseline(2) then math-heavy(1); ranges direct.
	wantGroups := []ResultRange{{Key: "baseline", Offset: 0, Count: 2}, {Key: "math-heavy", Offset: 2, Count: 1}}
	if len(res.Schema.Groups) != 2 || res.Schema.Groups[0] != wantGroups[0] || res.Schema.Groups[1] != wantGroups[1] {
		t.Errorf("groups = %+v", res.Schema.Groups)
	}
	wantTasks := []ResultRange{{ID: "t1", Offset: 0, Count: 2}, {ID: "t2", Offset: 2, Count: 1}}
	if len(res.Schema.Tasks) != 2 || res.Schema.Tasks[0] != wantTasks[0] || res.Schema.Tasks[1] != wantTasks[1] {
		t.Errorf("tasks = %+v", res.Schema.Tasks)
	}

	// Columns equally long, metric holes are nil.
	if got := res.Cols.Metrics["val_loss"]; len(got) != 3 || got[0] != nil || *got[1] != 1.9 || got[2] != nil {
		t.Errorf("val_loss column = %v", got)
	}
	// Vocab encoding: dclm=0, owm=1 (first appearance in sorted order).
	data := res.Schema.Axes["data"]
	if len(data.Vocab) != 2 || data.Vocab[0] != "dclm" || data.Vocab[1] != "owm" {
		t.Errorf("data vocab = %v", data.Vocab)
	}
	if col := res.Cols.Axes["data"]; col[0] != 0 || col[2] != 1 {
		t.Errorf("data column = %v", col)
	}
	if res.Schema.Metrics[0] != "math" || res.Schema.Metrics[1] != "val_loss" {
		t.Errorf("metrics = %v", res.Schema.Metrics)
	}
}

func TestResultsSortByPrimaryXWithinGroup(t *testing.T) {
	st := seedResultsStore(t, "t1")
	// Inserted out of x order; wire must sort by step within the group.
	insertRecords(t, st, "t1", [][2]string{
		{`{"model":"a","step":4000}`, `{"m":2}`},
		{`{"model":"a","step":2000}`, `{"m":1}`},
	}, 100)

	res, err := jobResultsFromDB(context.Background(), st, "j1")
	if err != nil {
		t.Fatalf("jobResultsFromDB: %v", err)
	}
	if col := res.Cols.Axes["step"]; col[0] != 2000.0 || col[1] != 4000.0 {
		t.Errorf("step column not sorted: %v", col)
	}
}

func TestResultsIdentityFallbackTaskID(t *testing.T) {
	st := seedResultsStore(t, "t1", "t2")
	insertRecords(t, st, "t1", [][2]string{{`{"lr":0.1}`, `{"acc":0.9}`}}, 100)
	insertRecords(t, st, "t2", [][2]string{{`{"lr":0.2}`, `{"acc":0.8}`}}, 200)

	res, err := jobResultsFromDB(context.Background(), st, "j1")
	if err != nil {
		t.Fatalf("jobResultsFromDB: %v", err)
	}
	// No model axis → each task is its own series, keyed by task_id.
	if len(res.Schema.Groups) != 2 || res.Schema.Groups[0].Key != "t1" || res.Schema.Groups[1].Key != "t2" {
		t.Errorf("groups = %+v", res.Schema.Groups)
	}
	if _, ok := res.Schema.Axes["model"]; ok {
		t.Errorf("phantom model axis in schema")
	}
	// lr is numeric but does not VARY within any series → label, no x.
	if ax := res.Schema.Axes["lr"]; ax.Role != "label" {
		t.Errorf("lr axis = %+v", ax)
	}
	if len(res.Schema.XAxes) != 0 {
		t.Errorf("x_axes = %v, want empty", res.Schema.XAxes)
	}
}

func TestResultsMultiModelTaskSplitsRuns(t *testing.T) {
	// One eval task recording two models: identity clustering wins, and
	// the task contributes ONE RUN PER GROUP — a run never spans a group
	// boundary, or a per-(group, task) slice would leak the other model's
	// rows (Codex r1 finding 2).
	st := seedResultsStore(t, "t1")
	insertRecords(t, st, "t1", [][2]string{
		{`{"model":"a","step":1}`, `{"m":1}`},
		{`{"model":"b","step":1}`, `{"m":2}`},
		{`{"model":"a","step":2}`, `{"m":3}`},
	}, 100)

	res, err := jobResultsFromDB(context.Background(), st, "j1")
	if err != nil {
		t.Fatalf("jobResultsFromDB: %v", err)
	}
	if len(res.Schema.Groups) != 2 || res.Schema.Groups[0].Key != "a" || res.Schema.Groups[0].Count != 2 {
		t.Errorf("groups = %+v", res.Schema.Groups)
	}
	wantTasks := []ResultRange{{ID: "t1", Offset: 0, Count: 2}, {ID: "t1", Offset: 2, Count: 1}}
	if len(res.Schema.Tasks) != 2 || res.Schema.Tasks[0] != wantTasks[0] || res.Schema.Tasks[1] != wantTasks[1] {
		t.Errorf("tasks = %+v, want %+v", res.Schema.Tasks, wantTasks)
	}
	assertTaskRunsNestInGroups(t, res)
}

// assertTaskRunsNestInGroups checks the range invariant: every task run
// lies entirely inside one identity group.
func assertTaskRunsNestInGroups(t *testing.T, res *JobResults) {
	t.Helper()
	for _, tr := range res.Schema.Tasks {
		contained := false
		for _, g := range res.Schema.Groups {
			if tr.Offset >= g.Offset && tr.Offset+tr.Count <= g.Offset+g.Count {
				contained = true
				break
			}
		}
		if !contained {
			t.Errorf("task run %+v spans a group boundary (groups %+v)", tr, res.Schema.Groups)
		}
	}
}

func TestResultsCrossTaskSeriesMonotonicX(t *testing.T) {
	// Codex r1 finding 1 fixture: one model spans two tasks and the
	// LEXICALLY LAST task holds the SMALLER steps. The group must order
	// by x (the series is the curve; last = latest), not cluster by task.
	st := seedResultsStore(t, "t1", "t2")
	insertRecords(t, st, "t1", [][2]string{
		{`{"model":"shared","step":100}`, `{"m":3}`},
		{`{"model":"shared","step":200}`, `{"m":4}`},
	}, 100)
	insertRecords(t, st, "t2", [][2]string{
		{`{"model":"shared","step":10}`, `{"m":1}`},
		{`{"model":"shared","step":20}`, `{"m":2}`},
	}, 200)

	res, err := jobResultsFromDB(context.Background(), st, "j1")
	if err != nil {
		t.Fatalf("jobResultsFromDB: %v", err)
	}
	col := res.Cols.Axes["step"]
	want := []any{10.0, 20.0, 100.0, 200.0}
	for i := range want {
		if col[i] != want[i] {
			t.Fatalf("step column = %v, want %v (monotonic within the group)", col, want)
		}
	}
	// Latest slice (group's last record) must be the true max step.
	g := res.Schema.Groups[0]
	if last := col[g.Offset+g.Count-1]; last != 200.0 {
		t.Errorf("group last x = %v, want 200", last)
	}
	// The task runs split by x interleave: t2 then t1, nested in the group.
	wantTasks := []ResultRange{{ID: "t2", Offset: 0, Count: 2}, {ID: "t1", Offset: 2, Count: 2}}
	if len(res.Schema.Tasks) != 2 || res.Schema.Tasks[0] != wantTasks[0] || res.Schema.Tasks[1] != wantTasks[1] {
		t.Errorf("tasks = %+v, want %+v", res.Schema.Tasks, wantTasks)
	}
	assertTaskRunsNestInGroups(t, res)
}

func TestResultsOffAxisTailInvariant(t *testing.T) {
	// Codex r2 fixture: legal records without the primary x. Within a
	// group they must form the TAIL (ordered by ts), leaving the
	// x-bearing records as a monotonic prefix — the shape every x-based
	// slice (latest/first/aligned) relies on.
	st := seedResultsStore(t, "t1")
	insertRecords(t, st, "t1", [][2]string{
		{`{"model":"a"}`, `{"m":9}`},            // off-axis, ts=100
		{`{"model":"a","step":200}`, `{"m":2}`}, // ts=101
		{`{"model":"a","step":100}`, `{"m":1}`}, // ts=102
	}, 100)
	// Second off-axis record with an EARLIER ts than the first one,
	// inserted later — the tail must come out ts-sorted regardless.
	if err := st.ApplyResultsIngestDelta(context.Background(), "t1", false,
		[]store.ResultRecordRow{{TaskID: "t1", JobID: "j1", TS: 50,
			AxesJSON: `{"model":"a"}`, MetricsJSON: `{"m":8}`}},
		1, store.FileIngestMark{}); err != nil {
		t.Fatalf("insert off-axis record: %v", err)
	}

	res, err := jobResultsFromDB(context.Background(), st, "j1")
	if err != nil {
		t.Fatalf("jobResultsFromDB: %v", err)
	}
	col := res.Cols.Axes["step"]
	if col[0] != 100.0 || col[1] != 200.0 || col[2] != nil || col[3] != nil {
		t.Fatalf("step column = %v, want monotonic prefix + null tail", col)
	}
	if res.Cols.TS[2] != 50 || res.Cols.TS[3] != 100 {
		t.Errorf("off-axis tail not ts-ordered: ts = %v", res.Cols.TS)
	}
}

func TestResultsTypedIdentityNeverMerges(t *testing.T) {
	// Codex r1 finding 3: model=1 vs model="1" (and true vs "true") are
	// distinct series; the display label may repeat but groups must not
	// merge.
	st := seedResultsStore(t, "t1")
	insertRecords(t, st, "t1", [][2]string{
		{`{"model":1}`, `{"m":1}`},
		{`{"model":"1"}`, `{"m":2}`},
		{`{"model":true}`, `{"m":3}`},
		{`{"model":"true"}`, `{"m":4}`},
	}, 100)

	res, err := jobResultsFromDB(context.Background(), st, "j1")
	if err != nil {
		t.Fatalf("jobResultsFromDB: %v", err)
	}
	if len(res.Schema.Groups) != 4 {
		t.Fatalf("groups = %+v, want 4 distinct series", res.Schema.Groups)
	}
	labels := map[string]int{}
	for _, g := range res.Schema.Groups {
		if g.Count != 1 {
			t.Errorf("merged group: %+v", g)
		}
		labels[g.Key]++
	}
	// Display labels legitimately repeat ("1" twice, "true" twice).
	if labels["1"] != 2 || labels["true"] != 2 {
		t.Errorf("labels = %v", labels)
	}
}

func TestResultsFallbackNeverCollidesWithModelValue(t *testing.T) {
	// t2 has no model (falls back to its task id); t1 explicitly records
	// model="t2". Same display label, but they must stay separate series.
	st := seedResultsStore(t, "t1", "t2")
	insertRecords(t, st, "t1", [][2]string{{`{"model":"t2"}`, `{"m":1}`}}, 100)
	insertRecords(t, st, "t2", [][2]string{{`{"lr":0.1}`, `{"m":2}`}}, 200)

	res, err := jobResultsFromDB(context.Background(), st, "j1")
	if err != nil {
		t.Fatalf("jobResultsFromDB: %v", err)
	}
	if len(res.Schema.Groups) != 2 {
		t.Fatalf("groups = %+v, want 2 (fallback must not merge with explicit model)", res.Schema.Groups)
	}
	if res.Schema.Groups[0].Key != "t2" || res.Schema.Groups[1].Key != "t2" {
		t.Errorf("labels = %+v", res.Schema.Groups)
	}
}

func TestResultsMixedTypeAxisMajorityWins(t *testing.T) {
	st := seedResultsStore(t, "t1")
	insertRecords(t, st, "t1", [][2]string{
		{`{"model":"a","step":100}`, `{"m":1}`},
		{`{"model":"a","step":200}`, `{"m":2}`},
		{`{"model":"a","step":"final"}`, `{"m":3}`},
	}, 100)

	res, err := jobResultsFromDB(context.Background(), st, "j1")
	if err != nil {
		t.Fatalf("jobResultsFromDB: %v", err)
	}
	ax := res.Schema.Axes["step"]
	if ax.Type != "num" {
		t.Errorf("majority type = %q, want num", ax.Type)
	}
	if ax.Nulled != 1 {
		t.Errorf("nulled = %d, want 1", ax.Nulled)
	}
	col := res.Cols.Axes["step"]
	// The "final" record sorts after numeric x (nulls last).
	if col[0] != 100.0 || col[1] != 200.0 || col[2] != nil {
		t.Errorf("step column = %v", col)
	}
}

func TestResultsSkippedFromDroppedCount(t *testing.T) {
	st := seedResultsStore(t, "t1")
	insertRecords(t, st, "t1", [][2]string{{`{"model":"a"}`, `{"m":1}`}}, 100)
	// Simulate a cap overflow recorded by ingest.
	if err := st.SetFileIngestMark(context.Background(), "t1", store.IngestFileResults,
		store.FileIngestMark{Size: 10, Offset: 10, Dropped: 7}); err != nil {
		t.Fatalf("SetFileIngestMark: %v", err)
	}

	res, err := jobResultsFromDB(context.Background(), st, "j1")
	if err != nil {
		t.Fatalf("jobResultsFromDB: %v", err)
	}
	if res.Skipped != 7 || !res.Truncated {
		t.Errorf("skipped=%d truncated=%v, want 7/true", res.Skipped, res.Truncated)
	}
}

func TestResultsEmptyJobShape(t *testing.T) {
	st := seedResultsStore(t, "t1")
	res, err := jobResultsFromDB(context.Background(), st, "j1")
	if err != nil {
		t.Fatalf("jobResultsFromDB: %v", err)
	}
	if res.N != 0 || res.Parsed != 0 || res.UpdatedAt != 0 {
		t.Errorf("empty job header: %+v", res)
	}
	// Arrays/maps must be present-and-empty, not null, for a stable wire.
	if res.Schema.Groups == nil || res.Schema.Tasks == nil || res.Schema.Axes == nil ||
		res.Schema.XAxes == nil || res.Schema.Metrics == nil ||
		res.Cols.TS == nil || res.Cols.Axes == nil || res.Cols.Metrics == nil {
		t.Errorf("empty job must serialize empty containers: %+v", res)
	}
}
