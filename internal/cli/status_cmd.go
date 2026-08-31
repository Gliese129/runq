package cli

import (
	"fmt"

	"github.com/gliese129/runq-lab/internal/api"
	"github.com/gliese129/runq-lab/internal/backend"
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
	target := resolveTarget(cmd)

	return withBackend(cmd, func(be backend.Backend) error {
		ctx := cmd.Context()

		// With a job_id argument: show that job's details (ID-based, no target filter).
		if len(args) == 1 {
			applyFresh(cmd, be, args[0])
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

		// No arguments: daemon health first (spec §7.2: status → /health)…
		var health map[string]any
		if p, ok := be.(*api.Proxy); ok {
			if h, err := p.Health(ctx); err == nil {
				health = h
				if !jsonOut {
					fmt.Printf("Daemon:    %v (up %vs)\n", health["version"], health["uptime_seconds"])
				}
			}
		}
		targetState, _ := health["target_state"].(string)

		// …then aggregate queue status (running/pending/GPU).
		jobs, err := be.ListJobs(ctx, "")
		if err != nil {
			return err
		}

		var running, pending int
		for _, j := range jobs {
			running += j.Tasks.Running
			pending += j.Tasks.Pending
		}

		// GPU pool is daemon-local. When a target filter is active the
		// pool counts belong to whatever local targets the daemon manages
		// and would be misleading for a remote HPC target. Only show GPU
		// info when no explicit target filter is set (aggregate view).
		gpusFree := -1 // sentinel: omit from output
		if target == "" && targetState != backend.TargetStateUnconfigured {
			gpusFree = 0
			if gpus, gerr := be.GPUStatus(ctx); gerr == nil {
				for _, g := range gpus {
					if g.TaskID == "" {
						gpusFree++
					}
				}
			}
		}

		if jsonOut {
			out := map[string]any{
				"running": running,
				"pending": pending,
			}
			if targetState != "" {
				out["target_state"] = targetState
			}
			if gpusFree >= 0 {
				out["gpus_free"] = gpusFree
			}
			printJSON(out)
			return nil
		}

		if targetState == backend.TargetStateUnconfigured {
			fmt.Println("Target:    not configured — run `runq target add <name> ...`")
		}
		fmt.Printf("Running:   %d\n", running)
		fmt.Printf("Pending:   %d\n", pending)
		if gpusFree >= 0 {
			fmt.Printf("GPUs free: %d\n", gpusFree)
		}
		return nil
	})
}

func init() {
	statusCmd.Flags().Bool("json", false, "output raw JSON")
	statusCmd.Flags().StringP("target", "t", "", "Filter by compute target")

	statusCmd.GroupID = groupDiag
	rootCmd.AddCommand(statusCmd)
}
