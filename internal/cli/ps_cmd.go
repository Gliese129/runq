package cli

import (
	"fmt"

	"github.com/gliese129/runq/internal/api"
	"github.com/gliese129/runq/internal/backend"
	"github.com/spf13/cobra"
)

// ── runq ps / ls（D16 双视角）──
//
// 无参数 = job 表（GET /jobs）；带 job_id = 该 job 的 task 表（GET
// /tasks?job=）。两个视角共用 --json / --target / --fresh。

var psCmd = &cobra.Command{
	Use:     "ps [job_id]",
	Aliases: []string{"ls"},
	Short:   "List jobs, or the tasks of one job",
	Example: `  runq ps               # job table
  runq ps 8f2a           # task table for job 8f2a
  runq ps --json
  runq ps -t tsubame --fresh`,
	Args: cobra.MaximumNArgs(1),
	RunE: runPs,
}

func runPs(cmd *cobra.Command, args []string) error {
	output, _ := cmd.Flags().GetString("output")
	jsonOut, _ := cmd.Flags().GetBool("json")
	jsonOut = jsonOut || output == "json"

	return withBackend(cmd, func(be backend.Backend) error {
		// Task view: ps <job_id> → flat task table.
		if len(args) == 1 {
			applyFresh(cmd, be, args[0])
			p, ok := be.(*api.Proxy)
			if !ok {
				return fmt.Errorf("task listing requires the daemon proxy")
			}
			tasks, err := p.ListTasks(cmd.Context(), args[0], "")
			if err != nil {
				return err
			}
			if jsonOut {
				printJSON(tasks)
				return nil
			}
			return printDashboardTasks(tasks)
		}

		// Job view.
		applyFresh(cmd, be, "")
		jobs, err := be.ListJobs(cmd.Context(), "")
		if err != nil {
			return err
		}
		if jsonOut {
			printJSON(jobs)
			return nil
		}
		return printDashboardJobs(jobs)
	})
}

func init() {
	psCmd.Flags().StringP("output", "o", "", "Output format (json)")
	psCmd.Flags().Bool("json", false, "output raw JSON")
	psCmd.Flags().StringP("target", "t", "", "Filter by compute target")

	psCmd.GroupID = groupCore
	rootCmd.AddCommand(psCmd)
}
