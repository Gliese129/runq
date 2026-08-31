package remote

import (
	"os"
	"strings"
	"testing"

	"github.com/gliese129/runq-lab/internal/config"
	"github.com/gliese129/runq-lab/internal/submitplan"
)

// RQ-76 ①: run.sh restores CONTEXT instead of rewriting scripts — an
// absolute HOME (resolved where sshd sets it) is exported first, then
// the target's env_setup runs verbatim, so every `~` inside it expands
// natively even when the scheduler stripped HOME (--export=NONE).
func TestRunScriptHomeAndEnvSetup(t *testing.T) {
	b := &Backend{Cfg: &config.TargetConfig{
		Name:     "tsubame",
		EnvSetup: "source ~/miniconda3/etc/profile.d/conda.sh",
	}}
	script := b.buildRunScript(submitplan.PlannedTask{
		TaskID: "tk1", TaskDir: "/ws/jb1/tk1", LogPath: "/ws/jb1/tk1.log",
		WorkingDir: "/home/u/proj", Command: "bash scripts/run.sh",
	}, submitplan.Plan{JobID: "jb1", Project: "p"})

	// nil FS → local resolution: HOME is this process's own.
	home, err := os.UserHomeDir()
	if err != nil {
		t.Skip("no local home")
	}
	homeLine := "export HOME='" + home + "'"
	if !strings.Contains(script, homeLine) {
		t.Errorf("missing HOME restoration %q:\n%s", homeLine, script)
	}
	if !strings.Contains(script, "source ~/miniconda3/etc/profile.d/conda.sh") {
		t.Errorf("env_setup not injected:\n%s", script)
	}

	// Ordering contract: log redirect → HOME → env_setup → env exports →
	// user command. env_setup output must be logged; explicit env wins.
	pos := func(sub string) int { return strings.Index(script, sub) }
	if !(pos("exec >> ") < pos(homeLine) && pos(homeLine) < pos("miniconda3") &&
		pos("miniconda3") < pos("export RUNQ_TASK_ID") && pos("miniconda3") < pos("bash scripts/run.sh")) {
		t.Errorf("ordering wrong:\n%s", script)
	}

	// No env_setup + HOME resolution results are cached on the lane: a
	// second build must not re-exec (sync.Once) — smoke via same output.
	script2 := b.buildRunScript(submitplan.PlannedTask{
		TaskID: "tk2", TaskDir: "/ws/jb1/tk2", LogPath: "/ws/jb1/tk2.log",
		WorkingDir: "/home/u/proj", Command: "true",
	}, submitplan.Plan{JobID: "jb1", Project: "p"})
	if !strings.Contains(script2, homeLine) {
		t.Errorf("cached HOME lost on second build:\n%s", script2)
	}
}

// env_setup participates in the target's semantic generation: editing it
// must rotate the lane (which is what re-resolves HOME).
func TestEnvSetupMovesGeneration(t *testing.T) {
	a := config.TargetConfig{Name: "x", Scheduler: "slurm", SubmitTemplate: "s"}
	c := a
	c.EnvSetup = "module load cuda"
	if a.SemanticEquals(&c) {
		t.Fatal("env_setup edit judged as no change")
	}
	if a.SemanticGeneration() == c.SemanticGeneration() {
		t.Fatal("env_setup edit did not move the generation")
	}
}
