package backend

// Tests for the RQ2-1 c5 field exposures: JobDetail.data_dir derivation
// and the detail-only execution facts on TaskView.

import (
	"encoding/json"
	"testing"

	"github.com/gliese129/runq-lab/internal/store"
)

func TestJobDataDirFromTaskDirs(t *testing.T) {
	tasks := []store.TaskRow{
		{ID: "t0"}, // pre-L2C row without a dir — must be skipped, not Dir("")
		{ID: "t1", TaskDir: "/home/u/.runq/jobs/exp-8c1de4b0/t1"},
		{ID: "t2", TaskDir: "/home/u/.runq/jobs/exp-8c1de4b0/t2"},
	}
	if got := jobDataDir(tasks); got != "/home/u/.runq/jobs/exp-8c1de4b0" {
		t.Errorf("jobDataDir = %q", got)
	}
}

func TestJobDataDirEmptyWhenNoDirs(t *testing.T) {
	if got := jobDataDir([]store.TaskRow{{ID: "t1"}}); got != "" {
		t.Errorf("jobDataDir = %q, want empty", got)
	}
	if got := jobDataDir(nil); got != "" {
		t.Errorf("jobDataDir(nil) = %q, want empty", got)
	}
}

func TestBuildJobDetailCarriesDataDirAndOmitsWhenAbsent(t *testing.T) {
	job := store.JobRow{ID: "j1", ConfigJSON: "{}"}
	withDir := BuildJobDetail(job, []store.TaskRow{
		{ID: "t1", ParamsJSON: "{}", TaskDir: "/ws/exp-j1/t1"},
	}, nil)
	if withDir.DataDir != "/ws/exp-j1" {
		t.Errorf("DataDir = %q", withDir.DataDir)
	}

	without := BuildJobDetail(job, []store.TaskRow{{ID: "t1", ParamsJSON: "{}"}}, nil)
	buf, err := json.Marshal(without)
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]any
	_ = json.Unmarshal(buf, &m)
	if _, present := m["data_dir"]; present {
		t.Errorf("data_dir must be omitted when unresolvable, got %v", m["data_dir"])
	}
}

func TestApplyTaskDetailExposesExecutionFacts(t *testing.T) {
	task := store.TaskRow{
		ID: "t1", ParamsJSON: "{}",
		Command:    "python train.py --lr 0.1",
		WorkingDir: "/home/u/proj",
		TaskDir:    "/ws/exp-j1/t1",
	}
	view := BuildTaskView(task)
	// List shape stays lean: the builder itself must NOT expose them.
	if view.Command != "" || view.WorkingDir != "" || view.TaskDir != "" {
		t.Fatalf("list-shaped view already carries detail fields: %+v", view)
	}
	applyTaskDetail(&view, task)
	if view.Command != task.Command || view.WorkingDir != task.WorkingDir || view.TaskDir != task.TaskDir {
		t.Errorf("detail fields not applied: %+v", view)
	}
}
