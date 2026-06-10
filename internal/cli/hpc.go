package cli

import (
	"context"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/gliese129/runq/internal/config"
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
    runq hpc kill <job_id|task_id>    Cancel via your kill template
    runq hpc rm <job_id>              Remove a completed job from DB
    runq hpc clean --older-than 7d    Delete old tasks, workspaces, and empty jobs`,
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

var hpcRmCmd = &cobra.Command{
	Use:   "rm <job_id>",
	Short: "Remove a completed job from the DB (does not delete files on disk)",
	Args:  cobra.ExactArgs(1),
	RunE:  runHPCRm,
}

var hpcCleanCmd = &cobra.Command{
	Use:   "clean",
	Short: "Remove finished tasks, their workspaces, and empty jobs older than a threshold",
	Long: `Remove finished tasks (success/failed/killed), their task directories and
log files, and jobs that have no remaining tasks.

Duration format: additive segments like 7d, 1m2w, 2w3d4h
  h = hours, d = days, w = weeks (7d), m = months (30d), y = years (365d)

Examples:
  runq hpc clean --older-than 7d        # older than 7 days
  runq hpc clean --older-than 1m        # older than 30 days
  runq hpc clean --older-than 7d --show # preview what would be deleted`,
	RunE: runHPCClean,
}

func init() {
	hpcInitCmd.Flags().String("scheduler", "", "preset to generate: slurm | pbs | sge (omit for generic)")
	hpcSubmitCmd.Flags().String("project-file", "", "load project config from a YAML file instead of the HPC registry")
	hpcSubmitCmd.Flags().StringP("note", "n", "", "Experiment note (overrides YAML note: field)")
	hpcSubmitCmd.Flags().Bool("no-preflight", false, "skip fail-before-submit checks (pip/import/path)")
	hpcStatusCmd.Flags().Bool("json", false, "output raw JSON")
	hpcLsCmd.Flags().Bool("json", false, "output raw JSON")
	for _, c := range []*cobra.Command{hpcBestCmd, hpcCollectCmd} {
		c.Flags().String("key", "", "metric key to rank by (e.g. loss, acc)")
		c.Flags().Bool("max", false, "rank by maximum instead of minimum")
		c.Flags().Bool("json", false, "output raw JSON")
	}
	hpcCleanCmd.Flags().String("older-than", "", "Age threshold (required), e.g. 7d, 1m2w, 2w3d4h")
	hpcCleanCmd.Flags().Bool("show", false, "Preview what would be deleted without actually deleting")
	hpcCleanCmd.MarkFlagRequired("older-than")

	hpcCmd.AddCommand(hpcInitCmd, hpcSubmitCmd, hpcStatusCmd, hpcKillCmd, hpcLsCmd, hpcBestCmd, hpcCollectCmd, hpcRmCmd, hpcCleanCmd)
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

// newHPCBackend loads both configs, opens the store, and returns a ready
// Backend. The caller must defer st.Close() on the returned store.
func newHPCBackend() (*hpc.Backend, *store.Store, error) {
	globalCfg, err := config.Load()
	if err != nil {
		return nil, nil, err
	}
	hpcCfg, err := hpcconfig.Load()
	if err != nil {
		return nil, nil, err
	}
	st, err := openHPCStore()
	if err != nil {
		return nil, nil, err
	}
	return hpc.New(hpcCfg, st, globalCfg), st, nil
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

	// Someone running `hpc init` almost certainly wants the unified CLI
	// (dashboard included) in HPC mode. Only flip the DEFAULT — an explicit
	// `mode: daemon` is user intent and stays. Either way say what happened
	// (side effects are explicit, philosophy C5).
	rawMode, explicit := config.RawMode()
	switch {
	case !explicit:
		if serr := config.SetKey("mode", config.ModeHPC); serr != nil {
			fmt.Printf("note: could not set mode=hpc: %v\n", serr)
		} else {
			fmt.Println("mode set to hpc (was unset) — `runq dashboard` and friends now use the HPC backend")
		}
	case rawMode == config.ModeHPC:
		// already hpc — nothing to announce
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
	jobID, n, err := submitHPCJobConfig(cmd, jobCfg, skip)
	if err != nil {
		return err
	}
	fmt.Printf("submitted job %s (%d tasks)\n", utils.IDColor(jobID), n)
	return nil
}

func submitHPCJobConfig(cmd *cobra.Command, jobCfg job.JobConfig, skipPreflight bool) (string, int, error) {
	b, st, err := newHPCBackend()
	if err != nil {
		return "", 0, err
	}
	defer st.Close()

	proj, err := resolveHPCProject(cmd, st, jobCfg.Project)
	if err != nil {
		return "", 0, err
	}
	return b.Submit(context.Background(), jobCfg, proj, hpc.SubmitOpts{SkipPreflight: skipPreflight})
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
	b, st, err := newHPCBackend()
	if err != nil {
		return err
	}
	defer st.Close()

	view, err := b.Status(context.Background(), args[0])
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
	fmt.Fprintf(w, "TASK_ID\tEXT_ID\tSTATUS\tSOURCE\tAGE\n")
	for _, t := range view.Tasks {
		age := time.Since(t.EnqueuedAt).Truncate(time.Second)
		fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\n",
			utils.IDColor(t.ID), t.ExternalID, utils.StatusColor(t.Status), t.StatusSource, age)
	}
	w.Flush()
	return nil
}

func runHPCKill(cmd *cobra.Command, args []string) error {
	b, st, err := newHPCBackend()
	if err != nil {
		return err
	}
	defer st.Close()

	n, err := b.Kill(context.Background(), args[0])
	if err != nil {
		return err
	}
	fmt.Printf("killed %d task(s)\n", n)
	return nil
}

// hpcJobItem is the stable JSON shape for `hpc ls` (also consumed by L2-D / AI).
type hpcJobItem struct {
	JobID      string `json:"job_id"`
	Project    string `json:"project"`
	Status     string `json:"status"`
	TotalTasks int    `json:"total_tasks"`
}

func runHPCLs(cmd *cobra.Command, args []string) error {
	st, err := openHPCStore()
	if err != nil {
		return err
	}
	defer st.Close()

	jobs, err := st.ListJobs(context.Background(), "")
	if err != nil {
		return err
	}
	items := make([]hpcJobItem, 0, len(jobs))
	for _, j := range jobs {
		items = append(items, hpcJobItem{JobID: j.ID, Project: j.ProjectName, Status: j.Status, TotalTasks: j.TotalTasks})
	}

	if jsonOut, _ := cmd.Flags().GetBool("json"); jsonOut {
		printJSON(items)
		return nil
	}
	if len(items) == 0 {
		fmt.Println("no jobs")
		return nil
	}
	w := newTable()
	fmt.Fprintf(w, "JOB_ID\tPROJECT\tSTATUS\tTASKS\n")
	for _, it := range items {
		fmt.Fprintf(w, "%s\t%s\t%s\t%d\n",
			utils.IDColor(it.JobID), it.Project, utils.StatusColor(it.Status), it.TotalTasks)
	}
	w.Flush()
	return nil
}

// hpcLeaderboard refreshes the job then returns its tasks ranked by --key. Shared
// by best/collect so they stay thin and consistent.
func hpcLeaderboard(cmd *cobra.Command, jobID string) ([]store.TaskScore, string, error) {
	key, _ := cmd.Flags().GetString("key")
	if key == "" {
		return nil, "", fmt.Errorf("--key is required (e.g. --key loss)")
	}
	maximize, _ := cmd.Flags().GetBool("max")

	b, st, err := newHPCBackend()
	if err != nil {
		return nil, "", err
	}
	defer st.Close()

	ctx := context.Background()
	if err := b.Refresh(ctx, jobID); err != nil {
		return nil, "", err
	}
	scores, err := st.MetricLeaderboard(ctx, jobID, key, maximize)
	return scores, key, err
}

func runHPCBest(cmd *cobra.Command, args []string) error {
	scores, key, err := hpcLeaderboard(cmd, args[0])
	if err != nil {
		return err
	}
	var top *store.TaskScore
	for i := range scores {
		if scores[i].HasValue {
			top = &scores[i]
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
	fmt.Printf("best %s = %v\n  task   %s (%s, source=%s)\n  params %v\n",
		key, top.Value, utils.IDColor(top.TaskID), utils.StatusColor(top.Status), top.Source, top.Params)
	return nil
}

func runHPCCollect(cmd *cobra.Command, args []string) error {
	scores, key, err := hpcLeaderboard(cmd, args[0])
	if err != nil {
		return err
	}

	if jsonOut, _ := cmd.Flags().GetBool("json"); jsonOut {
		printJSON(scores)
		return nil
	}
	if len(scores) == 0 {
		fmt.Println("no tasks")
		return nil
	}
	w := newTable()
	fmt.Fprintf(w, "TASK_ID\tSTATUS\tSOURCE\t%s\tPARAMS\n", strings.ToUpper(key))
	for _, s := range scores {
		val := "-"
		if s.HasValue {
			val = fmt.Sprintf("%v", s.Value)
		}
		fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%v\n",
			utils.IDColor(s.TaskID), utils.StatusColor(s.Status), s.Source, val, s.Params)
	}
	w.Flush()
	return nil
}

func runHPCRm(cmd *cobra.Command, args []string) error {
	st, err := openHPCStore()
	if err != nil {
		return err
	}
	defer st.Close()

	ctx := context.Background()
	j, err := st.GetJob(ctx, args[0])
	if err != nil {
		return err
	}
	if j == nil {
		return fmt.Errorf("job %q not found", args[0])
	}
	if j.Status != "done" {
		return fmt.Errorf("job %q is %s, only completed jobs can be removed (kill it first?)", args[0], j.Status)
	}
	if err := st.DeleteJob(ctx, args[0]); err != nil {
		return err
	}
	fmt.Printf("removed job %s (DB records only; files on disk are untouched)\n", utils.IDColor(args[0]))
	return nil
}

func runHPCClean(cmd *cobra.Command, args []string) error {
	olderThan, _ := cmd.Flags().GetString("older-than")
	showOnly, _ := cmd.Flags().GetBool("show")

	dur, err := utils.ParseHumanDuration(olderThan)
	if err != nil {
		return err
	}
	cutoff := time.Now().Add(-dur)

	st, err := openHPCStore()
	if err != nil {
		return err
	}
	defer st.Close()

	ctx := context.Background()
	tasks, err := st.ListFinishedTasksBefore(ctx, cutoff)
	if err != nil {
		return fmt.Errorf("query tasks: %w", err)
	}
	if len(tasks) == 0 {
		fmt.Println("Nothing to clean.")
		return nil
	}

	if showOnly {
		fmt.Printf("Would clean %d tasks (finished before %s):\n", len(tasks), cutoff.Format("2006-01-02 15:04"))
		for _, t := range tasks {
			finished := ""
			if t.FinishedAt != nil {
				finished = t.FinishedAt.Format("2006-01-02 15:04")
			}
			fmt.Printf("  %s  %-8s  finished=%s  task_dir=%s\n", t.ID, t.Status, finished, t.TaskDir)
		}
		return nil
	}

	var deletedTasks int
	var freedBytes int64
	for _, t := range tasks {
		freed := cleanTaskArtifacts(t)
		freedBytes += freed

		if err := st.DeleteTask(ctx, t.ID); err != nil {
			fmt.Fprintf(os.Stderr, "warning: failed to delete task %s: %v\n", t.ID, err)
			continue
		}
		deletedTasks++
	}

	deletedJobs, err := st.DeleteOrphanJobs(ctx)
	if err != nil {
		fmt.Fprintf(os.Stderr, "warning: failed to clean orphan jobs: %v\n", err)
	}

	fmt.Printf("Cleaned %d tasks, %d jobs", deletedTasks, deletedJobs)
	if freedBytes > 0 {
		fmt.Printf(", freed %s", formatBytes(freedBytes))
	}
	fmt.Println()
	return nil
}
