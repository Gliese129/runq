package cli

import (
	"fmt"
	"time"

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
	RunE:    runJobLs,
}

type jobSummary struct {
	JobID       string         `json:"job_id"`
	Project     string         `json:"project"`
	StatusCount map[string]int `json:"status_count"`
	CreatedAt   time.Time      `json:"created_at"`
}

func runJobLs(cmd *cobra.Command, args []string) error {
	var jobs []jobSummary
	if err := doAndDecode("GET", "/api/jobs", nil, &jobs); err != nil {
		return err
	}
	if len(jobs) == 0 {
		fmt.Println("no jobs")
		return nil
	}

	w := newTable()
	fmt.Fprintf(w, "JOB_ID\tPROJECT\tRUN\tPEND\tFAIL\tOK\tAGE\n")
	for _, j := range jobs {
		age := time.Since(j.CreatedAt).Truncate(time.Second)
		fmt.Fprintf(w, "%s\t%s\t%d\t%d\t%d\t%d\t%s\n",
			j.JobID, j.Project,
			j.StatusCount["running"],
			j.StatusCount["pending"],
			j.StatusCount["failed"],
			j.StatusCount["success"],
			age,
		)
	}
	w.Flush()
	return nil
}

var jobShowCmd = &cobra.Command{
	Use:   "show <job_id>",
	Short: "Show job details and its tasks",
	Args:  cobra.ExactArgs(1),
	// TODO: needs GET /api/jobs/:id endpoint (not yet implemented in API)
	RunE: func(cmd *cobra.Command, args []string) error {
		return fmt.Errorf("job show: not implemented (needs API endpoint)")
	},
}

var jobKillCmd = &cobra.Command{
	Use:   "kill <job_id>",
	Short: "Kill all tasks in a job",
	Args:  cobra.ExactArgs(1),
	RunE:  runJobKill,
}

func runJobKill(cmd *cobra.Command, args []string) error {
	jobID := args[0]
	var resp map[string]any
	if err := doAndDecode("DELETE", "/api/jobs/"+jobID, nil, &resp); err != nil {
		return err
	}
	fmt.Printf("job %s: %.0f tasks killed\n", jobID, resp["tasks_killed"])
	return nil
}

var jobPauseCmd = &cobra.Command{
	Use:   "pause <job_id>",
	Short: "Pause a job (stop dispatching new tasks, running tasks continue)",
	Args:  cobra.ExactArgs(1),
	// TODO: needs API endpoint
	RunE: func(cmd *cobra.Command, args []string) error {
		return fmt.Errorf("job pause: not implemented (needs API endpoint)")
	},
}

var jobResumeCmd = &cobra.Command{
	Use:   "resume <job_id>",
	Short: "Resume a paused job",
	Args:  cobra.ExactArgs(1),
	// TODO: needs API endpoint
	RunE: func(cmd *cobra.Command, args []string) error {
		return fmt.Errorf("job resume: not implemented (needs API endpoint)")
	},
}

var jobRmCmd = &cobra.Command{
	Use:     "rm <job_id>",
	Aliases: []string{"remove", "delete"},
	Short:   "Remove a completed job record",
	Args:    cobra.ExactArgs(1),
	// TODO: needs API endpoint
	RunE: func(cmd *cobra.Command, args []string) error {
		return fmt.Errorf("job rm: not implemented (needs API endpoint)")
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
