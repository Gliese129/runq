package cli

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/gliese129/runq-lab/internal/api"
	"github.com/gliese129/runq-lab/internal/backend"
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
	return withBackend(cmd, func(b backend.Backend) error {
		view, err := b.GetTask(cmd.Context(), args[0])
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

var taskRetryYes bool

func runTaskRetry(cmd *cobra.Command, args []string) error {
	id := args[0]
	return withBackend(cmd, func(b backend.Backend) error {
		err := b.RetryTask(cmd.Context(), id)
		// RQ-75: the target's config changed since this task was submitted
		// — the rerun will use the NEW config. Ask (y/N) unless -y.
		var apiErr *api.APIError
		if errors.As(err, &apiErr) && apiErr.Code == backend.CodeGenerationChanged {
			if !taskRetryYes {
				fmt.Fprintf(cmd.ErrOrStderr(),
					"W: target config CHANGED since task %s was submitted (%s).\n   The rerun will use the NEW config.\n", id, apiErr.Details)
				fmt.Fprint(cmd.ErrOrStderr(), "Rerun anyway? [y/N]: ")
				var answer string
				_, _ = fmt.Fscanln(cmd.InOrStdin(), &answer)
				if a := strings.ToLower(strings.TrimSpace(answer)); a != "y" && a != "yes" {
					return fmt.Errorf("aborted")
				}
			}
			cr, ok := b.(interface {
				RetryTaskConfirm(context.Context, string) error
			})
			if !ok {
				return err
			}
			err = cr.RetryTaskConfirm(cmd.Context(), id)
		}
		if err != nil {
			return err
		}
		fmt.Printf("task %s re-enqueued\n", id)
		return nil
	})
}

func init() {
	taskRetryCmd.Flags().BoolVarP(&taskRetryYes, "yes", "y", false,
		"skip the confirmation when the target config changed since submission")
	taskCmd.AddCommand(taskShowCmd)
	taskCmd.AddCommand(taskRetryCmd)
	taskCmd.GroupID = groupManagement
	rootCmd.AddCommand(taskCmd)
}
