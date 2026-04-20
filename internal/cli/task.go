package cli

import (
	"fmt"

	"github.com/gliese129/runq/internal/scheduler"
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
	id := args[0]
	var task scheduler.Task
	if err := doAndDecode("GET", "/api/tasks/"+id, nil, &task); err != nil {
		return err
	}
	printJSON(task)
	return nil
}

var taskRetryCmd = &cobra.Command{
	Use:   "retry <task_id>",
	Short: "Manually retry a failed task",
	Args:  cobra.ExactArgs(1),
	// TODO: needs API endpoint (POST /api/tasks/:id/retry)
	RunE: func(cmd *cobra.Command, args []string) error {
		return fmt.Errorf("task retry: not implemented (needs API endpoint)")
	},
}

func init() {
	taskCmd.AddCommand(taskShowCmd)
	taskCmd.AddCommand(taskRetryCmd)
	rootCmd.AddCommand(taskCmd)
}
