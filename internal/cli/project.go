package cli

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"

	"github.com/gliese129/runq/internal/backend"
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

		return withBackend(func(be backend.Backend) error {
			if err := be.CreateProject(context.Background(), cfg); err != nil {
				return err
			}
			fmt.Printf("project %q registered\n", cfg.ProjectName)
			return nil
		})
	},
}

var projectLsCmd = &cobra.Command{
	Use:     "ls",
	Aliases: []string{"list"},
	Short:   "List all registered projects",
	RunE:    runProjectLs,
}

func runProjectLs(cmd *cobra.Command, args []string) error {
	return withBackend(func(be backend.Backend) error {
		ctx := context.Background()
		summaries, err := be.ListProjects(ctx)
		if err != nil {
			return err
		}
		if len(summaries) == 0 {
			fmt.Println("no projects registered")
			return nil
		}

		w := newTable()
		fmt.Fprintf(w, "NAME\tDIR\tGPUs/TASK\tRESUME\tJOBS\n")
		for _, s := range summaries {
			// Fetch full config for display fields not in ProjectSummary.
			gpus := 1
			resume := "off"
			if cfg, err := be.GetProject(ctx, s.Name); err == nil {
				if cfg.Defaults.GPUsPerTask > 0 {
					gpus = cfg.Defaults.GPUsPerTask
				}
				if cfg.Resume.Enabled {
					resume = "on"
				}
			}
			fmt.Fprintf(w, "%s\t%s\t%d\t%s\t%d\n", s.Name, s.WorkDir, gpus, resume, s.JobCount)
		}
		w.Flush()
		return nil
	})
}

var projectShowCmd = &cobra.Command{
	Use:   "show <name>",
	Short: "Show project details",
	Args:  cobra.ExactArgs(1),
	RunE:  runProjectShow,
}

func runProjectShow(cmd *cobra.Command, args []string) error {
	return withBackend(func(be backend.Backend) error {
		cfg, err := be.GetProject(context.Background(), args[0])
		if err != nil {
			return err
		}
		printJSON(cfg)
		return nil
	})
}

var projectEditCmd = &cobra.Command{
	Use:   "edit <name>",
	Short: "Edit project config in $EDITOR",
	Args:  cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		return withBackend(func(be backend.Backend) error {
			ctx := context.Background()
			name := args[0]
			cfg, err := be.GetProject(ctx, name)
			if err != nil {
				return err
			}
			temp, err := os.CreateTemp("", fmt.Sprintf("%s-*.yaml", name))
			if err != nil {
				return err
			}
			defer os.Remove(temp.Name())
			content, _ := yaml.Marshal(cfg)
			if err := os.WriteFile(temp.Name(), content, 0644); err != nil {
				return fmt.Errorf("failed to write temp file: %w", err)
			}

			editor := os.Getenv("EDITOR")
			if editor == "" {
				editor = "vim"
			}
			editorCmd := exec.Command(editor, temp.Name())
			editorCmd.Stdin = os.Stdin
			editorCmd.Stdout = os.Stdout
			editorCmd.Stderr = os.Stderr
			if err := editorCmd.Run(); err != nil {
				return err
			}
			content, err = os.ReadFile(temp.Name())
			if err != nil {
				return err
			}
			var updated project.Config
			if err := yaml.Unmarshal(content, &updated); err != nil {
				return err
			}
			if err := be.UpdateProject(ctx, updated); err != nil {
				return err
			}
			fmt.Printf("project %q updated\n", name)
			return nil
		})
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
	return withBackend(func(be backend.Backend) error {
		if err := be.DeleteProject(context.Background(), args[0]); err != nil {
			return err
		}
		fmt.Printf("project %q removed\n", args[0])
		return nil
	})
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
	projectCmd.AddCommand(projectArchiveCmd)
	projectCmd.AddCommand(projectUnarchiveCmd)
	projectCmd.GroupID = groupManagement
	rootCmd.AddCommand(projectCmd)
}

// ── archive / unarchive (mode-aware) ──

var projectArchiveCmd = &cobra.Command{
	Use:   "archive <name>",
	Short: "Hide a project (and its jobs in global lists) from default views; reversible",
	Args:  cobra.ExactArgs(1),
	RunE:  func(cmd *cobra.Command, args []string) error { return runProjectArchive(args[0], true) },
}

var projectUnarchiveCmd = &cobra.Command{
	Use:   "unarchive <name>",
	Short: "Bring an archived project back",
	Args:  cobra.ExactArgs(1),
	RunE:  func(cmd *cobra.Command, args []string) error { return runProjectArchive(args[0], false) },
}

func runProjectArchive(name string, archive bool) error {
	verb := "archived"
	if !archive {
		verb = "unarchived"
	}
	return withBackend(func(be backend.Backend) error {
		ctx := context.Background()
		var err error
		if archive {
			err = be.ArchiveProject(ctx, name)
		} else {
			err = be.UnarchiveProject(ctx, name)
		}
		if err != nil {
			return err
		}
		fmt.Printf("project %q %s\n", name, verb)
		return nil
	})
}
