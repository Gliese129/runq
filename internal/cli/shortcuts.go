package cli

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"maps"
	"os"
	"os/signal"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/fsnotify/fsnotify"
	"github.com/gliese129/runq/internal/api"
	"github.com/gliese129/runq/internal/config"
	job2 "github.com/gliese129/runq/internal/job"
	"github.com/gliese129/runq/internal/utils"
	"github.com/gosuri/uilive"
	"github.com/olekukonko/tablewriter"
	"github.com/spf13/cobra"
	"gopkg.in/yaml.v3"
)

// taskRowView mirrors the task DTO returned by /api/tasks.
// It intentionally keeps StartTime as int64 because the DB stores the
// /proc start tick separately from wall-clock timestamps.
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
		// dry-run
		if dryRun || dryRunAlias {
			tasks, err := job2.Expand(&job)
			if err != nil {
				return err
			}
			if len(tasks) == 0 {
				fmt.Println("No sweep job")
			} else {
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
				err := table.Bulk(data)
				if err != nil {
					return err
				}
				return table.Render()
			}
		}
		_, mode, err := loadModeConfig()
		if err != nil {
			return err
		}
		if mode == config.ModeHPC {
			if watch {
				return fmt.Errorf("--watch is not supported in hpc mode")
			}
			jobID, n, err := submitHPCJobConfig(cmd, job, noPreflight)
			if err != nil {
				return err
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
			// F8 preflight is daemon-side; the CLI flag tunnels through
			// as a query string. Daemon's POST /api/jobs handler reads
			// it and forwards into SubmitJobOpts.SkipPreflight.
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
		// env: users should use shell `export` directly; env isolation belongs in project.yaml if needed later.
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

		type JobResp struct {
			JobId      string `json:"job_id"`
			TotalTasks int    `json:"total_tasks"`
		}
		var resp JobResp
		if err := doAndDecodeWithTimeout("POST", "/api/jobs", jobCfg, &resp, api.SubmitClientTimeout); err != nil {
			return err
		}
		fmt.Printf("Job submitted: id=%s tasks=%d\n", resp.JobId, resp.TotalTasks)
		return nil
	},
}

// ── runq ps / ls (unified job list) ──

var psCmd = &cobra.Command{
	Use:     "ps",
	Aliases: []string{"ls"},
	Short:   "List jobs in the configured backend",
	Example: `  runq ps
  runq ls
  runq ps --json`,
	RunE: runPs,
}

func runPs(cmd *cobra.Command, args []string) error {
	output, _ := cmd.Flags().GetString("output")
	jsonOut, _ := cmd.Flags().GetBool("json")
	_, mode, err := loadModeConfig()
	if err != nil {
		return err
	}
	backend, closeBackend, err := newDashboardBackend(mode)
	if err != nil {
		return err
	}
	defer closeBackend()
	jobs, err := backend.ListJobs(context.Background())
	if err != nil {
		return err
	}
	if output == "json" || jsonOut {
		printJSON(jobs)
		return nil
	}
	return printDashboardJobs(jobs)
}

// ── runq logs (shortcut for task logs) ──

var logsCmd = &cobra.Command{
	Use:   "logs <task_id>",
	Short: "Tail task output (default: follow mode)",
	Example: `  runq logs a3f9              # tail -f style
  runq logs a3f9 --no-follow  # print all and exit`,
	Args: cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		id := args[0]
		var task taskRowView
		if err := doAndDecode("GET", fmt.Sprintf("/api/tasks/%s", id), nil, &task); err != nil {
			return err
		}

		// Print a colored header before tailing.
		fmt.Printf("%s  %s  %s\n",
			utils.IDColor(task.ID),
			utils.StatusColor(task.Status),
			utils.Dimf("%s", task.LogPath))

		logfile := task.LogPath
		noFollow, _ := cmd.Flags().GetBool("no-follow")
		if noFollow {
			data, err := os.ReadFile(logfile)
			if err != nil {
				return err
			}
			fmt.Print(string(data))
			return nil
		}

		writer := uilive.New()
		writer.Start()
		defer writer.Stop()

		file, err := os.Open(logfile)
		if err != nil {
			return err
		}
		if _, err := file.Seek(0, io.SeekEnd); err != nil {
			return err
		}

		watcher, err := fsnotify.NewWatcher()
		if err != nil {
			return err
		}
		defer watcher.Close()
		watcher.Add(logfile)

		reader := bufio.NewReader(file)
		for {
			select {
			case event, ok := <-watcher.Events:
				if !ok {
					return nil
				}
				if event.Op&fsnotify.Write == fsnotify.Write {
					for {
						line, err := reader.ReadString('\n')
						if err == io.EOF {
							break
						}
						if err != nil {
							return err
						}
						fmt.Fprintf(writer.Bypass(), "%s", line)
					}
				}
			case err, ok := <-watcher.Errors:
				if !ok {
					return nil
				}
				return err
			}
		}
	},
}

// ── runq kill (shortcut for task kill) ──

var killCmd = &cobra.Command{
	Use:   "kill <task_id | job_id>",
	Short: "Kill a task or all tasks in a job",
	Args:  cobra.ExactArgs(1),
	RunE:  runKill,
}

func runKill(cmd *cobra.Command, args []string) error {
	id := args[0]
	_, mode, err := loadModeConfig()
	if err != nil {
		return err
	}
	backend, closeBackend, err := newDashboardBackend(mode)
	if err != nil {
		return err
	}
	defer closeBackend()
	jsonOut, _ := cmd.Flags().GetBool("json")

	// Try task kill first.
	err = backend.KillTask(context.Background(), id)
	if err == nil {
		if jsonOut {
			printJSON(map[string]bool{"ok": true})
			return nil
		}
		fmt.Printf("task %s killed\n", id)
		return nil
	}

	// If not found as task, try as job.
	err = backend.KillJob(context.Background(), id)
	if err == nil {
		if jsonOut {
			printJSON(map[string]bool{"ok": true})
			return nil
		}
		fmt.Printf("job %s killed\n", id)
		return nil
	}

	return fmt.Errorf("no task or job found with id %q", id)
}

// ── runq gpu ──

var gpuCmd = &cobra.Command{
	Use:   "gpu",
	Short: "Show GPU allocation status",
	RunE:  runGPU,
}

func runGPU(cmd *cobra.Command, args []string) error {
	_, mode, err := loadModeConfig()
	if err != nil {
		return err
	}
	backend, closeBackend, err := newDashboardBackend(mode)
	if err != nil {
		return err
	}
	defer closeBackend()

	gpus, err := backend.GPUStatus(context.Background())
	if err != nil {
		return err
	}

	w := newTable()
	fmt.Fprintf(w, "GPU\tNAME\tMEM\tUTIL\tTASK\n")
	for _, g := range gpus {
		task := "-"
		if g.TaskID != "" {
			task = g.TaskID
		}
		mem := fmt.Sprintf("%d/%d MB", g.MemUsedMB, g.MemTotalMB)
		fmt.Fprintf(w, "%d\t%s\t%s\t%d%%\t%s\n", g.Index, g.Name, mem, g.UtilPercent, task)
	}
	w.Flush()
	return nil
}

// ── runq status ──

var statusCmd = &cobra.Command{
	Use:   "status [job_id]",
	Short: "Show daemon status, queue length, and scheduling info",
	Args:  cobra.MaximumNArgs(1),
	RunE:  runStatus,
}

func runStatus(cmd *cobra.Command, args []string) error {
	jsonOut, _ := cmd.Flags().GetBool("json")
	if len(args) == 1 {
		_, mode, err := loadModeConfig()
		if err != nil {
			return err
		}
		backend, closeBackend, err := newDashboardBackend(mode)
		if err != nil {
			return err
		}
		defer closeBackend()
		detail, err := backend.GetJob(context.Background(), args[0])
		if err != nil {
			return err
		}
		if jsonOut {
			printJSON(detail)
			return nil
		}
		return printDashboardDetail(detail)
	}

	_, mode, err := loadModeConfig()
	if err != nil {
		return err
	}
	if mode == config.ModeHPC {
		return fmt.Errorf("status requires <job_id> in hpc mode")
	}
	var s map[string]any
	if err := doAndDecode("GET", "/api/status", nil, &s); err != nil {
		return err
	}
	if jsonOut {
		printJSON(s)
		return nil
	}

	fmt.Printf("Running:   %.0f\n", s["running"])
	fmt.Printf("Pending:   %.0f\n", s["pending"])
	fmt.Printf("GPUs free: %.0f\n", s["gpus_free"])
	return nil
}

func init() {
	// submit flags
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

	// run flags
	runCmd.Flags().Int("gpus", 0, "Number of GPUs (overrides project default)")
	runCmd.Flags().Int("max-retry", 1, "max try count for a task, default 1")

	// ps flags
	psCmd.Flags().StringP("output", "o", "", "Output format (json)")
	psCmd.Flags().Bool("json", false, "output raw JSON")

	// logs flags
	logsCmd.Flags().Bool("no-follow", false, "Print log and exit (no tail -f)")
	killCmd.Flags().Bool("json", false, "output raw JSON")
	statusCmd.Flags().Bool("json", false, "output raw JSON")

	// Core commands.
	submitCmd.GroupID = groupCore
	sweepCmd.GroupID = groupCore
	runCmd.GroupID = groupCore
	psCmd.GroupID = groupCore
	logsCmd.GroupID = groupCore
	killCmd.GroupID = groupCore

	// Diagnostics.
	gpuCmd.GroupID = groupDiag
	statusCmd.GroupID = groupDiag
	doctorCmd.GroupID = groupDiag
	cleanCmd.GroupID = groupDiag
	initCmd.GroupID = groupDiag

	rootCmd.AddCommand(submitCmd)
	rootCmd.AddCommand(runCmd)
	rootCmd.AddCommand(psCmd)
	rootCmd.AddCommand(logsCmd)
	rootCmd.AddCommand(killCmd)
	rootCmd.AddCommand(gpuCmd)
	rootCmd.AddCommand(statusCmd)
	rootCmd.AddCommand(doctorCmd)
	rootCmd.AddCommand(cleanCmd)
	rootCmd.AddCommand(sweepCmd)
	rootCmd.AddCommand(initCmd)
}
