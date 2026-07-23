package backend

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/gliese129/runq/internal/store"
)

// genLane is an active lane with a generation stamp; records retries.
type genLane struct {
	*UnavailableBackend
	gen     string
	retried []string
}

func (g *genLane) Generation() string { return g.gen }
func (g *genLane) RetryTask(_ context.Context, id string) error {
	g.retried = append(g.retried, id)
	return nil
}

// RQ-75: rerunning a task whose target config changed needs confirmation;
// confirmed reruns are restamped to the active generation and run on the
// ACTIVE lane — never on the quiesced retiring one.
func TestRetryTaskCrossGeneration(t *testing.T) {
	st, err := store.Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	ctx := context.Background()
	_ = st.InsertJob(ctx, &store.JobRow{ID: "j1", ProjectName: "p", Status: "failed", TotalTasks: 1, CreatedAt: time.Now(), Target: "hpc"})
	if err := st.InsertTask(ctx, &store.TaskRow{
		ID: "t1", JobID: "j1", ProjectName: "p", Command: "true", ParamsJSON: "{}",
		GPUsNeeded: 1, Status: "failed", EnqueuedAt: time.Now(),
		Target: "hpc", TargetGeneration: "gen-OLD",
	}); err != nil {
		t.Fatal(err)
	}

	active := &genLane{UnavailableBackend: NewUnavailableBackend(errors.New("unused")), gen: "gen-NEW"}
	retiring := &genLane{UnavailableBackend: NewUnavailableBackend(errors.New("unused")), gen: "gen-OLD"}
	m, err := NewMultiBackend(map[string]Backend{"hpc": active}, st, "hpc")
	if err != nil {
		t.Fatal(err)
	}
	m.SetRetiringLane("hpc", "gen-OLD", retiring)

	// Unconfirmed: refused with the typed error; nothing ran anywhere.
	err = m.RetryTask(ctx, "t1")
	var gc *GenerationChangedError
	if !errors.As(err, &gc) {
		t.Fatalf("unconfirmed cross-gen retry: got %v, want GenerationChangedError", err)
	}
	if gc.TaskGeneration != "gen-OLD" || gc.ActiveGeneration != "gen-NEW" {
		t.Fatalf("error payload: %+v", gc)
	}
	if len(active.retried)+len(retiring.retried) != 0 {
		t.Fatal("refused retry still reached a lane")
	}

	// Confirmed: restamped to the active generation, run on the ACTIVE lane.
	if err := m.RetryTaskGen(ctx, "t1", true); err != nil {
		t.Fatal(err)
	}
	if len(active.retried) != 1 || active.retried[0] != "t1" {
		t.Fatalf("active lane retried = %v", active.retried)
	}
	if len(retiring.retried) != 0 {
		t.Fatal("retry reached the retiring lane")
	}
	row, _ := st.GetTask(ctx, "t1")
	if row.TargetGeneration != "gen-NEW" {
		t.Fatalf("row not restamped: %q", row.TargetGeneration)
	}

	// Same-generation retry needs no confirmation.
	if err := m.RetryTask(ctx, "t1"); err != nil {
		t.Fatalf("same-gen retry: %v", err)
	}
}
