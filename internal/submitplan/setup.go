package submitplan

import (
	"context"
	"fmt"
	"os"
	"os/exec"

	"github.com/gliese129/runq/internal/job"
	"github.com/gliese129/runq/internal/project"
)

// RunSetup executes the project's setup_command, if any. Backends call this
// AFTER Build (which stays side-effect-free) and BEFORE persisting or
// submitting anything, so a setup failure aborts with zero residue.
//
// Execution context: the node runq runs on (login node in HPC mode), cwd =
// working_dir, env = process env + project environment. Output streams to
// the caller's stdout/stderr — a model pre-download should be watchable.
func RunSetup(ctx context.Context, proj *project.Config, cfg job.JobConfig) error {
	if proj == nil || proj.SetupCommand == "" {
		return nil
	}
	rendered, err := job.RenderSetup(proj.SetupCommand, &cfg)
	if err != nil {
		return fmt.Errorf("setup_command: %w", err)
	}

	cmd := exec.CommandContext(ctx, "sh", "-c", rendered)
	cmd.Dir = proj.WorkingDir
	cmd.Env = os.Environ()
	for k, v := range proj.Environment {
		cmd.Env = append(cmd.Env, k+"="+v)
	}
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr

	fmt.Printf("setup: %s\n", rendered)
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("setup_command failed (nothing submitted): %w", err)
	}
	return nil
}
