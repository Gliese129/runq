package cli

import (
	"fmt"
	"os"
	"os/exec"

	"github.com/gliese129/runq/internal/config"
	"github.com/spf13/cobra"
	"gopkg.in/yaml.v3"
)

// runq hpc config — the missing entry point for the hpc: section.
// CLI persona: terse, assumes the user knows the placeholder vocabulary;
// `check` exists so a typo is caught locally in milliseconds instead of by
// a failed submission on the cluster.

var hpcConfigCmd = &cobra.Command{
	Use:   "config",
	Short: "Show the hpc: section of config.yaml",
	Long: `runq hpc config — view, edit, and validate the HPC cluster templates.

  runq hpc config          Print the resolved hpc: section and its file path
  runq hpc config edit     Open config.yaml in $EDITOR, then re-validate
  runq hpc config check    Render every template with sample values (zero cost)`,
	RunE: runHPCConfigShow,
}

var hpcConfigEditCmd = &cobra.Command{
	Use:   "edit",
	Short: "Open config.yaml in $EDITOR, then validate the hpc: section",
	RunE:  runHPCConfigEdit,
}

var hpcConfigCheckCmd = &cobra.Command{
	Use:   "check",
	Short: "Validate templates: placeholders, regex, sample render preview",
	RunE:  runHPCConfigCheck,
}

func init() {
	hpcConfigCmd.AddCommand(hpcConfigEditCmd)
	hpcConfigCmd.AddCommand(hpcConfigCheckCmd)
	hpcCmd.AddCommand(hpcConfigCmd)
}

func runHPCConfigShow(cmd *cobra.Command, args []string) error {
	cfg, err := config.LoadHPCConfig()
	if err != nil {
		return err
	}
	out, err := yaml.Marshal(map[string]*config.TargetConfig{"hpc": cfg})
	if err != nil {
		return err
	}
	cmd.Printf("# %s\n%s", config.ConfigPath(), out)
	return nil
}

func runHPCConfigEdit(cmd *cobra.Command, args []string) error {
	path := config.ConfigPath()
	if _, err := os.Stat(path); os.IsNotExist(err) {
		return fmt.Errorf("no config at %s — run `runq hpc init` first", path)
	}

	editor := os.Getenv("VISUAL")
	if editor == "" {
		editor = os.Getenv("EDITOR")
	}
	if editor == "" {
		editor = "vi"
	}
	ed := exec.Command(editor, path)
	ed.Stdin, ed.Stdout, ed.Stderr = os.Stdin, os.Stdout, os.Stderr
	if err := ed.Run(); err != nil {
		return fmt.Errorf("%s exited with error: %w", editor, err)
	}

	// Immediate feedback: re-validate what was just saved.
	return runHPCConfigCheck(cmd, nil)
}

func runHPCConfigCheck(cmd *cobra.Command, args []string) error {
	cfg, err := config.LoadHPCConfig()
	if err != nil {
		return err
	}
	failed := 0
	for _, r := range cfg.CheckHPC() {
		var mark string
		switch r.Status {
		case "ok":
			mark = "✓"
		case "fail":
			mark = "✗"
			failed++
		default:
			mark = "-"
		}
		cmd.Printf("%s %-18s %s\n", mark, r.Name, r.Detail)
	}
	if failed > 0 {
		return fmt.Errorf("%d template check(s) failed", failed)
	}
	return nil
}
