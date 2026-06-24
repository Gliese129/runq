package cli

import (
	"fmt"

	"github.com/gliese129/runq/internal/backend"
	"github.com/spf13/cobra"
)

// ── runq status ──

var statusCmd = &cobra.Command{
	Use:   "status [job_id]",
	Short: "Show daemon status, queue length, and scheduling info",
	Args:  cobra.MaximumNArgs(1),
	RunE:  runStatus,
}

func runStatus(cmd *cobra.Command, args []string) error {
	jsonOut, _ := cmd.Flags().GetBool("json")

	return withBackend(func(be backend.Backend) error {
		ctx := cmd.Context()

		// With a job_id argument: show that job's details.
		if len(args) == 1 {
			detail, err := be.GetJob(ctx, args[0])
			if err != nil {
				return err
			}
			if jsonOut {
				printJSON(detail)
				return nil
			}
			return printDashboardDetail(detail)
		}

		// No arguments: show aggregate status (running/pending/GPU).
		jobs, err := be.ListJobs(ctx, "")
		if err != nil {
			return err
		}

		var running, pending int
		for _, j := range jobs {
			running += j.Tasks.Running
			pending += j.Tasks.Pending
		}

		gpusFree := 0
		if gpus, gerr := be.GPUStatus(ctx); gerr == nil {
			for _, g := range gpus {
				if g.TaskID == "" {
					gpusFree++
				}
			}
		}

		if jsonOut {
			printJSON(map[string]int{
				"running":   running,
				"pending":   pending,
				"gpus_free": gpusFree,
			})
			return nil
		}

		fmt.Printf("Running:   %d\n", running)
		fmt.Printf("Pending:   %d\n", pending)
		fmt.Printf("GPUs free: %d\n", gpusFree)
		return nil
	})
}

func init() {
	statusCmd.Flags().Bool("json", false, "output raw JSON")

	statusCmd.GroupID = groupDiag
	rootCmd.AddCommand(statusCmd)
}
