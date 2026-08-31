package remote

import (
	"os/exec"
	"strings"
	"testing"

	"github.com/gliese129/runq-lab/internal/config"
	"github.com/gliese129/runq-lab/internal/submitplan"
)

func TestRunqdSubmitTemplatePropagatesTimeout(t *testing.T) {
	cfg, err := config.HPCPreset("runq")
	if err != nil {
		t.Fatalf("HPCPreset(runq): %v", err)
	}

	for _, tc := range []struct {
		name    string
		timeout int
		want    string
	}{
		{name: "configured", timeout: 125, want: "125"},
		{name: "no timeout", timeout: 0, want: "0"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			task := submitplan.PlannedTask{
				TaskID:     "task; echo injected",
				TaskDir:    "/tmp/task dir",
				LogPath:    "/tmp/task dir/task's.log",
				Name:       "name with spaces",
				GPUsNeeded: 2,
				Timeout:    tc.timeout,
			}
			plan := submitplan.Plan{JobID: "job-1", Project: "project; false"}
			runsh := "/tmp/task dir/run's.sh"

			rendered, err := renderSubmitCmd(cfg.SubmitTemplate, task, plan, runsh)
			if err != nil {
				t.Fatalf("renderSubmitCmd: %v", err)
			}
			if strings.Contains(rendered, "{{") {
				t.Fatalf("rendered command contains an unresolved placeholder: %s", rendered)
			}

			// Execute only a shell function named runqd. It prints its argv, which
			// verifies both timeout propagation and quoting of hostile values.
			script := `runqd() { printf '%s\n' "$@"; }; ` + rendered
			out, err := exec.Command("sh", "-c", script).CombinedOutput()
			if err != nil {
				t.Fatalf("rendered command is not shell-safe: %v\n%s", err, out)
			}
			got := strings.Split(strings.TrimSuffix(string(out), "\n"), "\n")
			want := []string{
				"submit", "--gpus", "2", "--timeout", tc.want,
				"--task-id", task.TaskID,
				"--task-dir", task.TaskDir,
				"--name", task.Name,
				"--project", plan.Project,
				"--log", task.LogPath,
				runsh,
			}
			if strings.Join(got, "\x00") != strings.Join(want, "\x00") {
				t.Fatalf("runqd argv mismatch\n got: %#v\nwant: %#v\ncommand: %s", got, want, rendered)
			}
		})
	}
}

func TestRunqdWrapperKeepsSDKMachineProtocolEnabled(t *testing.T) {
	task := submitplan.PlannedTask{
		TaskID: "task-1", TaskDir: "/tmp/task-1", LogPath: "/tmp/task-1/task.log",
		WorkingDir: "/tmp", Command: "true",
	}
	plan := submitplan.Plan{JobID: "job-1", Project: "project"}

	runqdScript := (&Backend{Cfg: &config.TargetConfig{Scheduler: "runq"}}).buildRunScript(task, plan)
	if strings.Contains(runqdScript, "RUNQ_NO_DAEMON") {
		t.Fatalf("runqd wrapper disabled the SDK machine protocol:\n%s", runqdScript)
	}

	hpcScript := (&Backend{Cfg: &config.TargetConfig{Scheduler: "slurm"}}).buildRunScript(task, plan)
	if !strings.Contains(hpcScript, "export RUNQ_NO_DAEMON='1'") {
		t.Fatalf("traditional HPC wrapper lost no-daemon guard:\n%s", hpcScript)
	}
}
