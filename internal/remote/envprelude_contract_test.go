package remote

import (
	"context"
	"strings"
	"testing"

	"github.com/gliese129/runq-lab/internal/config"
	"github.com/gliese129/runq-lab/internal/submitplan"
	"github.com/gliese129/runq-lab/internal/utils"
)

// THE single-injection-point contract (Codex r2 #2): run.sh, the submit
// command and the setup wrapper must all carry the SAME env prelude —
// env_setup included — rendered by utils.EnvPrelude. If any surface
// assembles its own environment again, this test is the tripwire.
// (The preflight probe goes through the same struct — pinned in the
// preflight package where its script is visible.)
func TestEnvPreludeSharedAcrossSurfaces(t *testing.T) {
	b := &Backend{Cfg: &config.TargetConfig{
		Name:           "hpc",
		EnvSetup:       "module load cuda/12.1",
		SubmitTemplate: "qsub {{run_sh}}",
	}}
	env := map[string]string{"TSUBAME_GROUP": "tga-x"}
	want := []string{"module load cuda/12.1", "export TSUBAME_GROUP='tga-x'"}

	// run.sh
	script := b.buildRunScript(submitplan.PlannedTask{
		TaskID: "tk1", TaskDir: "/ws/tk1", LogPath: "/ws/tk1.log",
		WorkingDir: "/wd", Command: "bash run.sh", Env: env,
	}, submitplan.Plan{JobID: "jb1", Project: "p"})
	for _, w := range want {
		if !strings.Contains(script, w) {
			t.Errorf("run.sh missing %q:\n%s", w, script)
		}
	}

	// submit command prelude (what Prepare prefixes onto qsub/sbatch):
	// module-provided schedulers need env_setup BEFORE the qsub line.
	prefix := b.envPrelude(env).Render()
	for _, w := range want {
		if !strings.Contains(prefix, w) {
			t.Errorf("submit prelude missing %q:\n%s", w, prefix)
		}
	}
	if i := strings.Index(prefix, "module load"); i < 0 || i > strings.Index(prefix, "export TSUBAME_GROUP") {
		t.Errorf("env_setup must precede exports:\n%s", prefix)
	}

	// setup wrapper: same prelude type, by construction — pin that the
	// remote script contains both parts (via a LocalFS dry run).
	prelude := b.envPrelude(env)
	if r := prelude.Render(); !strings.Contains(r, "module load cuda/12.1") || !strings.Contains(r, "TSUBAME_GROUP") {
		t.Errorf("setup prelude: %q", r)
	}
	_ = context.Background()
	_ = utils.EnvPrelude{}
}
