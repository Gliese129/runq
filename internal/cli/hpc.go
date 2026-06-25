package cli

import (
	"context"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/gliese129/runq/internal/backend"
	"github.com/gliese129/runq/internal/config"
	"github.com/gliese129/runq/internal/hpcconfig"
	"github.com/gliese129/runq/internal/job"
	"github.com/gliese129/runq/internal/project"
	"github.com/gliese129/runq/internal/utils"
	"github.com/spf13/cobra"
	"gopkg.in/yaml.v3"
)

// HPC commands operate directly on the per-user HPC store (~/.runq/runq.db).
// Unlike the daemon commands they never touch a socket — HPC has no resident
// process.
var hpcCmd = &cobra.Command{
	Use:   "hpc",
	Short: "Submit and track jobs on an HPC cluster (Slurm/PBS/SGE) — no daemon",
	Long: `runq hpc — run sweeps on a shared HPC cluster without the daemon.

  Files only; the cluster's scheduler does the dispatching. runq compiles your
  sweep, writes per-task workspaces, renders your submit template, and tracks
  status by reading status.json + metrics.jsonl back into a local DB (~/.runq).

  Setup:
    runq hpc init                     Write ~/.runq/config.yaml (edit for your cluster)
    runq hpc submit job.yaml          Compile + submit every task
    runq hpc status <job_id>          Refresh from disk and show progress
    runq hpc kill <job_id|task_id>    Cancel via your kill template
    runq hpc clean --older-than 7d    Remove tasks matching selectors`,
}

var hpcInitCmd = &cobra.Command{
	Use:   "init",
	Short: "Generate ~/.runq/config.yaml template",
	RunE:  runHPCInit,
}

var hpcSubmitCmd = &cobra.Command{
	Use:   "submit <job.yaml>",
	Short: "Compile a sweep and submit each task to the cluster",
	Args:  cobra.ExactArgs(1),
	RunE:  runHPCSubmit,
}

var hpcStatusCmd = &cobra.Command{
	Use:   "status <job_id>",
	Short: "Refresh from disk and show a job's tasks",
	Args:  cobra.ExactArgs(1),
	RunE:  runHPCStatus,
}

var hpcKillCmd = &cobra.Command{
	Use:   "kill <job_id|task_id>",
	Short: "Cancel a job or a single task",
	Args:  cobra.ExactArgs(1),
	RunE:  runHPCKill,
}

var hpcLsCmd = &cobra.Command{
	Use:     "ls",
	Aliases: []string{"list"},
	Short:   "List HPC jobs (DB state; run `hpc status <id>` to refresh one)",
	RunE:    runHPCLs,
}

var hpcBestCmd = &cobra.Command{
	Use:   "best <job_id> --key <metric>",
	Short: "Show the task with the best value of a metric",
	Args:  cobra.ExactArgs(1),
	RunE:  runHPCBest,
}

var hpcCollectCmd = &cobra.Command{
	Use:   "collect <job_id> --key <metric>",
	Short: "Per-task params + best metric value, ranked",
	Args:  cobra.ExactArgs(1),
	RunE:  runHPCCollect,
}

var hpcCleanCmd = &cobra.Command{
	Use:   "clean",
	Short: "Remove tasks and their artifacts based on selectors",
	Long: `Remove tasks matching the given selectors, their task directories and
log files, and jobs that have no remaining tasks.

Selectors (at least one required):
  --older-than <dur>   Tasks finished before this threshold
  --orphan             Tasks whose workspace directory is missing (DB-only)
  --archived           Tasks belonging to archived jobs
  --job <id>           All tasks in a specific job
  --task <id>          A specific task

Modifiers:
  --ckpt-only          Only delete checkpoints/ directory
  --show               Preview what would be deleted
  --yes                Skip confirmation prompt

Duration format: additive segments like 7d, 1m2w, 2w3d4h
  h = hours, d = days, w = weeks (7d), m = months (30d), y = years (365d)

Examples:
  runq hpc clean --older-than 7d        # older than 7 days
  runq hpc clean --orphan               # orphan tasks (no files)
  runq hpc clean --older-than 7d --show # preview what would be deleted`,
	RunE: runHPCClean,
}

func init() {
	hpcInitCmd.Flags().String("scheduler", "", "preset to generate: slurm | pbs | sge | tsubame | abci (omit for generic)")
	hpcSubmitCmd.Flags().String("project-file", "", "load project config from a YAML file instead of the HPC registry")
	hpcSubmitCmd.Flags().StringP("note", "n", "", "Experiment note (overrides YAML note: field)")
	hpcSubmitCmd.Flags().Bool("no-preflight", false, "skip fail-before-submit checks (pip/import/path)")
	hpcSubmitCmd.Flags().Bool("dry-run", false, "compile and render only: print preflight, submit command and run.sh — nothing written or queued")
	hpcStatusCmd.Flags().Bool("json", false, "output raw JSON")
	hpcLsCmd.Flags().Bool("json", false, "output raw JSON")
	for _, c := range []*cobra.Command{hpcBestCmd, hpcCollectCmd} {
		c.Flags().String("key", "", "metric key to rank by (e.g. loss, acc)")
		c.Flags().Bool("max", false, "rank by maximum instead of minimum")
		c.Flags().Bool("json", false, "output raw JSON")
	}
	hpcCleanCmd.Flags().String("older-than", "", "Age threshold, e.g. 7d, 1m2w, 2w3d4h")
	hpcCleanCmd.Flags().Bool("orphan", false, "Select orphan tasks (workspace directory missing)")
	hpcCleanCmd.Flags().Bool("archived", false, "Select tasks from archived jobs")
	hpcCleanCmd.Flags().String("job", "", "Select all tasks in a specific job")
	hpcCleanCmd.Flags().String("task", "", "Select a specific task")
	hpcCleanCmd.Flags().Bool("ckpt-only", false, "Only delete checkpoints/, keep other artifacts and DB records")
	hpcCleanCmd.Flags().Bool("show", false, "Preview what would be deleted without actually deleting")
	hpcCleanCmd.Flags().BoolP("yes", "y", false, "Skip confirmation prompt")

	hpcCmd.AddCommand(hpcInitCmd, hpcSubmitCmd, hpcStatusCmd, hpcKillCmd, hpcLsCmd, hpcBestCmd, hpcCollectCmd, hpcCleanCmd)
	hpcCmd.GroupID = groupCore
	rootCmd.AddCommand(hpcCmd)
}

// withHPCBackend opens an HPC backend via the factory and runs fn.
func withHPCBackend(fn func(backend.Backend) error) error {
	be, closer, err := backend.NewHPCBackendFromConfig()
	if err != nil {
		return err
	}
	defer closer()
	return fn(be)
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

func runHPCInit(cmd *cobra.Command, args []string) error {
	scheduler, _ := cmd.Flags().GetString("scheduler")
	path, created, err := hpcconfig.WriteTemplate(scheduler)
	if err != nil {
		return err
	}
	if created {
		fmt.Printf("wrote %s — edit it for your cluster, then `runq hpc submit job.yaml`\n", path)
	} else {
		fmt.Printf("%s already exists (left unchanged)\n", path)
	}

	rawMode, explicit := config.RawMode()
	switch {
	case !explicit:
		if serr := config.SetKey("mode", config.ModeHPC); serr != nil {
			fmt.Printf("note: could not set mode=hpc: %v\n", serr)
		} else {
			fmt.Println("mode set to hpc (was unset) — `runq dashboard` and friends now use the HPC backend")
		}
	case rawMode == config.ModeHPC:
		// already hpc
	default:
		fmt.Printf("note: mode is %q — run `runq config set mode=hpc` if this machine should use the HPC backend\n", rawMode)
	}
	return nil
}

func runHPCSubmit(cmd *cobra.Command, args []string) error {
	var jobCfg job.JobConfig
	buf, err := os.ReadFile(args[0])
	if err != nil {
		return err
	}
	if err := yaml.Unmarshal(buf, &jobCfg); err != nil {
		return fmt.Errorf("parse %s: %w", args[0], err)
	}
	if noteOverride, _ := cmd.Flags().GetString("note"); noteOverride != "" {
		jobCfg.Note = noteOverride
	}

	skip, _ := cmd.Flags().GetBool("no-preflight")

	if dry, _ := cmd.Flags().GetBool("dry-run"); dry {
		return withHPCBackend(func(be backend.Backend) error {
			if err := ensureProjectRegistered(cmd, be, jobCfg.Project); err != nil {
				return err
			}
			out, err := be.PreviewSubmit(context.Background(), jobCfg, skip)
			if err != nil {
				return err
			}
			fmt.Print(out)
			return nil
		})
	}

	return withHPCBackend(func(be backend.Backend) error {
		if err := ensureProjectRegistered(cmd, be, jobCfg.Project); err != nil {
			return err
		}
		jobID, n, err := be.SubmitJob(context.Background(), jobCfg, backend.SubmitOptions{SkipPreflight: skip})
		if err != nil {
			return err
		}
		fmt.Printf("submitted job %s (%d tasks)\n", utils.IDColor(jobID), n)
		return nil
	})
}

// previewHPCJobConfig is `runq submit --dry` in hpc mode.
func previewHPCJobConfig(cmd *cobra.Command, jobCfg job.JobConfig, skipPreflight bool) error {
	return withHPCBackend(func(be backend.Backend) error {
		if err := ensureProjectRegistered(cmd, be, jobCfg.Project); err != nil {
			return err
		}
		out, err := be.PreviewSubmit(context.Background(), jobCfg, skipPreflight)
		if err != nil {
			return err
		}
		fmt.Println(out)
		return nil
	})
}

func runHPCStatus(cmd *cobra.Command, args []string) error {
	return withHPCBackend(func(be backend.Backend) error {
		ctx := context.Background()
		if err := be.RefreshJob(ctx, args[0]); err != nil {
			return err
		}
		detail, err := be.GetJob(ctx, args[0])
		if err != nil {
			return err
		}

		if jsonOut, _ := cmd.Flags().GetBool("json"); jsonOut {
			printJSON(map[string]any{
				"capabilities": be.Capabilities(),
				"job":          detail.Job,
				"tasks":        detail.Tasks,
			})
			return nil
		}

		fmt.Printf("job %s  project=%s  status=%s  tasks=%d\n\n",
			utils.IDColor(detail.Job.ID), detail.Job.Project,
			utils.StatusColor(detail.Job.Status), detail.Job.Tasks.Total)

		w := newTable()
		fmt.Fprintf(w, "TASK_ID\tEXT_ID\tSTATUS\tSCHED_STATE\tQUEUE\tSOURCE\tAGE\n")
		for _, t := range detail.Tasks {
			age := ""
			if t.StartedAt != nil {
				age = time.Since(time.Unix(*t.StartedAt, 0)).Truncate(time.Second).String()
			}
			status := utils.StatusColor(t.Status)
			if t.OrphanAt != nil {
				status += " [orphan]"
			}
			fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\t%s\t%s\n",
				utils.IDColor(t.ID), t.ExternalID, status,
				t.NativeState, t.Queue, t.StatusSource, age)
		}
		w.Flush()
		return nil
	})
}

func runHPCKill(cmd *cobra.Command, args []string) error {
	return withHPCBackend(func(be backend.Backend) error {
		// Try as job first, then as task.
		if err := be.KillJob(context.Background(), args[0]); err != nil {
			if err2 := be.KillTask(context.Background(), args[0]); err2 != nil {
				return err // return original job error
			}
			fmt.Println("killed 1 task")
			return nil
		}
		fmt.Println("kill signal sent")
		return nil
	})
}

func runHPCLs(cmd *cobra.Command, args []string) error {
	return withHPCBackend(func(be backend.Backend) error {
		jobs, err := be.ListJobs(context.Background(), "")
		if err != nil {
			return err
		}

		if jsonOut, _ := cmd.Flags().GetBool("json"); jsonOut {
			printJSON(jobs)
			return nil
		}
		if len(jobs) == 0 {
			fmt.Println("no jobs")
			return nil
		}
		w := newTable()
		fmt.Fprintf(w, "JOB_ID\tPROJECT\tSTATUS\tTASKS\n")
		for _, j := range jobs {
			fmt.Fprintf(w, "%s\t%s\t%s\t%d\n",
				utils.IDColor(j.ID), j.Project, utils.StatusColor(j.Status), j.Tasks.Total)
		}
		w.Flush()
		return nil
	})
}

func runHPCBest(cmd *cobra.Command, args []string) error {
	key, _ := cmd.Flags().GetString("key")
	if key == "" {
		return fmt.Errorf("--key is required (e.g. --key loss)")
	}
	maximize, _ := cmd.Flags().GetBool("max")

	return withHPCBackend(func(be backend.Backend) error {
		ctx := context.Background()
		if err := be.RefreshJob(ctx, args[0]); err != nil {
			return err
		}
		rows, err := be.CompareMetrics(ctx, args[0], key, maximize)
		if err != nil {
			return err
		}

		var top *backend.CompareRow
		for i := range rows {
			if rows[i].HasValue {
				top = &rows[i]
				break
			}
		}

		if jsonOut, _ := cmd.Flags().GetBool("json"); jsonOut {
			printJSON(top)
			return nil
		}
		if top == nil {
			fmt.Printf("no task has metric %q yet\n", key)
			return nil
		}
		fmt.Printf("best %s = %v\n  task   %s (%s)\n  params %v\n",
			key, top.Best, utils.IDColor(top.TaskID), utils.StatusColor(top.Status), top.Params)
		return nil
	})
}

func runHPCCollect(cmd *cobra.Command, args []string) error {
	key, _ := cmd.Flags().GetString("key")
	if key == "" {
		return fmt.Errorf("--key is required (e.g. --key loss)")
	}
	maximize, _ := cmd.Flags().GetBool("max")

	return withHPCBackend(func(be backend.Backend) error {
		ctx := context.Background()
		if err := be.RefreshJob(ctx, args[0]); err != nil {
			return err
		}
		rows, err := be.CompareMetrics(ctx, args[0], key, maximize)
		if err != nil {
			return err
		}

		if jsonOut, _ := cmd.Flags().GetBool("json"); jsonOut {
			printJSON(rows)
			return nil
		}
		if len(rows) == 0 {
			fmt.Println("no tasks")
			return nil
		}
		w := newTable()
		fmt.Fprintf(w, "TASK_ID\tSTATUS\t%s\tPARAMS\n", strings.ToUpper(key))
		for _, r := range rows {
			val := "-"
			if r.HasValue {
				val = fmt.Sprintf("%v", r.Best)
			}
			fmt.Fprintf(w, "%s\t%s\t%s\t%v\n",
				utils.IDColor(r.TaskID), utils.StatusColor(r.Status), val, r.Params)
		}
		w.Flush()
		return nil
	})
}

func runHPCClean(cmd *cobra.Command, args []string) error {
	opts, err := buildCleanOptions(cmd)
	if err != nil {
		return err
	}

	return withHPCBackend(func(be backend.Backend) error {
		// Always preview first.
		previewOpts := opts
		previewOpts.DryRun = true
		result, err := be.Clean(cmd.Context(), previewOpts)
		if err != nil {
			return err
		}

		if len(result.Preview) == 0 {
			fmt.Println("Nothing to clean.")
			return nil
		}

		printCleanPreview(result.Preview)

		if opts.DryRun {
			return nil
		}

		yes, _ := cmd.Flags().GetBool("yes")
		if !yes {
			if !confirmClean(len(result.Preview)) {
				fmt.Println("Aborted.")
				return nil
			}
		}

		result, err = be.Clean(cmd.Context(), opts)
		if err != nil {
			return err
		}

		fmt.Printf("Cleaned %d tasks, %d jobs", result.Tasks, result.Jobs)
		if result.FreedBytes > 0 {
			fmt.Printf(", freed %s", humanBytes(result.FreedBytes))
		}
		fmt.Println()
		return nil
	})
}
