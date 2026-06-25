package cli

import (
	"fmt"

	"github.com/gliese129/runq/internal/backend"
	"github.com/spf13/cobra"
)

var taskCmd = &cobra.Command{
	Use:   "task",
	Short: "Manage individual tasks",
}

var taskShowCmd = &cobra.Command{
	Use:   "show <task_id>",
	Short: "Show task details (command, GPU, retry count, etc.)",
	Args:  cobra.ExactArgs(1),
	RunE:  runTaskShow,
}

func runTaskShow(cmd *cobra.Command, args []string) error {
	return withBackend(func(b backend.Backend) error {
		view, _, err := b.GetTask(cmd.Context(), args[0])
		if err != nil {
			return err
		}
		printJSON(view)
		return nil
	})
}

var taskRetryCmd = &cobra.Command{
	Use:   "retry <task_id>",
	Short: "Manually retry a failed task",
	Args:  cobra.ExactArgs(1),
	RunE:  runTaskRetry,
}

func runTaskRetry(cmd *cobra.Command, args []string) error {
	id := args[0]
	return withBackend(func(b backend.Backend) error {
		if err := b.RetryTask(cmd.Context(), id); err != nil {
			return err
		}
		fmt.Printf("task %s re-enqueued\n", id)
		return nil
	})
}

func init() {
	taskCmd.AddCommand(taskShowCmd)
	taskCmd.AddCommand(taskRetryCmd)
	taskCmd.GroupID = groupManagement
	rootCmd.AddCommand(taskCmd)
}
