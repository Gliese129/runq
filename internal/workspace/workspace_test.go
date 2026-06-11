package workspace

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/gliese129/runq/internal/job"
	"github.com/gliese129/runq/internal/project"
)

func TestPathHelpers(t *testing.T) {
	root := filepath.Join("root", "jobs")
	dir := TaskDir(root, "task1")
	if dir != filepath.Join(root, "task1") {
		t.Fatalf("TaskDir = %q", dir)
	}
	if ParamsPath(dir) != filepath.Join(dir, "params.json") {
		t.Fatalf("ParamsPath mismatch")
	}
	if MetricsPath(dir) != filepath.Join(dir, "metrics.jsonl") {
		t.Fatalf("MetricsPath mismatch")
	}
	if CheckpointsDir(dir) != filepath.Join(dir, "checkpoints") {
		t.Fatalf("CheckpointsDir mismatch")
	}
	if WandbConfigPath(dir) != filepath.Join(dir, "wandb_config.json") {
		t.Fatalf("WandbConfigPath mismatch")
	}
}

func TestWriteCreatesWorkspace(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "task1")
	params := job.TaskParams{"lr": 0.1, "seed": 7}
	wandb := &project.WandbConfig{Project: "exp", Entity: "lab"}

	if err := Write(dir, params, wandb, ""); err != nil {
		t.Fatalf("Write: %v", err)
	}
	if info, err := os.Stat(CheckpointsDir(dir)); err != nil || !info.IsDir() {
		t.Fatalf("checkpoint dir missing or not dir: info=%v err=%v", info, err)
	}

	var gotParams map[string]any
	b, err := os.ReadFile(ParamsPath(dir))
	if err != nil {
		t.Fatalf("read params: %v", err)
	}
	if err := json.Unmarshal(b, &gotParams); err != nil {
		t.Fatalf("unmarshal params: %v", err)
	}
	if gotParams["lr"] != 0.1 {
		t.Fatalf("params lr = %#v", gotParams["lr"])
	}

	var gotWandb project.WandbConfig
	b, err = os.ReadFile(WandbConfigPath(dir))
	if err != nil {
		t.Fatalf("read wandb config: %v", err)
	}
	if err := json.Unmarshal(b, &gotWandb); err != nil {
		t.Fatalf("unmarshal wandb config: %v", err)
	}
	if gotWandb.Project != "exp" || gotWandb.Entity != "lab" {
		t.Fatalf("wandb config = %+v", gotWandb)
	}
}

func TestWriteOmitsWandbWhenNil(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "task1")
	if err := Write(dir, job.TaskParams{"x": 1}, nil, ""); err != nil {
		t.Fatalf("Write: %v", err)
	}
	if _, err := os.Stat(WandbConfigPath(dir)); !os.IsNotExist(err) {
		t.Fatalf("wandb config should be absent, err=%v", err)
	}
}

func TestJobDirName(t *testing.T) {
	cases := []struct{ note, id, want string }{
		{"eval-v3", "jb1a2b3c4d", "eval-v3-jb1a2b3c4d"},
		{"", "jb1a2b3c4d", "jb1a2b3c4d"},
		{"模型 eval/8B", "jb1a2b3c4d", "eval-8B-jb1a2b3c4d"},
		{"---", "jb1a2b3c4d", "jb1a2b3c4d"}, // all-separator note → bare id
	}
	for _, c := range cases {
		if got := JobDirName(c.note, c.id); got != c.want {
			t.Errorf("JobDirName(%q,%q) = %q, want %q", c.note, c.id, got, c.want)
		}
	}
}
