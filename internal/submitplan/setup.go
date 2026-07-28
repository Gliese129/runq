package submitplan

import (
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"

	"github.com/gliese129/runq/internal/job"
	"github.com/gliese129/runq/internal/project"
	"github.com/gliese129/runq/internal/rfs"
	"github.com/gliese129/runq/internal/utils"
)

// RunSetup executes the project's setup_command, if any. Backends call this
// AFTER Build (which stays side-effect-free) and BEFORE persisting or
// submitting anything, so a setup failure aborts with zero residue.
//
// Execution context is WHERE THE TASK'S FILES LIVE (Codex r1 #4): for a
// remote target, fsys is the target's rfs.FS and the command runs on the
// login node — after the target's env_setup, cwd = working_dir, project
// environment exported. Running it on the daemon machine would download
// a model into the wrong filesystem and leave the target's cache empty.
// fsys == nil = local execution (local targets, tests). The prelude is
// THE environment (Codex r2 #3): callers pass the plan's merged env +
// target env_setup + resolved HOME through utils.EnvPrelude, so setup
// sees exactly what preflight probed and run.sh will export — .env,
// submit overrides and HOME included. Output streams to the caller's
// stdout/stderr — a model pre-download should be watchable.
func RunSetup(ctx context.Context, proj *project.Config, cfg job.JobConfig, fsys rfs.FS, prelude utils.EnvPrelude) error {
	if proj == nil || proj.SetupCommand == "" {
		return nil
	}
	rendered, err := job.RenderSetup(proj.SetupCommand, &cfg)
	if err != nil {
		return fmt.Errorf("setup_command: %w", err)
	}

	if fsys == nil {
		cmd := exec.CommandContext(ctx, "sh", "-c", prelude.Render()+rendered)
		cmd.Dir = proj.WorkingDir
		cmd.Env = os.Environ()
		cmd.Stdout = os.Stdout
		cmd.Stderr = os.Stderr

		fmt.Printf("setup: %s\n", rendered)
		if err := cmd.Run(); err != nil {
			return fmt.Errorf("setup_command failed (nothing submitted): %w", err)
		}
		return nil
	}

	// Remote: same prelude as every other execution surface, then cd +
	// command on the target's login node.
	script := prelude.Render() +
		"cd " + utils.ShellQuote(proj.WorkingDir) + " || exit 1\n" +
		rendered + "\n"

	fmt.Printf("setup (on target): %s\n", rendered)
	stream, err := fsys.ExecStream(ctx, "sh", "-c", script)
	if err != nil {
		return fmt.Errorf("setup_command failed to start on target: %w", err)
	}
	defer stream.Close()
	if _, err := io.Copy(os.Stdout, stream); err != nil {
		return fmt.Errorf("setup_command failed on target (nothing submitted): %w", err)
	}
	return nil
}
