package cli

import (
	"fmt"

	"github.com/spf13/cobra"
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
		fmt.Println("TODO: project add")
		return nil
	},
}

var projectLsCmd = &cobra.Command{
	Use:     "ls",
	Aliases: []string{"list"},
	Short:   "List all registered projects",
	RunE: func(cmd *cobra.Command, args []string) error {
		fmt.Println("TODO: project ls")
		return nil
	},
}

var projectShowCmd = &cobra.Command{
	Use:   "show <name>",
	Short: "Show project details",
	Args:  cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		fmt.Println("TODO: project show")
		return nil
	},
}

var projectEditCmd = &cobra.Command{
	Use:   "edit <name>",
	Short: "Edit project config in $EDITOR",
	Args:  cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		fmt.Println("TODO: project edit")
		return nil
	},
}

var projectRmCmd = &cobra.Command{
	Use:     "rm <name>",
	Aliases: []string{"remove", "delete"},
	Short:   "Remove a project",
	Args:    cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		fmt.Println("TODO: project rm")
		return nil
	},
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
