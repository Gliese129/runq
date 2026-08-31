package cli

import (
	"fmt"

	"github.com/gliese129/runq-lab/internal/backend"
	"github.com/gliese129/runq-lab/internal/utils"
	"github.com/spf13/cobra"
)

// ── runq best — show the task with the best metric value ──

var bestCmd = &cobra.Command{
	Use:   "best <job_id> --key <metric>",
	Short: "Show the task with the best value of a metric",
	Example: `  runq best abc123 --key loss
  runq best abc123 --key acc --max
  runq best abc123 --key loss --json`,
	Args: cobra.ExactArgs(1),
	RunE: runBest,
}

func runBest(cmd *cobra.Command, args []string) error {
	key, _ := cmd.Flags().GetString("key")
	if key == "" {
		return fmt.Errorf("--key is required (e.g. --key loss)")
	}
	maximize, _ := cmd.Flags().GetBool("max")

	return withBackend(cmd, func(be backend.Backend) error {
		ctx := cmd.Context()
		// Refresh for poll-model backends; push-model backends no-op.
		_ = be.RefreshJob(ctx, args[0])

		rows, err := be.CompareMetrics(ctx, args[0], key, maximize)
		if err != nil {
			return err
		}

		var top *backend.CompareRow
		for i := range rows {
			if rows[i].HasValue {
				top = &rows[i]
				break
			}
		}

		if jsonOut, _ := cmd.Flags().GetBool("json"); jsonOut {
			printJSON(top)
			return nil
		}
		if top == nil {
			fmt.Printf("no task has metric %q yet\n", key)
			return nil
		}
		fmt.Printf("best %s = %v\n  task   %s (%s)\n  params %v\n",
			key, top.Best, utils.IDColor(top.TaskID), utils.StatusColor(top.Status), top.Params)
		return nil
	})
}

func init() {
	bestCmd.Flags().String("key", "", "metric key to rank by (e.g. loss, acc)")
	bestCmd.Flags().Bool("max", false, "rank by maximum instead of minimum")
	bestCmd.Flags().Bool("json", false, "output raw JSON")
	bestCmd.Flags().StringP("target", "t", "", "Compute target (for target-scoped resolution)")

	bestCmd.GroupID = groupCore
	rootCmd.AddCommand(bestCmd)
}
