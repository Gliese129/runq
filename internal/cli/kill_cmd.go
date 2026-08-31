package cli

import (
	"errors"
	"fmt"

	"github.com/gliese129/runq-lab/internal/backend"
	"github.com/spf13/cobra"
)

// ── runq kill (shortcut for task kill) ──

var killCmd = &cobra.Command{
	Use:   "kill <task_id | job_id>",
	Short: "Kill a task or all tasks in a job",
	Args:  cobra.ExactArgs(1),
	RunE:  runKill,
}

func runKill(cmd *cobra.Command, args []string) error {
	id := args[0]
	jsonOut, _ := cmd.Flags().GetBool("json")

	return withBackend(cmd, func(be backend.Backend) error {
		ctx := cmd.Context()

		// Prefix dispatch: try task, fall through to job — but ONLY on
		// not-found (D21: branch on the code, never on error presence).
		// Any other error is a real answer: a partial-cancel report
		// ("killed 2; could not cancel 2: ...") is the most valuable
		// output this command produces — swallowing it into "no task or
		// job found" was RQ-65 retest #3.
		taskErr := be.KillTask(ctx, id)
		if taskErr == nil {
			if jsonOut {
				printJSON(map[string]bool{"ok": true})
				return nil
			}
			fmt.Printf("task %s killed\n", id)
			return nil
		}
		if !errors.Is(taskErr, backend.ErrNotFound) {
			return taskErr
		}

		jobErr := be.KillJob(ctx, id)
		if jobErr == nil {
			if jsonOut {
				printJSON(map[string]bool{"ok": true})
				return nil
			}
			fmt.Printf("job %s killed\n", id)
			return nil
		}
		if !errors.Is(jobErr, backend.ErrNotFound) {
			return jobErr
		}

		return fmt.Errorf("no task or job found with id %q", id)
	})
}

func init() {
	killCmd.Flags().Bool("json", false, "output raw JSON")
	killCmd.Flags().StringP("target", "t", "", "Compute target (for target-scoped resolution)")

	killCmd.GroupID = groupCore
	rootCmd.AddCommand(killCmd)
}
