package cli

import (
	"context"
	"fmt"
	"os"
	"time"

	"github.com/gliese129/runq/internal/hpc"
	"github.com/gliese129/runq/internal/hpcconfig"
	"github.com/gliese129/runq/internal/job"
	"github.com/gliese129/runq/internal/project"
	"github.com/gliese129/runq/internal/store"
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
    runq hpc kill <job_id|task_id>    Cancel via your kill template`,
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

func init() {
	hpcInitCmd.Flags().String("scheduler", "", "preset to generate: slurm | pbs | sge (omit for generic)")
	hpcSubmitCmd.Flags().String("project-file", "", "load project config from a YAML file instead of the HPC registry")
	hpcSubmitCmd.Flags().Bool("no-preflight", false, "skip fail-before-submit checks (pip/import/path)")
	hpcStatusCmd.Flags().Bool("json", false, "output raw JSON")

	hpcCmd.AddCommand(hpcInitCmd, hpcSubmitCmd, hpcStatusCmd, hpcKillCmd)
	hpcCmd.GroupID = groupCore
	rootCmd.AddCommand(hpcCmd)
}

// openHPCStore ensures the data dir exists and opens the HPC store.
func openHPCStore() (*store.Store, error) {
	if err := os.MkdirAll(hpcconfig.DataDir(), 0o755); err != nil {
		return nil, fmt.Errorf("create data dir: %w", err)
	}
	return store.Open(hpcconfig.DBPath())
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

	cfg, err := hpcconfig.Load()
	if err != nil {
		return err
	}
	st, err := openHPCStore()
	if err != nil {
		return err
	}
	defer st.Close()

	proj, err := resolveHPCProject(cmd, st, jobCfg.Project)
	if err != nil {
		return err
	}

	skip, _ := cmd.Flags().GetBool("no-preflight")
	b := hpc.New(cfg, st)
	jobID, n, err := b.Submit(context.Background(), jobCfg, proj, hpc.SubmitOpts{SkipPreflight: skip})
	if err != nil {
		return err
	}
	fmt.Printf("submitted job %s (%d tasks)\n", utils.IDColor(jobID), n)
	return nil
}

// resolveHPCProject loads the project config from --project-file when given,
// otherwise from the HPC store's registry. When loaded from a file with no
// project_name, it inherits the job's project field so the FK lines up.
func resolveHPCProject(cmd *cobra.Command, st *store.Store, jobProject string) (*project.Config, error) {
	if pf, _ := cmd.Flags().GetString("project-file"); pf != "" {
		buf, err := os.ReadFile(pf)
		if err != nil {
			return nil, err
		}
		var cfg project.Config
		if err := yaml.Unmarshal(buf, &cfg); err != nil {
			return nil, fmt.Errorf("parse %s: %w", pf, err)
		}
		if cfg.ProjectName == "" {
			cfg.ProjectName = jobProject
		}
		return &cfg, nil
	}

	reg := project.NewRegistry(st.DB())
	p, err := reg.Get(jobProject)
	if err != nil {
		return nil, fmt.Errorf("project %q not in HPC store — pass --project-file or register it first: %w", jobProject, err)
	}
	return p, nil
}

func runHPCStatus(cmd *cobra.Command, args []string) error {
	cfg, err := hpcconfig.Load()
	if err != nil {
		return err
	}
	st, err := openHPCStore()
	if err != nil {
		return err
	}
	defer st.Close()

	view, err := hpc.New(cfg, st).Status(context.Background(), args[0])
	if err != nil {
		return err
	}

	if jsonOut, _ := cmd.Flags().GetBool("json"); jsonOut {
		printJSON(view)
		return nil
	}

	fmt.Printf("job %s  project=%s  status=%s  tasks=%d\n\n",
		utils.IDColor(view.Job.ID), view.Job.ProjectName,
		utils.StatusColor(view.Job.Status), view.Job.TotalTasks)

	w := newTable()
	fmt.Fprintf(w, "TASK_ID\tEXT_ID\tSTATUS\tAGE\n")
	for _, t := range view.Tasks {
		age := time.Since(t.EnqueuedAt).Truncate(time.Second)
		fmt.Fprintf(w, "%s\t%s\t%s\t%s\n",
			utils.IDColor(t.ID), t.ExternalID, utils.StatusColor(t.Status), age)
	}
	w.Flush()
	return nil
}

func runHPCKill(cmd *cobra.Command, args []string) error {
	cfg, err := hpcconfig.Load()
	if err != nil {
		return err
	}
	st, err := openHPCStore()
	if err != nil {
		return err
	}
	defer st.Close()

	n, err := hpc.New(cfg, st).Kill(context.Background(), args[0])
	if err != nil {
		return err
	}
	fmt.Printf("killed %d task(s)\n", n)
	return nil
}
