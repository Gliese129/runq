package cli

import (
	"context"
	"fmt"

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
	_, mode, err := loadModeConfig()
	if err != nil {
		return err
	}
	backend, closeBackend, err := newBackend(mode)
	if err != nil {
		return err
	}
	defer closeBackend()
	jsonOut, _ := cmd.Flags().GetBool("json")

	// Try task kill first.
	err = backend.KillTask(context.Background(), id)
	if err == nil {
		if jsonOut {
			printJSON(map[string]bool{"ok": true})
			return nil
		}
		fmt.Printf("task %s killed\n", id)
		return nil
	}

	// If not found as task, try as job.
	err = backend.KillJob(context.Background(), id)
	if err == nil {
		if jsonOut {
			printJSON(map[string]bool{"ok": true})
			return nil
		}
		fmt.Printf("job %s killed\n", id)
		return nil
	}

	return fmt.Errorf("no task or job found with id %q", id)
}

func init() {
	killCmd.Flags().Bool("json", false, "output raw JSON")

	killCmd.GroupID = groupCore
	rootCmd.AddCommand(killCmd)
}
