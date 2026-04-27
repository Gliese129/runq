package cli

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/gliese129/runq/internal/job"
	"github.com/spf13/cobra"
	"gopkg.in/yaml.v3"
)

var initCmd = &cobra.Command{
	Use:   "init [script.py]",
	Short: "Initialize project.yaml + job.yaml by scanning a Python script",
	Long: `Scan a Python training script for argparse arguments and generate
project.yaml and job.yaml templates.

If no script is specified, runq looks for train.py or main.py in the
current directory.

Examples:
  # Auto-detect script in current directory
  runq init

  # Specify script explicitly
  runq init train_v2.py

  # Custom project name and output directory
  runq init train.py --project resnet50 -o ./configs/`,
	Args: cobra.MaximumNArgs(1),
	RunE: runInit,
}

func init() {
	initCmd.Flags().StringP("output", "o", ".", "Output directory for generated YAML files")
	initCmd.Flags().String("project", "", "Project name (default: current directory name)")
}

// candidateScripts is the priority order for auto-detection.
var candidateScripts = []string{"train.py", "main.py"}

func runInit(cmd *cobra.Command, args []string) error {
	outDir, _ := cmd.Flags().GetString("output")
	projectName, _ := cmd.Flags().GetString("project")

	// Resolve script path.
	var scriptPath string
	if len(args) == 1 {
		scriptPath = args[0]
	} else {
		// Auto-detect: look for train.py, main.py in current directory.
		for _, name := range candidateScripts {
			if _, err := os.Stat(name); err == nil {
				scriptPath = name
				break
			}
		}
		if scriptPath == "" {
			return fmt.Errorf("no Python script found (tried %s). Specify one: runq init <script.py>",
				strings.Join(candidateScripts, ", "))
		}
	}

	// Make paths absolute.
	absScript, err := filepath.Abs(scriptPath)
	if err != nil {
		return fmt.Errorf("resolve script path: %w", err)
	}
	if _, err := os.Stat(absScript); err != nil {
		return fmt.Errorf("script not found: %s", absScript)
	}

	absOut, err := filepath.Abs(outDir)
	if err != nil {
		return fmt.Errorf("resolve output dir: %w", err)
	}

	// Default project name from directory name.
	if projectName == "" {
		projectName = filepath.Base(filepath.Dir(absScript))
	}

	// Scan argparse.
	fmt.Printf("Scanning %s ...\n", scriptPath)
	argInfos, err := job.ScanArgparse(absScript)
	if err != nil {
		return fmt.Errorf("scan argparse: %w", err)
	}
	if len(argInfos) == 0 {
		fmt.Println("  No argparse arguments found. Generating minimal templates.")
	} else {
		fmt.Printf("  Found %d arguments: %s\n", len(argInfos), summarizeArgs(argInfos))
	}

	// Generate project.yaml.
	projPath := filepath.Join(absOut, "project.yaml")
	if err := writeProjectYAML(projPath, projectName, filepath.Dir(absScript), scriptPath); err != nil {
		return err
	}
	fmt.Printf("  Created %s\n", projPath)

	// Generate job.yaml.
	jobPath := filepath.Join(absOut, "job.yaml")
	if err := writeJobYAML(jobPath, projectName, argInfos); err != nil {
		return err
	}
	fmt.Printf("  Created %s\n", jobPath)

	fmt.Println("\nNext steps:")
	fmt.Printf("  1. Edit %s — fill in sweep values\n", jobPath)
	fmt.Printf("  2. runq project add %s\n", absOut)
	fmt.Printf("  3. runq submit %s\n", jobPath)
	return nil
}

func summarizeArgs(args []job.ArgInfo) string {
	names := make([]string, 0, len(args))
	for _, a := range args {
		s := a.Name
		if a.Type != "" {
			s += "(" + a.Type + ")"
		}
		names = append(names, s)
	}
	return strings.Join(names, ", ")
}

// writeProjectYAML generates a project.yaml file.
func writeProjectYAML(path, projectName, workingDir, scriptFile string) error {
	// Check if file already exists.
	if _, err := os.Stat(path); err == nil {
		return fmt.Errorf("%s already exists (delete it first or use -o to write elsewhere)", path)
	}

	scriptBase := filepath.Base(scriptFile)
	proj := map[string]any{
		"project_name":     projectName,
		"working_dir":      workingDir,
		"command_template": fmt.Sprintf("python %s {{args}}", scriptBase),
		"defaults": map[string]any{
			"gpus_per_task": 1,
			"max_retry":     0,
		},
	}

	return writeYAML(path, proj, projectYAMLHeader)
}

// writeJobYAML generates a job.yaml with sweep parameters from scanned args.
func writeJobYAML(path, projectName string, argInfos []job.ArgInfo) error {
	if _, err := os.Stat(path); err == nil {
		return fmt.Errorf("%s already exists (delete it first or use -o to write elsewhere)", path)
	}

	// Build sweep parameters from ArgInfo.
	// Numeric types get example ranges, others get their default as single value.
	params := make(map[string]any, len(argInfos))
	for _, arg := range argInfos {
		params[arg.Name] = argInfoToSweepValue(arg)
	}

	jobCfg := map[string]any{
		"project":     projectName,
		"description": "auto-generated sweep — edit values below",
	}

	if len(params) > 0 {
		jobCfg["sweep"] = []map[string]any{
			{
				"method":     "grid",
				"parameters": params,
			},
		}
	}

	return writeYAML(path, jobCfg, jobYAMLHeader)
}

// argInfoToSweepValue generates example sweep values from ArgInfo.
func argInfoToSweepValue(arg job.ArgInfo) []any {
	// If we have a default, use it as a single-element list.
	// The user is expected to edit and expand these.
	if arg.Default != "" {
		return []any{arg.Default}
	}
	// No default — provide a placeholder based on type.
	switch arg.Type {
	case "float":
		return []any{0.001, 0.01}
	case "int":
		return []any{1, 10}
	case "bool":
		return []any{true, false}
	default:
		return []any{"FIXME"}
	}
}

func writeYAML(path string, data any, header string) error {
	out, err := yaml.Marshal(data)
	if err != nil {
		return fmt.Errorf("marshal YAML: %w", err)
	}
	content := header + string(out)
	return os.WriteFile(path, []byte(content), 0o644)
}

const projectYAMLHeader = `# runq project configuration — generated by runq init
# Edit as needed, then register: runq project add .
#
`

const jobYAMLHeader = `# runq job configuration — generated by runq init
# Edit sweep values below, then submit: runq submit job.yaml
#
# Tip: use runq submit job.yaml --dry to preview expanded tasks
#
`
