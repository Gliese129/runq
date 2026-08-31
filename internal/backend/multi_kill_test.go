package backend

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/gliese129/runq-lab/internal/store"
)

type killRecordingLane struct {
	*UnavailableBackend
	err   error
	calls int
}

func (l *killRecordingLane) KillJob(context.Context, string) error {
	l.calls++
	return l.err
}

func TestKillJobAttemptsEveryGenerationAndJoinsErrors(t *testing.T) {
	st, err := store.Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	ctx := context.Background()
	if err := st.InsertJob(ctx, &store.JobRow{
		ID: "j1", ProjectName: "p", Status: "running", TotalTasks: 3,
		CreatedAt: time.Now(), Target: "hpc",
	}); err != nil {
		t.Fatal(err)
	}

	activeCause := errors.New("active cancel failed")
	retiringCause := errors.New("retiring cancel failed")
	active := &killRecordingLane{UnavailableBackend: NewUnavailableBackend(nil), err: activeCause}
	retiringFailed := &killRecordingLane{UnavailableBackend: NewUnavailableBackend(nil), err: retiringCause}
	retiringOK := &killRecordingLane{UnavailableBackend: NewUnavailableBackend(nil)}
	m, err := NewMultiBackend(map[string]Backend{"hpc": active}, st, "hpc")
	if err != nil {
		t.Fatal(err)
	}
	m.SetRetiringLane("hpc", "old-a", retiringFailed)
	m.SetRetiringLane("hpc", "old-b", retiringOK)

	err = m.KillJob(ctx, "j1")
	if !errors.Is(err, activeCause) || !errors.Is(err, retiringCause) {
		t.Fatalf("KillJob error = %v, want both active and retiring causes", err)
	}
	for name, lane := range map[string]*killRecordingLane{
		"active": active, "retiring failed": retiringFailed, "retiring successful": retiringOK,
	} {
		if lane.calls != 1 {
			t.Errorf("%s calls = %d, want 1", name, lane.calls)
		}
	}
}
