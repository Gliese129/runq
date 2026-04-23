package cli

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"

	"github.com/gliese129/runq/internal/project"
	"github.com/spf13/cobra"
	"gopkg.in/yaml.v3"
)

var projectCmd = &cobra.Command{
	Use:   "project",
	Short: "Manage projects (experiment types)",
}

var projectAddCmd = &cobra.Command{
	Use:   "add [name | .]",
	Short: "Register a new project",
	Long: `Register a new project from CLI flags or from a project.yaml file.

When "." is given as the argument, runq looks for project.yaml in the
current directory. The project_name field in YAML is used unless a name
is also provided on the command line (CLI takes priority).`,
	Example: `  # From a YAML file in current directory
  runq project add .

  # From a YAML file with CLI name override
  runq project add myproject --file ./project.yaml

  # Inline (minimal)
  runq project add myproject --dir . --cmd "python train.py {{args}}"`,
	RunE: func(cmd *cobra.Command, args []string) error {
		cfg := project.Config{}
		var yamlPath string
		var projectName string

		if len(args) > 0 {
			if args[0] == "." {
				wd, err := os.Getwd()
				if err != nil {
					return err
				}
				yamlPath = filepath.Join(wd, "project.yaml")
			} else {
				projectName = args[0]
			}
		}

		// --file flag overrides "." resolution (but not explicit project name)
		if file, _ := cmd.Flags().GetString("file"); file != "" && yamlPath == "" {
			yamlPath = file
		}

		if yamlPath != "" {
			buf, err := os.ReadFile(yamlPath)
			if err != nil {
				return err
			}
			if err := yaml.Unmarshal(buf, &cfg); err != nil {
				return err
			}
		}

		// CLI flags override YAML fields
		if projectName != "" {
			cfg.ProjectName = projectName
		}
		if dir, _ := cmd.Flags().GetString("dir"); dir != "" {
			cfg.WorkingDir = dir
		}
		if cmdTpl, _ := cmd.Flags().GetString("cmd"); cmdTpl != "" {
			cfg.CmdTemplate = cmdTpl
		}

		type Resp struct {
			Message string `json:"message"`
		}
		var resp Resp
		if err := doAndDecode("POST", "/api/projects", cfg, &resp); err != nil {
			return err
		}
		fmt.Println(resp.Message)
		return nil
	},
}

var projectLsCmd = &cobra.Command{
	Use:     "ls",
	Aliases: []string{"list"},
	Short:   "List all registered projects",
	RunE:    runProjectLs,
}

func runProjectLs(cmd *cobra.Command, args []string) error {
	var configs []project.Config
	if err := doAndDecode("GET", "/api/projects", nil, &configs); err != nil {
		return err
	}
	if len(configs) == 0 {
		fmt.Println("no projects registered")
		return nil
	}

	w := newTable()
	fmt.Fprintf(w, "NAME\tDIR\tGPUs/TASK\tRESUME\n")
	for _, c := range configs {
		gpus := c.Defaults.GPUsPerTask
		if gpus == 0 {
			gpus = 1
		}
		resume := "off"
		if c.Resume.Enabled {
			resume = "on"
		}
		fmt.Fprintf(w, "%s\t%s\t%d\t%s\n", c.ProjectName, c.WorkingDir, gpus, resume)
	}
	w.Flush()
	return nil
}

var projectShowCmd = &cobra.Command{
	Use:   "show <name>",
	Short: "Show project details",
	Args:  cobra.ExactArgs(1),
	RunE:  runProjectShow,
}

func runProjectShow(cmd *cobra.Command, args []string) error {
	name := args[0]
	var cfg project.Config
	if err := doAndDecode("GET", "/api/projects/"+name, nil, &cfg); err != nil {
		return err
	}
	printJSON(cfg)
	return nil
}

var projectEditCmd = &cobra.Command{
	Use:   "edit <name>",
	Short: "Edit project config in $EDITOR",
	Args:  cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		name := args[0]
		query := fmt.Sprintf("/api/projects/%s", name)
		var cfg project.Config
		if err := doAndDecode("GET", query, nil, &cfg); err != nil {
			return err
		}
		temp, err := os.CreateTemp("", fmt.Sprintf("%s-*.yaml", name))
		if err != nil {
			return err
		}
		defer os.Remove(temp.Name())
		content, _ := yaml.Marshal(cfg) // Marshal on a plain struct won't fail
		if err := os.WriteFile(temp.Name(), content, 0644); err != nil {
			return fmt.Errorf("failed to write temp file: %w", err)
		}

		editor := os.Getenv("EDITOR")
		if editor == "" {
			editor = "vim" // i prefer vim
		}
		cmd_ := exec.Command(editor, temp.Name())
		cmd_.Stdin = os.Stdin
		cmd_.Stdout = os.Stdout
		cmd_.Stderr = os.Stderr
		err = cmd_.Run()
		if err != nil {
			return err
		}
		content, err = os.ReadFile(temp.Name())
		if err != nil {
			return err
		}
		if err := yaml.Unmarshal(content, &cfg); err != nil {
			return err
		}
		type Resp struct {
			Message string `json:"message"`
		}
		var resp Resp
		if err := doAndDecode("PUT", query, cfg, &resp); err != nil {
			return err
		}
		fmt.Println(resp.Message)
		return nil
	},
}

var projectRmCmd = &cobra.Command{
	Use:     "rm <name>",
	Aliases: []string{"remove", "delete"},
	Short:   "Remove a project",
	Args:    cobra.ExactArgs(1),
	RunE:    runProjectRm,
}

func runProjectRm(cmd *cobra.Command, args []string) error {
	name := args[0]
	var resp map[string]string
	if err := doAndDecode("DELETE", "/api/projects/"+name, nil, &resp); err != nil {
		return err
	}
	fmt.Println(resp["message"])
	return nil
}

func init() {
	// Flags for project add
	projectAddCmd.Flags().StringP("file", "f", "", "Path to project.yaml")
	projectAddCmd.Flags().String("dir", "", "Working directory")
	projectAddCmd.Flags().String("cmd", "", "Command template")

	projectCmd.AddCommand(projectAddCmd)
	projectCmd.AddCommand(projectLsCmd)
	projectCmd.AddCommand(projectShowCmd)
	projectCmd.AddCommand(projectEditCmd)
	projectCmd.AddCommand(projectRmCmd)
	rootCmd.AddCommand(projectCmd)
}
