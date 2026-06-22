package cli

import (
	"context"
	"fmt"
	"maps"
	"os"
	"os/signal"
	"path/filepath"
	"slices"
	"strconv"
	"syscall"
	"time"

	"github.com/gliese129/runq/internal/api"
	"github.com/gliese129/runq/internal/backend"
	"github.com/gliese129/runq/internal/config"
	job2 "github.com/gliese129/runq/internal/job"
	"github.com/gliese129/runq/internal/project"
	"github.com/gliese129/runq/internal/utils"
	"github.com/gosuri/uilive"
	"github.com/olekukonko/tablewriter"
	"github.com/spf13/cobra"
	"gopkg.in/yaml.v3"
)

// taskRowView mirrors the task DTO returned by /api/tasks.
type taskRowView struct {
	ID          string
	JobID       string
	ProjectName string
	Command     string
	ParamsJSON  string
	GPUsNeeded  int
	GPUs        string
	Status      string
	RetryCount  int
	MaxRetry    int
	PID         int
	StartTime   int64
	LogPath     string
	WorkingDir  string
	EnvJSON     string
	Resumable   bool
	ExtraArgs   string
	EnqueuedAt  time.Time
	StartedAt   *time.Time
	FinishedAt  *time.Time
}

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
  runq submit job.yaml --dry

  # Submit and watch progress
  runq submit job.yaml --watch`,
	Args: cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		file := args[0]
		if file == "" {
			return fmt.Errorf("no job file")
		}
		dryRun, _ := cmd.Flags().GetBool("dry")
		dryRunAlias, _ := cmd.Flags().GetBool("dry-run")
		watch, _ := cmd.Flags().GetBool("watch")
		projectOverride, _ := cmd.Flags().GetString("project")
		noteOverride, _ := cmd.Flags().GetString("note")
		noPreflight, _ := cmd.Flags().GetBool("no-preflight")
		jsonOut, _ := cmd.Flags().GetBool("json")

		if file == "." {
			file = "job.yaml"
		}
		if wd, err := os.Getwd(); err == nil && !filepath.IsAbs(file) {
			file = filepath.Join(wd, file)
		}
		var job job2.JobConfig
		fs, err := os.ReadFile(file)
		if err != nil {
			return err
		}
		if err := yaml.Unmarshal(fs, &job); err != nil {
			return err
		}
		if projectOverride != "" {
			job.Project = projectOverride
		}
		if noteOverride != "" {
			job.Note = noteOverride
		}
		// dry-run: param table + (parity with `runq hpc submit --dry-run`)
		if dryRun || dryRunAlias {
			// HPC mode gets the FULL preview (preflight + run.sh + submit
			// command) — same code path as `runq hpc submit --dry-run`.
			if _, mode, merr := loadModeConfig(); merr == nil && mode == config.ModeHPC {
				return previewHPCJobConfig(cmd, job, noPreflight)
			}
			tasks, err := job2.Expand(&job)
			if err != nil {
				return err
			}
			if len(tasks) == 0 {
				fmt.Println("No tasks would be generated.")
				return nil
			}
			fmt.Printf("dry-run: %d task(s) would be submitted\n", len(tasks))

			keys := slices.Sorted(maps.Keys(tasks[0]))
			table := tablewriter.NewTable(os.Stdout)
			table.Header(keys)
			data := make([][]string, 0, len(tasks))
			for _, task := range tasks {
				row := make([]string, 0, len(keys))
				for _, key := range keys {
					row = append(row, fmt.Sprintf("%v", task[key]))
				}
				data = append(data, row)
			}
			if err := table.Bulk(data); err != nil {
				return err
			}
			if err := table.Render(); err != nil {
				return err
			}

			// Best-effort preview details.
			var proj project.Config
			if perr := doAndDecode("GET", "/api/projects/"+job.Project, nil, &proj); perr == nil && proj.CmdTemplate != "" {
				storageCfg, _ := config.Load()
				root := config.ProspectiveRoot(storageCfg, proj.WorkingDir, proj.ProjectName)
				fmt.Printf("\nworkspace root: %s/<note>-<job_id> (ids regenerate at submit)\n", root)
				if cmdStr, rerr := job2.Render(proj.CmdTemplate, tasks[0]); rerr == nil {
					fmt.Printf("\n── command (task 1 of %d) ──\n%s\n", len(tasks), cmdStr)
				} else {
					fmt.Printf("\ncommand render error (would also fail at submit): %v\n", rerr)
				}
			}
			return nil
		}
		_, mode, err := loadModeConfig()
		if err != nil {
			return err
		}
		if mode == config.ModeHPC {
			if watch {
				return fmt.Errorf("--watch is not supported in hpc mode")
			}
			var jobID string
			var n int
			if serr := withHPCBackend(func(be backend.Backend) error {
				if err := ensureProjectRegistered(cmd, be, job.Project); err != nil {
					return err
				}
				var submitErr error
				jobID, n, submitErr = be.SubmitJob(context.Background(), job, backend.SubmitOptions{SkipPreflight: noPreflight})
				return submitErr
			}); serr != nil {
				return serr
			}
			resp := struct {
				JobID      string `json:"job_id"`
				TotalTasks int    `json:"total_tasks"`
			}{JobID: jobID, TotalTasks: n}
			if jsonOut {
				printJSON(resp)
				return nil
			}
			fmt.Printf("submitted job %s (%d tasks)\n", utils.IDColor(jobID), n)
			return nil
		}
		// submit
		type JobResp struct {
			JobId      string `json:"job_id"`
			TotalTasks int    `json:"total_tasks"`
			FreeGPUs   int    `json:"free_gpus"`
			TotalGPUs  int    `json:"total_gpus"`
		}
		var resp JobResp
		submitPath := "/api/jobs"
		if noPreflight {
			submitPath = "/api/jobs?no_preflight=1"
		}
		if err := doAndDecodeWithTimeout("POST", submitPath, job, &resp, api.SubmitClientTimeout); err != nil {
			return err
		}
		if jsonOut {
			printJSON(resp)
			return nil
		}
		fmt.Printf("Job submitted: id=%s tasks=%d\n", resp.JobId, resp.TotalTasks)
		if resp.TotalGPUs > 0 && resp.FreeGPUs == 0 {
			fmt.Printf("  queued: waiting for GPUs (0/%d free)\n", resp.TotalGPUs)
		} else if resp.TotalGPUs > 0 && resp.FreeGPUs < resp.TotalGPUs {
			fmt.Printf("  %d/%d GPUs free — some tasks may queue\n", resp.FreeGPUs, resp.TotalGPUs)
		}
		// watch
		if watch {
			writer := uilive.New()
			writer.Start()
			defer writer.Stop()

			ticker := time.NewTicker(time.Second * 1)
			sigChan := make(chan os.Signal, 1)
			signal.Notify(sigChan, syscall.SIGTERM, syscall.SIGINT)
			for {
				select {
				case <-sigChan:
					fmt.Println("Kill signal received!")
					return nil
				case <-ticker.C:
					var tasks []taskRowView
					query := fmt.Sprintf("/api/tasks?job=%s", resp.JobId)
					if err := doAndDecode("GET", query, nil, &tasks); err != nil {
						return err
					}

					table := tablewriter.NewTable(writer)
					table.Header([]string{
						"ID",
						"STATUS",
						"GPUS",
						"RETRY",
						"PID",
						"DURATION",
						"COMMAND",
					})
					data := make([][]string, 0, len(tasks))
					for _, task := range tasks {
						duration := "-"
						if task.StartedAt != nil {
							end := time.Now()
							if task.FinishedAt != nil {
								end = *task.FinishedAt
							}
							duration = end.Sub(*task.StartedAt).Round(time.Second).String()
						}
						data = append(data, []string{
							task.ID, string(task.Status), fmt.Sprintf("%v", task.GPUs),
							strconv.Itoa(task.RetryCount), strconv.Itoa(task.PID),
							duration, task.Command,
						})
					}

					if err := table.Bulk(data); err != nil {
						return err
					}
					if err := table.Render(); err != nil {
						return err
					}
				}
			}
		}
		return nil
	},
}

func init() {
	submitCmd.Flags().Bool("dry", false, "Expand sweep and print tasks without submitting")
	submitCmd.Flags().Bool("dry-run", false, "Expand sweep and print tasks without submitting")
	submitCmd.Flags().Bool("watch", false, "Block and show live progress after submit")
	submitCmd.Flags().String("project", "", "Override the project name in the YAML")
	submitCmd.Flags().StringP("note", "n", "", "Experiment note (overrides YAML note: field)")
	submitCmd.Flags().String("project-file", "", "load project config from a YAML file instead of the HPC registry")
	submitCmd.Flags().Bool("json", false, "output raw JSON")
	submitCmd.Flags().Bool(
		"no-preflight",
		false,
		"Skip submit-time checks (imports, pip check, path args, writability). "+
			"Use when the daemon misclassifies a runtime-only path or conditional import.",
	)

	submitCmd.GroupID = groupCore
	rootCmd.AddCommand(submitCmd)
}
