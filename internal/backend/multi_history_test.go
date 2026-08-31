package backend

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/gliese129/runq-lab/internal/rfs"
	"github.com/gliese129/runq-lab/internal/store"
)

type artifactGenerationLane struct {
	*UnavailableBackend
	generation string
	logCalls   int
	fs         rfs.FS
}

func (l *artifactGenerationLane) Generation() string { return l.generation }
func (l *artifactGenerationLane) FS() rfs.FS         { return l.fs }

func (l *artifactGenerationLane) TaskLogTail(context.Context, string, int) (*LogPage, error) {
	l.logCalls++
	return &LogPage{Lines: []string{"historical"}}, nil
}

type recordingArtifactFS struct {
	rfs.FS
	execCalls int
	commands  []string
}

func (f *recordingArtifactFS) Exec(_ context.Context, cmd string, args ...string) ([]byte, []byte, int, error) {
	f.execCalls++
	f.commands = append(f.commands, strings.Join(append([]string{cmd}, args...), " "))
	return nil, nil, 0, nil
}

func TestSettledTaskArtifactsStayOnOwningGeneration(t *testing.T) {
	st, err := store.Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	ctx := context.Background()
	if err := st.InsertJob(ctx, &store.JobRow{
		ID: "job-history", ProjectName: "p", Status: "done", TotalTasks: 1,
		Target: "hpc", CreatedAt: time.Now(),
	}); err != nil {
		t.Fatal(err)
	}
	if err := st.InsertTask(ctx, &store.TaskRow{
		ID: "task-history", JobID: "job-history", ProjectName: "p", Command: "true",
		ParamsJSON: "{}", Status: "success", Target: "hpc", TargetGeneration: "old",
		EnqueuedAt: time.Now(),
	}); err != nil {
		t.Fatal(err)
	}

	active := &artifactGenerationLane{UnavailableBackend: NewUnavailableBackend(errors.New("wrong endpoint")), generation: "new"}
	historical := &artifactGenerationLane{UnavailableBackend: NewUnavailableBackend(nil), generation: "old"}
	m, err := NewMultiBackend(map[string]Backend{"hpc": active}, st, "hpc")
	if err != nil {
		t.Fatal(err)
	}
	m.SetHistoricalLane("hpc", "old", historical)

	page, err := m.TaskLogTail(ctx, "task-history", 20)
	if err != nil {
		t.Fatal(err)
	}
	if historical.logCalls != 1 || active.logCalls != 0 || len(page.Lines) != 1 || page.Lines[0] != "historical" {
		t.Fatalf("artifact routing: active=%d historical=%d page=%+v", active.logCalls, historical.logCalls, page)
	}

	m.RemoveHistoricalLane("hpc", "old")
	_, err = m.TaskLogTail(ctx, "task-history", 20)
	if err == nil || !strings.Contains(err.Error(), "unavailable target generation") {
		t.Fatalf("missing historical lane error = %v, want honest unavailable-generation error", err)
	}
	if active.logCalls != 0 {
		t.Fatal("missing historical generation fell through to active endpoint")
	}
}

func TestCompleteRetiringLaneMovesItOutOfLiveRefresh(t *testing.T) {
	st, err := store.Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	active := newGenerationTestLane()
	retiring := newGenerationTestLane()
	m := newGenerationMulti(t, st, active)
	m.SetRetiringLane("hpc", "old", retiring)
	m.CompleteRetiringLane("hpc", "old", retiring)

	if _, ok := m.retiringLane("hpc", "old"); ok {
		t.Fatal("completed generation remains live")
	}
	if got, ok := m.historicalLane("hpc", "old"); !ok || got != retiring {
		t.Fatal("completed generation was not retained for artifacts")
	}
}

func TestCleanUsesTaskGenerationArtifactEndpoint(t *testing.T) {
	st, err := store.Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	ctx := context.Background()
	if err := st.InsertJob(ctx, &store.JobRow{
		ID: "job-clean-history", ProjectName: "p", Status: "done", TotalTasks: 1,
		Target: "hpc", CreatedAt: time.Now(),
	}); err != nil {
		t.Fatal(err)
	}
	finished := time.Now()
	if err := st.InsertTask(ctx, &store.TaskRow{
		ID: "task-clean-history", JobID: "job-clean-history", ProjectName: "p",
		Command: "true", ParamsJSON: "{}", Status: "success", Target: "hpc",
		TargetGeneration: "old", TaskDir: t.TempDir(), EnqueuedAt: time.Now(), FinishedAt: &finished,
	}); err != nil {
		t.Fatal(err)
	}
	activeFS := &recordingArtifactFS{}
	historyFS := &recordingArtifactFS{}
	active := &artifactGenerationLane{UnavailableBackend: NewUnavailableBackend(nil), generation: "new", fs: activeFS}
	history := &artifactGenerationLane{UnavailableBackend: NewUnavailableBackend(nil), generation: "old", fs: historyFS}
	m, err := NewMultiBackend(map[string]Backend{"hpc": active}, st, "hpc")
	if err != nil {
		t.Fatal(err)
	}
	m.SetHistoricalLane("hpc", "old", history)
	result, err := m.Clean(ctx, CleanOptions{TaskID: "task-clean-history"})
	if err != nil {
		t.Fatal(err)
	}
	if result.Tasks != 1 || historyFS.execCalls != 1 || activeFS.execCalls != 0 {
		t.Fatalf("clean result=%+v active exec=%d historical exec=%d", result, activeFS.execCalls, historyFS.execCalls)
	}
}

func TestJobArtifactsPartitionEveryTaskByOwningGeneration(t *testing.T) {
	st, err := store.Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	ctx := context.Background()
	if err := st.InsertJob(ctx, &store.JobRow{
		ID: "job-mixed-artifacts", ProjectName: "p", Status: "done", TotalTasks: 2,
		Target: "hpc", CreatedAt: time.Now(),
	}); err != nil {
		t.Fatal(err)
	}
	finished := time.Now()
	for _, task := range []store.TaskRow{
		{ID: "task-old", JobID: "job-mixed-artifacts", ProjectName: "p", Command: "true",
			ParamsJSON: "{}", Status: "success", Target: "hpc", TargetGeneration: "old",
			TaskDir: "/cluster-old/task-old", LogPath: "/cluster-old/task-old/stdout.log",
			EnqueuedAt: time.Now(), FinishedAt: &finished},
		{ID: "task-new", JobID: "job-mixed-artifacts", ProjectName: "p", Command: "true",
			ParamsJSON: "{}", Status: "success", Target: "hpc", TargetGeneration: "new",
			TaskDir: "/cluster-new/task-new", LogPath: "/cluster-new/task-new/stdout.log",
			EnqueuedAt: time.Now(), FinishedAt: &finished},
	} {
		row := task
		if err := st.InsertTask(ctx, &row); err != nil {
			t.Fatal(err)
		}
	}
	oldFS := &recordingArtifactFS{}
	newFS := &recordingArtifactFS{}
	active := &artifactGenerationLane{UnavailableBackend: NewUnavailableBackend(nil), generation: "new", fs: newFS}
	history := &artifactGenerationLane{UnavailableBackend: NewUnavailableBackend(nil), generation: "old", fs: oldFS}
	m, err := NewMultiBackend(map[string]Backend{"hpc": active}, st, "hpc")
	if err != nil {
		t.Fatal(err)
	}
	m.SetHistoricalLane("hpc", "old", history)

	if _, err := m.JobLogSearch(ctx, "job-mixed-artifacts", "needle"); err != nil {
		t.Fatal(err)
	}
	if _, err := m.JobActivity(ctx, "job-mixed-artifacts"); err != nil {
		t.Fatal(err)
	}
	oldCommands := strings.Join(oldFS.commands, "\n")
	newCommands := strings.Join(newFS.commands, "\n")
	if oldFS.execCalls != 2 || !strings.Contains(oldCommands, "/cluster-old/task-old") || strings.Contains(oldCommands, "/cluster-new/task-new") {
		t.Fatalf("old-generation commands (%d): %s", oldFS.execCalls, oldCommands)
	}
	if newFS.execCalls != 2 || !strings.Contains(newCommands, "/cluster-new/task-new") || strings.Contains(newCommands, "/cluster-old/task-old") {
		t.Fatalf("new-generation commands (%d): %s", newFS.execCalls, newCommands)
	}
}
