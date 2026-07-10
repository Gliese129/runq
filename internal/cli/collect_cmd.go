package cli

import (
	"fmt"
	"strings"

	"github.com/gliese129/runq/internal/backend"
	"github.com/gliese129/runq/internal/utils"
	"github.com/spf13/cobra"
)

// ── runq collect — per-task params + best metric, ranked ──

var collectCmd = &cobra.Command{
	Use:   "collect <job_id> --key <metric>",
	Short: "Per-task params + best metric value, ranked",
	Example: `  runq collect abc123 --key loss
  runq collect abc123 --key acc --max
  runq collect abc123 --key loss --json`,
	Args: cobra.ExactArgs(1),
	RunE: runCollect,
}

func runCollect(cmd *cobra.Command, args []string) error {
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

		if jsonOut, _ := cmd.Flags().GetBool("json"); jsonOut {
			printJSON(rows)
			return nil
		}
		if len(rows) == 0 {
			fmt.Println("no tasks")
			return nil
		}
		w := newTable()
		fmt.Fprintf(w, "TASK_ID\tSTATUS\t%s\tPARAMS\n", strings.ToUpper(key))
		for _, r := range rows {
			val := "-"
			if r.HasValue {
				val = fmt.Sprintf("%v", r.Best)
			}
			fmt.Fprintf(w, "%s\t%s\t%s\t%v\n",
				utils.IDColor(r.TaskID), utils.StatusColor(r.Status), val, r.Params)
		}
		w.Flush()
		return nil
	})
}

func init() {
	collectCmd.Flags().String("key", "", "metric key to rank by (e.g. loss, acc)")
	collectCmd.Flags().Bool("max", false, "rank by maximum instead of minimum")
	collectCmd.Flags().Bool("json", false, "output raw JSON")
	collectCmd.Flags().StringP("target", "t", "", "Compute target (for target-scoped resolution)")

	collectCmd.GroupID = groupCore
	rootCmd.AddCommand(collectCmd)
}
