package store

import (
	"context"
	"testing"
	"time"
)

func TestMetricLeaderboard(t *testing.T) {
	st, err := Open(":memory:")
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer st.Close()
	ctx := context.Background()

	if _, err := st.DB().ExecContext(ctx,
		`INSERT INTO projects(name, config_json) VALUES('p','{}')`); err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	job := JobRow{ID: "j", ProjectName: "p", ConfigJSON: "{}", Status: "pending", TotalTasks: 3, CreatedAt: now}
	mk := func(id, params string) TaskRow {
		return TaskRow{ID: id, JobID: "j", ProjectName: "p", Command: "c", ParamsJSON: params, Status: "success", EnqueuedAt: now}
	}
	if err := st.InsertJobWithTasks(ctx, &job, []TaskRow{
		mk("t1", `{"lr":0.1}`), mk("t2", `{"lr":0.2}`), mk("t3", `{"lr":0.3}`),
	}); err != nil {
		t.Fatalf("InsertJobWithTasks: %v", err)
	}
	// t1: loss {0.3, 0.2} → min 0.2, max 0.3.  t2: loss 0.5.  t3: no loss metric.
	// (summary era: the streaming reduction already produced min/max.)
	if err := st.MergeMetricSummaries(ctx, []MetricSummaryRow{
		{TaskID: "t1", JobID: "j", Key: "loss", Min: 0.2, Max: 0.3, Last: 0.2, LastTS: 2, Count: 2},
		{TaskID: "t2", JobID: "j", Key: "loss", Min: 0.5, Max: 0.5, Last: 0.5, LastTS: 1, Count: 1},
	}); err != nil {
		t.Fatal(err)
	}

	// minimize: t1(0.2) < t2(0.5) < t3(none).
	min, err := st.MetricLeaderboard(ctx, "j", "loss", false)
	if err != nil {
		t.Fatal(err)
	}
	if len(min) != 3 {
		t.Fatalf("len = %d, want 3", len(min))
	}
	if min[0].TaskID != "t1" || min[0].Value != 0.2 {
		t.Errorf("min winner = %+v, want t1 0.2", min[0])
	}
	if min[1].TaskID != "t2" {
		t.Errorf("min second = %s, want t2", min[1].TaskID)
	}
	if min[2].TaskID != "t3" || min[2].HasValue {
		t.Errorf("task without metric should sink to bottom: %+v", min[2])
	}
	if min[0].Params["lr"] != 0.1 {
		t.Errorf("params not decoded: %+v", min[0].Params)
	}

	// maximize flips the winner: t2(0.5) > t1(0.3).
	max, err := st.MetricLeaderboard(ctx, "j", "loss", true)
	if err != nil {
		t.Fatal(err)
	}
	if max[0].TaskID != "t2" || max[0].Value != 0.5 {
		t.Errorf("max winner = %+v, want t2 0.5", max[0])
	}
	if max[1].TaskID != "t1" || max[1].Value != 0.3 {
		t.Errorf("max second = %+v, want t1 0.3", max[1])
	}
}
