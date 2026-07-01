package cli

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"os"
	"os/signal"
	"path/filepath"
	"slices"
	"strconv"
	"syscall"
	"time"

	"github.com/gliese129/runq/internal/backend"
	job2 "github.com/gliese129/runq/internal/job"
	"github.com/gliese129/runq/internal/project"
	"github.com/gliese129/runq/internal/utils"
	"github.com/gosuri/uilive"
	"github.com/olekukonko/tablewriter"
	"github.com/spf13/cobra"
	"gopkg.in/yaml.v3"
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
		var jobCfg job2.JobConfig
		fs, err := os.ReadFile(file)
		if err != nil {
			return err
		}
		if err := yaml.Unmarshal(fs, &jobCfg); err != nil {
			return err
		}
		if projectOverride != "" {
			jobCfg.Project = projectOverride
		}
		if noteOverride != "" {
			jobCfg.Note = noteOverride
		}

		return withBackend(cmd, func(be backend.Backend) error {
			ctx := cmd.Context()

			// HPC may need project registration from --project-file.
			if err := ensureProjectRegistered(cmd, be, jobCfg.Project); err != nil {
				return err
			}

			// ── dry-run ──
			if dryRun || dryRunAlias {
				return submitDryRun(ctx, be, jobCfg, noPreflight)
			}

			// ── submit ──
			jobID, n, err := be.SubmitJob(ctx, jobCfg, backend.SubmitOptions{
				SkipPreflight: noPreflight,
			})
			if err != nil {
				return err
			}
			if jsonOut {
				printJSON(struct {
					JobID      string `json:"job_id"`
					TotalTasks int    `json:"total_tasks"`
				}{JobID: jobID, TotalTasks: n})
				return nil
			}
			fmt.Printf("submitted job %s (%d tasks)\n", utils.IDColor(jobID), n)
			printGPUHint(ctx, be)

			if watch {
				return watchJob(ctx, be, jobID)
			}
			return nil
		})
	},
}

// ensureProjectRegistered registers a project from --project-file if provided.
// The project is registered in the DB so SubmitJob can find it by name.
func ensureProjectRegistered(cmd *cobra.Command, be backend.Backend, jobProject string) error {
	pf, _ := cmd.Flags().GetString("project-file")
	if pf == "" {
		return nil
	}
	buf, err := os.ReadFile(pf)
	if err != nil {
		return err
	}
	var cfg project.Config
	if err := yaml.Unmarshal(buf, &cfg); err != nil {
		return fmt.Errorf("parse %s: %w", pf, err)
	}
	if cfg.ProjectName == "" {
		cfg.ProjectName = jobProject
	}
	ctx := context.Background()
	if _, err := be.GetProject(ctx, cfg.ProjectName); err != nil {
		return be.CreateProject(ctx, cfg)
	}
	return be.UpdateProject(ctx, cfg)
}

// submitDryRun shows the task expansion table plus optional preview info.
// HPC backends render the full run.sh + submit command via PreviewSubmit;
// other backends get a parameter table + command preview from DryRun.
// The preview attempt is always made — the daemon routes to the correct
// target backend and returns ErrNotSupported for backends without a
// submit template.
func submitDryRun(ctx context.Context, be backend.Backend, jobCfg job2.JobConfig, noPreflight bool) error {
	// Try full submit preview (HPC targets render run.sh + submit command).
	out, err := be.PreviewSubmit(ctx, jobCfg, noPreflight)
	if err == nil {
		fmt.Println(out)
		return nil
	}
	if !errors.Is(err, backend.ErrNotSupported) {
		return err
	}

	// Fall back to common dry-run: task expansion + command preview.
	result, err := be.DryRun(ctx, jobCfg)
	if err != nil {
		return err
	}
	if len(result.Tasks) == 0 {
		fmt.Println("No tasks would be generated.")
		return nil
	}
	fmt.Printf("dry-run: %d task(s) would be submitted\n", len(result.Tasks))

	keys := slices.Sorted(maps.Keys(result.Tasks[0]))
	table := tablewriter.NewTable(os.Stdout)
	table.Header(keys)
	data := make([][]string, 0, len(result.Tasks))
	for _, task := range result.Tasks {
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

	if result.WorkspaceRoot != "" {
		fmt.Printf("\nworkspace root: %s/<note>-<job_id> (ids regenerate at submit)\n", result.WorkspaceRoot)
	}
	if result.SampleCommand != "" {
		fmt.Printf("\n── command (task 1 of %d) ──\n%s\n", len(result.Tasks), result.SampleCommand)
	}
	return nil
}

// watchJob polls job status and renders a live-updating task table.
func watchJob(ctx context.Context, be backend.Backend, jobID string) error {
	writer := uilive.New()
	writer.Start()
	defer writer.Stop()

	ticker := time.NewTicker(time.Second)
	sigChan := make(chan os.Signal, 1)
	signal.Notify(sigChan, syscall.SIGTERM, syscall.SIGINT)
	for {
		select {
		case <-sigChan:
			fmt.Println("Kill signal received!")
			return nil
		case <-ticker.C:
			detail, err := be.GetJob(ctx, jobID)
			if err != nil {
				return err
			}
			table := tablewriter.NewTable(writer)
			table.Header([]string{"ID", "STATUS", "GPUS", "RETRY", "DURATION"})
			data := make([][]string, 0, len(detail.Tasks))
			for _, task := range detail.Tasks {
				duration := "-"
				if task.ElapsedSec != nil {
					duration = (time.Duration(*task.ElapsedSec * float64(time.Second))).Round(time.Second).String()
				}
				data = append(data, []string{
					task.ID, task.Status, task.GPUs,
					strconv.Itoa(task.RetryCount), duration,
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
	submitCmd.Flags().StringP("target", "t", "", "Compute target to submit to (default: config default_target)")

	submitCmd.GroupID = groupCore
	rootCmd.AddCommand(submitCmd)
}
