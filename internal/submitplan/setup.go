package submitplan

import (
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"sort"
	"strings"

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
// fsys == nil = local execution (local targets, tests). Output streams
// to the caller's stdout/stderr — a model pre-download should be
// watchable.
func RunSetup(ctx context.Context, proj *project.Config, cfg job.JobConfig, fsys rfs.FS, envSetup string) error {
	if proj == nil || proj.SetupCommand == "" {
		return nil
	}
	rendered, err := job.RenderSetup(proj.SetupCommand, &cfg)
	if err != nil {
		return fmt.Errorf("setup_command: %w", err)
	}

	if fsys == nil {
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

	// Remote: assemble the same environment run.sh would give the task —
	// env_setup first (make conda/PATH visible in the non-interactive
	// shell), then the project environment, then cd + command.
	var script strings.Builder
	if s := strings.TrimSpace(envSetup); s != "" {
		script.WriteString(s + "\n")
	}
	keys := make([]string, 0, len(proj.Environment))
	for k := range proj.Environment {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		script.WriteString("export " + k + "=" + utils.ShellQuote(proj.Environment[k]) + "\n")
	}
	script.WriteString("cd " + utils.ShellQuote(proj.WorkingDir) + " || exit 1\n")
	script.WriteString(rendered + "\n")

	fmt.Printf("setup (on target): %s\n", rendered)
	stream, err := fsys.ExecStream(ctx, "sh", "-c", script.String())
	if err != nil {
		return fmt.Errorf("setup_command failed to start on target: %w", err)
	}
	defer stream.Close()
	if _, err := io.Copy(os.Stdout, stream); err != nil {
		return fmt.Errorf("setup_command failed on target (nothing submitted): %w", err)
	}
	return nil
}
