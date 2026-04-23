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
	Status      string         `json:"status"`
	TotalTasks  int            `json:"total_tasks"`
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
	fmt.Fprintf(w, "JOB_ID\tPROJECT\tSTATUS\tRUN\tPEND\tFAIL\tOK\tAGE\n")
	for _, j := range jobs {
		age := time.Since(j.CreatedAt).Truncate(time.Second)
		fmt.Fprintf(w, "%s\t%s\t%s\t%d\t%d\t%d\t%d\t%s\n",
			j.JobID, j.Project, j.Status,
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
	RunE:  runJobShow,
}

func runJobShow(cmd *cobra.Command, args []string) error {
	jobID := args[0]
	var result map[string]any
	if err := doAndDecode("GET", "/api/jobs/"+jobID, nil, &result); err != nil {
		return err
	}
	printJSON(result)
	return nil
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
	RunE:  runJobPause,
}

func runJobPause(cmd *cobra.Command, args []string) error {
	jobID := args[0]
	var resp map[string]any
	if err := doAndDecode("POST", "/api/jobs/"+jobID+"/pause", nil, &resp); err != nil {
		return err
	}
	fmt.Printf("job %s paused\n", jobID)
	return nil
}

var jobResumeCmd = &cobra.Command{
	Use:   "resume <job_id>",
	Short: "Resume a paused job",
	Args:  cobra.ExactArgs(1),
	RunE:  runJobResume,
}

func runJobResume(cmd *cobra.Command, args []string) error {
	jobID := args[0]
	var resp map[string]any
	if err := doAndDecode("POST", "/api/jobs/"+jobID+"/resume", nil, &resp); err != nil {
		return err
	}
	fmt.Printf("job %s resumed\n", jobID)
	return nil
}

var jobRmCmd = &cobra.Command{
	Use:     "rm <job_id>",
	Aliases: []string{"remove", "delete"},
	Short:   "Remove a completed job record",
	Args:    cobra.ExactArgs(1),
	RunE:    runJobRm,
}

func runJobRm(cmd *cobra.Command, args []string) error {
	jobID := args[0]
	var resp map[string]any
	if err := doAndDecode("POST", "/api/jobs/"+jobID+"/rm", nil, &resp); err != nil {
		return err
	}
	fmt.Printf("job %s removed\n", jobID)
	return nil
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
