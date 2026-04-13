package cli

import (
	"fmt"

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
	RunE: func(cmd *cobra.Command, args []string) error {
		fmt.Println("TODO: task show")
		return nil
	},
}

var taskRetryCmd = &cobra.Command{
	Use:   "retry <task_id>",
	Short: "Manually retry a failed task",
	Args:  cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		fmt.Println("TODO: task retry")
		return nil
	},
}

func init() {
	taskCmd.AddCommand(taskShowCmd)
	taskCmd.AddCommand(taskRetryCmd)
	rootCmd.AddCommand(taskCmd)
}
