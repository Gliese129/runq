package cli

import (
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"

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
	sweepCmd.Flags().Bool("list", false, "Use list (zip) mode instead of grid")
	sweepCmd.Flags().Bool("dry", false, "Expand sweep and print tasks without submitting")
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
	listMode, _ := cmd.Flags().GetBool("list")
	dryRun, _ := cmd.Flags().GetBool("dry")

	// Default project name from current directory name.
	if projectName == "" {
		wd, err := os.Getwd()
		if err != nil {
			return fmt.Errorf("cannot detect project name: %w (use --project)", err)
		}
		projectName = filepath.Base(wd)
	}

	method := "grid"
	if listMode {
		method = "list"
	}

	block, err := parseSweepArgs(args, method)
	if err != nil {
		return err
	}

	jobCfg := job2.JobConfig{
		Project:     projectName,
		Description: description,
		Sweep:       []job2.SweepBlock{block},
	}

	// Dry-run: expand and print.
	if dryRun {
		tasks, err := job2.Expand(&jobCfg)
		if err != nil {
			return err
		}
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

	// Submit.
	type JobResp struct {
		JobId      string `json:"job_id"`
		TotalTasks int    `json:"total_tasks"`
		FreeGPUs   int    `json:"free_gpus"`
		TotalGPUs  int    `json:"total_gpus"`
	}
	var resp JobResp
	if err := doAndDecode("POST", "/api/jobs", jobCfg, &resp); err != nil {
		return err
	}
	fmt.Printf("Job submitted: id=%s tasks=%d (method=%s)\n", resp.JobId, resp.TotalTasks, method)
	if resp.TotalGPUs > 0 && resp.FreeGPUs == 0 {
		fmt.Printf("  queued: waiting for GPUs (0/%d free)\n", resp.TotalGPUs)
	} else if resp.TotalGPUs > 0 && resp.FreeGPUs < resp.TotalGPUs {
		fmt.Printf("  %d/%d GPUs free — some tasks may queue\n", resp.FreeGPUs, resp.TotalGPUs)
	}
	return nil
}
