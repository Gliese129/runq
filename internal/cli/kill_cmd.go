package cli

import (
	"fmt"

	"github.com/gliese129/runq/internal/backend"
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

	return withBackend(func(be backend.Backend) error {
		ctx := cmd.Context()

		// Try task kill first.
		err := be.KillTask(ctx, id)
		if err == nil {
			if jsonOut {
				printJSON(map[string]bool{"ok": true})
				return nil
			}
			fmt.Printf("task %s killed\n", id)
			return nil
		}

		// If not found as task, try as job.
		err = be.KillJob(ctx, id)
		if err == nil {
			if jsonOut {
				printJSON(map[string]bool{"ok": true})
				return nil
			}
			fmt.Printf("job %s killed\n", id)
			return nil
		}

		return fmt.Errorf("no task or job found with id %q", id)
	})
}

func init() {
	killCmd.Flags().Bool("json", false, "output raw JSON")

	killCmd.GroupID = groupCore
	rootCmd.AddCommand(killCmd)
}
