package cli

import (
	"fmt"

	"github.com/spf13/cobra"
)

var jobCmd = &cobra.Command{
	Use:   "job",
	Short: "Manage jobs (sweep submissions)",
}

var jobLsCmd = &cobra.Command{
	Use:     "ls",
	Aliases: []string{"list"},
	Short:   "List all jobs",
	RunE: func(cmd *cobra.Command, args []string) error {
		fmt.Println("TODO: job ls")
		return nil
	},
}

var jobShowCmd = &cobra.Command{
	Use:   "show <job_id>",
	Short: "Show job details and its tasks",
	Args:  cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		fmt.Println("TODO: job show")
		return nil
	},
}

var jobKillCmd = &cobra.Command{
	Use:   "kill <job_id>",
	Short: "Kill all tasks in a job",
	Args:  cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		fmt.Println("TODO: job kill")
		return nil
	},
}

var jobPauseCmd = &cobra.Command{
	Use:   "pause <job_id>",
	Short: "Pause a job (stop dispatching new tasks, running tasks continue)",
	Args:  cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		fmt.Println("TODO: job pause")
		return nil
	},
}

var jobResumeCmd = &cobra.Command{
	Use:   "resume <job_id>",
	Short: "Resume a paused job",
	Args:  cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		fmt.Println("TODO: job resume")
		return nil
	},
}

var jobRmCmd = &cobra.Command{
	Use:     "rm <job_id>",
	Aliases: []string{"remove", "delete"},
	Short:   "Remove a completed job record",
	Args:    cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		fmt.Println("TODO: job rm")
		return nil
	},
}

func init() {
	jobCmd.AddCommand(jobLsCmd)
	jobCmd.AddCommand(jobShowCmd)
	jobCmd.AddCommand(jobKillCmd)
	jobCmd.AddCommand(jobPauseCmd)
	jobCmd.AddCommand(jobResumeCmd)
	jobCmd.AddCommand(jobRmCmd)
	rootCmd.AddCommand(jobCmd)
}
