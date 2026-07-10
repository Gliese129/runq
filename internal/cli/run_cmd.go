package cli

import (
	"fmt"
	"os"
	"strings"

	"github.com/gliese129/runq/internal/backend"
	job2 "github.com/gliese129/runq/internal/job"
	"github.com/gliese129/runq/internal/utils"
	"github.com/spf13/cobra"
)

// ── runq run (quick single task) ──

var runCmd = &cobra.Command{
	Use:   "run <project> [flags] -- [args...]",
	Short: "Run a single task without a YAML file",
	Example: `  # Run with default settings
  runq run resnet50 -- --lr 0.01 --batch_size 32

  # Run with 4 GPUs
  runq run resnet50 --gpus 4 -- --lr 0.01`,
	Args: cobra.MinimumNArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		project := args[0]
		gpuPerTask, _ := cmd.Flags().GetInt("gpus")
		maxRetry, _ := cmd.Flags().GetInt("max-retry")
		sweep := job2.SweepBlock{
			Method:     "list",
			Parameters: map[string]job2.ParameterSpec{},
		}
		if len(args) > 1 {
			params := make(map[string]job2.ParameterSpec)
			passThroughArgs := args[1:]
			for i := 0; i < len(passThroughArgs); i += 2 {
				if strings.HasPrefix(passThroughArgs[i], "--") && i < len(passThroughArgs)-1 {
					params[passThroughArgs[i][2:]] = job2.ParameterSpec{
						Values: []any{passThroughArgs[i+1]},
					}
				} else {
					fmt.Fprintf(os.Stderr, "warning: invalid param %q ignored\n", passThroughArgs[i])
				}
			}
			sweep.Parameters = params
		}

		jobCfg := job2.JobConfig{
			Project: project,
			Sweep:   []job2.SweepBlock{sweep},
			Overrides: &job2.Overrides{
				GPUsPerTask: &gpuPerTask,
				MaxRetry:    &maxRetry,
			},
		}

		return withBackend(cmd, func(be backend.Backend) error {
			jobID, n, err := be.SubmitJob(cmd.Context(), jobCfg, backend.SubmitOptions{})
			if err != nil {
				return err
			}
			fmt.Printf("Job submitted: id=%s tasks=%d\n", utils.IDColor(jobID), n)
			return nil
		})
	},
}

func init() {
	runCmd.Flags().Int("gpus", 0, "Number of GPUs (overrides project default)")
	runCmd.Flags().Int("max-retry", 1, "max try count for a task, default 1")
	runCmd.Flags().StringP("target", "t", "", "Compute target to submit to (default: config default_target)")

	runCmd.GroupID = groupCore
	rootCmd.AddCommand(runCmd)
}
