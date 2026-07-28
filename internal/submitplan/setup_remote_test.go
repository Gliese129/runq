package submitplan

import (
	"context"
	"os"
	"strings"
	"testing"

	"github.com/gliese129/runq/internal/job"
	"github.com/gliese129/runq/internal/project"
	"github.com/gliese129/runq/internal/rfs"
	"github.com/gliese129/runq/internal/utils"
)

// Codex r1 #4: with a target FS, setup_command executes THROUGH it (on
// the login node for real remotes), inside cd working_dir, with the
// project environment and the target's env_setup applied — not on the
// machine the daemon happens to run on.
func TestRunSetupOnTargetFS(t *testing.T) {
	dir := t.TempDir()
	proj := &project.Config{
		ProjectName:  "p",
		WorkingDir:   dir,
		SetupCommand: `printf '%s' "$FROM_ENV:$FROM_SETUP" > setup_ran.txt`,
		Environment:  map[string]string{"FROM_ENV": "pe"},
	}
	err := RunSetup(context.Background(), proj, job.JobConfig{Project: "p"},
		rfs.NewLocalFS(), utils.EnvPrelude{EnvSetup: "export FROM_SETUP=es", Env: proj.Environment})
	if err != nil {
		t.Fatalf("RunSetup: %v", err)
	}
	buf, rerr := os.ReadFile(dir + "/setup_ran.txt")
	if rerr != nil {
		t.Fatalf("setup did not run in working_dir: %v", rerr)
	}
	if string(buf) != "pe:es" {
		t.Fatalf("environment/env_setup not applied: %q", buf)
	}

	// Non-zero exit must abort with an error.
	proj.SetupCommand = "false"
	if err := RunSetup(context.Background(), proj, job.JobConfig{Project: "p"}, rfs.NewLocalFS(), utils.EnvPrelude{}); err == nil {
		t.Fatal("failing setup passed silently")
	} else if !strings.Contains(err.Error(), "setup_command") {
		t.Fatalf("error lost context: %v", err)
	}
}
