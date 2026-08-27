package cli

import (
	"context"
	"fmt"
	"maps"
	"os"
	"slices"
	"strings"

	"github.com/gliese129/runq/internal/api"
	"github.com/gliese129/runq/internal/backend"
	job2 "github.com/gliese129/runq/internal/job"
	"github.com/olekukonko/tablewriter"
	"github.com/spf13/cobra"
)

var sweepCmd = &cobra.Command{
	Use:   "sweep [flags] key=v1,v2 [key2=v3,v4 ...]",
	Short: "Quick parameter sweep from CLI (no YAML needed)",
	Long: `Submit a parameter sweep directly from the command line.
Each argument is a key=values pair where values are comma-separated.

By default all combinations are generated (grid / cartesian product).
Use --list to zip parameters 1-to-1 instead.

Examples:
  # Grid sweep: 2 × 3 = 6 tasks
  runq sweep --project resnet50 lr=1e-4,3e-4 batch=32,64,128

  # List sweep: 3 paired tasks
  runq sweep --project resnet50 --list lr=1e-4,3e-4,1e-3 batch=32,64,128

  # Preview without submitting
  runq sweep --project resnet50 --dry lr=1e-4,3e-4 batch=32,64`,
	Args: cobra.MinimumNArgs(1),
	RunE: runSweep,
}

func init() {
	sweepCmd.Flags().String("project", "", "Project name (default: current directory name)")
	sweepCmd.Flags().String("description", "", "Optional job description")
	sweepCmd.Flags().StringP("note", "n", "", "Experiment note")
	sweepCmd.Flags().Bool("list", false, "Use list (zip) mode instead of grid")
	sweepCmd.Flags().Bool("dry", false, "Expand sweep and print tasks without submitting")
	sweepCmd.Flags().StringP("target", "t", "", "Compute target to submit to (default: config default_target)")

	sweepCmd.GroupID = groupCore
	rootCmd.AddCommand(sweepCmd)
}

// parseSweepArgs parses "key=v1,v2" arguments into a SweepBlock.
func parseSweepArgs(args []string, method string) (job2.SweepBlock, error) {
	params := make(map[string]job2.ParameterSpec, len(args))
	for _, arg := range args {
		parts := strings.SplitN(arg, "=", 2)
		if len(parts) != 2 || parts[0] == "" || parts[1] == "" {
			return job2.SweepBlock{}, fmt.Errorf("invalid parameter %q: expected key=v1,v2", arg)
		}
		key := parts[0]
		if _, dup := params[key]; dup {
			return job2.SweepBlock{}, fmt.Errorf("duplicate parameter %q", key)
		}
		rawValues := strings.Split(parts[1], ",")
		values := make([]any, 0, len(rawValues))
		for _, v := range rawValues {
			values = append(values, inferType(strings.TrimSpace(v)))
		}
		params[key] = job2.ParameterSpec{Values: values}
	}
	return job2.SweepBlock{Method: method, Parameters: params}, nil
}

// inferType tries to parse a string value as int, float, or bool.
// Falls back to string if none match.
func inferType(s string) any {
	// Try bool.
	if s == "true" {
		return true
	}
	if s == "false" {
		return false
	}
	// Try int (only pure digits, no dots or 'e').
	if !strings.Contains(s, ".") && !strings.ContainsAny(s, "eE") {
		var i int
		if _, err := fmt.Sscanf(s, "%d", &i); err == nil {
			// Verify full consumption (no trailing chars).
			if fmt.Sprintf("%d", i) == s {
				return i
			}
		}
	}
	// Try float.
	var f float64
	if _, err := fmt.Sscanf(s, "%g", &f); err == nil {
		return f
	}
	return s
}

func runSweep(cmd *cobra.Command, args []string) error {
	projectName, _ := cmd.Flags().GetString("project")
	description, _ := cmd.Flags().GetString("description")
	note, _ := cmd.Flags().GetString("note")
	listMode, _ := cmd.Flags().GetBool("list")
	dryRun, _ := cmd.Flags().GetBool("dry")
	method := "grid"
	if listMode {
		method = "list"
	}

	block, err := parseSweepArgs(args, method)
	if err != nil {
		return err
	}

	// --dry → POST /jobs/plan（spec §7.2, D12）：与 submit 同源的展开 + note
	// 解析，daemon 权威。daemon 不可达时降级为纯本地展开（plan 本就是廉价
	// 本地操作，降级只少 note 解析）。
	if dryRun {
		jobCfg := job2.JobConfig{
			Project:     projectName,
			Description: description,
			Note:        note,
			Sweep:       []job2.SweepBlock{block},
		}
		err := withBackend(cmd, func(be backend.Backend) error {
			p, ok := be.(*api.Proxy)
			if !ok {
				return backend.ErrNotSupported
			}
			tasks, noteResolved, err := p.PlanJob(cmd.Context(), jobCfg)
			if err != nil {
				return err
			}
			if noteResolved != "" {
				fmt.Printf("Note: %s\n", noteResolved)
			}
			return printTaskTable(tasks, method)
		})
		if err != nil {
			fmt.Fprintln(os.Stderr, "daemon unavailable, falling back to local expansion")
			return printSweepPreview(jobCfg, method)
		}
		return nil
	}

	// Submit requires a backend for project detection and job submission.
	return withBackend(cmd, func(be backend.Backend) error {
		ctx := cmd.Context()

		// Auto-detect project from current directory.
		if projectName == "" {
			wd, err := os.Getwd()
			if err != nil {
				return fmt.Errorf("cannot detect project: %w (use --project)", err)
			}
			detected, err := detectProject(ctx, be, wd, "runq sweep --project <name>")
			if err != nil {
				return err
			}
			projectName = detected
		}

		jobCfg := job2.JobConfig{
			Project:     projectName,
			Description: description,
			Note:        note,
			Sweep:       []job2.SweepBlock{block},
		}

		jobID, totalTasks, err := be.SubmitJob(ctx, jobCfg, backend.SubmitOptions{})
		if err != nil {
			return err
		}
		fmt.Printf("Job submitted: id=%s tasks=%d (method=%s)\n", jobID, totalTasks, method)
		printGPUHint(ctx, be)
		return nil
	})
}

// printSweepPreview expands sweep parameters and prints the task table.
func printSweepPreview(jobCfg job2.JobConfig, method string) error {
	tasks, err := job2.Expand(&jobCfg)
	if err != nil {
		return err
	}
	return printTaskTable(tasks, method)
}

// printTaskTable renders expanded task params — shared by the plan path
// (daemon-authoritative) and the local fallback.
func printTaskTable(tasks []job2.TaskParams, method string) error {
	if len(tasks) == 0 {
		fmt.Println("No tasks generated.")
		return nil
	}
	fmt.Printf("Method: %s, %d tasks:\n", method, len(tasks))
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
	return table.Render()
}

// printGPUHint prints a best-effort GPU utilization message after job submit.
// Silent on error or when GPU info is unavailable (e.g. HPC mode).
func printGPUHint(ctx context.Context, be backend.Backend) {
	gpus, err := be.GPUStatus(ctx)
	if err != nil || len(gpus) == 0 {
		return
	}
	total := len(gpus)
	free := 0
	for _, g := range gpus {
		if g.TaskID == "" {
			free++
		}
	}
	if free == 0 {
		fmt.Printf("  queued: waiting for GPUs (0/%d free)\n", total)
	} else if free < total {
		fmt.Printf("  %d/%d GPUs free — some tasks may queue\n", free, total)
	}
}

// detectProject uses the backend to find projects registered for a directory.
// Returns the project name if exactly one match, or a descriptive error
// listing candidates (with copyable commands) if zero or multiple.
func detectProject(ctx context.Context, be backend.Backend, dir string, usagePrefix string) (string, error) {
	matches, err := be.MatchProjects(ctx, dir)
	if err != nil {
		return "", fmt.Errorf("cannot detect project for %s: %w\n  use --project to specify", dir, err)
	}

	switch len(matches) {
	case 0:
		return "", fmt.Errorf("no project registered for directory %s\n\n  Register one first:\n    runq project add . --dir %s\n\n  Or specify explicitly:\n    %s", dir, dir, usagePrefix)
	case 1:
		return matches[0].Name, nil
	default:
		var sb strings.Builder
		fmt.Fprintf(&sb, "multiple projects found for %s:\n\n", dir)
		for _, m := range matches {
			fmt.Fprintf(&sb, "  %s  --project %s\n", usagePrefix, m.Name)
		}
		fmt.Fprintf(&sb, "\nSpecify one with --project")
		return "", fmt.Errorf("%s", sb.String())
	}
}
