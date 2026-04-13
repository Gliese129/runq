package cli

import (
	"fmt"

	"github.com/spf13/cobra"
)

// ── runq submit (shortcut for job submit) ──

var submitCmd = &cobra.Command{
	Use:   "submit [job.yaml | .]",
	Short: "Submit a job for scheduling",
	Long: `Submit a job from a YAML file. If "." is given, runq looks for
job.yaml in the current directory.`,
	Example: `  # Submit from file
  runq submit experiments/lr_sweep.yaml

  # Submit from current directory (looks for job.yaml)
  runq submit .

  # Preview expanded tasks without submitting
  runq submit job.yaml --dry-run

  # Submit and watch progress
  runq submit job.yaml --watch`,
	Args: cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		fmt.Println("TODO: submit")
		return nil
	},
}

// ── runq run (quick single task) ──

var runCmd = &cobra.Command{
	Use:   "run <project> [flags] -- [args...]",
	Short: "Run a single task without a YAML file",
	Example: `  # Run with default settings
  runq run resnet50 -- --lr 0.01 --batch_size 32

  # Run with 4 GPUs
  runq run resnet50 --gpus 4 -- --lr 0.01`,
	RunE: func(cmd *cobra.Command, args []string) error {
		fmt.Println("TODO: run")
		return nil
	},
}

// ── runq ps (shortcut for task ls) ──

var psCmd = &cobra.Command{
	Use:   "ps",
	Short: "List tasks (default: running + pending)",
	Example: `  runq ps                     # running + pending
  runq ps -a                  # include completed
  runq ps -l                  # detailed view
  runq ps --status failed     # filter by status
  runq ps --job <job_id>      # filter by job
  runq ps -o json             # JSON output`,
	RunE: func(cmd *cobra.Command, args []string) error {
		fmt.Println("TODO: ps")
		return nil
	},
}

// ── runq logs (shortcut for task logs) ──

var logsCmd = &cobra.Command{
	Use:   "logs <task_id>",
	Short: "Tail task output (default: follow mode)",
	Example: `  runq logs a3f9              # tail -f style
  runq logs a3f9 --no-follow  # print all and exit`,
	Args: cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		fmt.Println("TODO: logs")
		return nil
	},
}

// ── runq kill (shortcut for task kill) ──

var killCmd = &cobra.Command{
	Use:   "kill <task_id | job_id>",
	Short: "Kill a task or all tasks in a job",
	Args:  cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		fmt.Println("TODO: kill")
		return nil
	},
}

// ── runq gpu ──

var gpuCmd = &cobra.Command{
	Use:   "gpu",
	Short: "Show GPU allocation status",
	RunE: func(cmd *cobra.Command, args []string) error {
		fmt.Println("TODO: gpu")
		return nil
	},
}

// ── runq status ──

var statusCmd = &cobra.Command{
	Use:   "status",
	Short: "Show daemon status, queue length, and scheduling info",
	RunE: func(cmd *cobra.Command, args []string) error {
		fmt.Println("TODO: status")
		return nil
	},
}

func init() {
	// submit flags
	submitCmd.Flags().Bool("dry-run", false, "Expand sweep and print tasks without submitting")
	submitCmd.Flags().Bool("watch", false, "Block and show live progress after submit")
	submitCmd.Flags().String("project", "", "Override the project name in the YAML")

	// run flags
	runCmd.Flags().Int("gpus", 0, "Number of GPUs (overrides project default)")

	// ps flags
	psCmd.Flags().BoolP("all", "a", false, "Include completed tasks")
	psCmd.Flags().BoolP("long", "l", false, "Detailed output")
	psCmd.Flags().String("status", "", "Filter by status (running,pending,failed,success,killed)")
	psCmd.Flags().String("job", "", "Filter by job ID")
	psCmd.Flags().StringP("output", "o", "", "Output format (json)")
	psCmd.Flags().Bool("no-header", false, "Suppress table header")

	// logs flags
	logsCmd.Flags().Bool("no-follow", false, "Print log and exit (no tail -f)")

	rootCmd.AddCommand(submitCmd)
	rootCmd.AddCommand(runCmd)
	rootCmd.AddCommand(psCmd)
	rootCmd.AddCommand(logsCmd)
	rootCmd.AddCommand(killCmd)
	rootCmd.AddCommand(gpuCmd)
	rootCmd.AddCommand(statusCmd)
}
