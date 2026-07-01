package cli

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"

	"github.com/gliese129/runq/internal/config"
	"github.com/spf13/cobra"
	"gopkg.in/yaml.v3"
)

var configCmd = &cobra.Command{
	Use:   "config",
	Short: "Get or set global runq configuration",
}

var configSetCmd = &cobra.Command{
	Use:   "set <key>=<value>",
	Short: "Set a global config key",
	Args:  cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		key, value, ok := strings.Cut(args[0], "=")
		if !ok || strings.TrimSpace(key) == "" {
			return fmt.Errorf("expected <key>=<value>")
		}
		if err := config.SetKey(key, value); err != nil {
			return err
		}
		got, err := config.GetKey(strings.TrimSpace(key))
		if err != nil {
			return err
		}
		fmt.Fprintf(cmd.OutOrStdout(), "%s=%s\n", strings.TrimSpace(key), got)
		return nil
	},
}

var configGetCmd = &cobra.Command{
	Use:   "get <key>",
	Short: "Get a global config key",
	Args:  cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		value, err := config.GetKey(args[0])
		if err != nil {
			return err
		}
		fmt.Fprintln(cmd.OutOrStdout(), value)
		return nil
	},
}

var configListCmd = &cobra.Command{
	Use:   "list",
	Short: "List global config keys",
	Args:  cobra.NoArgs,
	RunE: func(cmd *cobra.Command, args []string) error {
		values, err := config.ListKeys()
		if err != nil {
			return err
		}
		keys := config.Keys()
		sort.Strings(keys)
		for _, key := range keys {
			fmt.Fprintf(cmd.OutOrStdout(), "%s=%s\n", key, values[key])
		}
		return nil
	},
}

// ── Target management subcommands ──────────────────────────────────────────

var configAddCmd = &cobra.Command{
	Use:   "add <name>",
	Short: "Add a new compute target to config.yaml",
	Long: `Add a new compute target. Template presets fill in scheduler templates
and signal maps for common HPC schedulers.

Examples:
  runq config add tsubame --template=slurm --host=login.tsubame.titech.ac.jp --user=alice
  runq config add local-a100 --gpus=0,1,2,3
  runq config add abci --template=abci --host=es.abci.local`,
	Args: cobra.ExactArgs(1),
	RunE: runConfigAdd,
}

var configShowCmd = &cobra.Command{
	Use:   "show [<name>]",
	Short: "Show target configuration (all targets or a specific one)",
	Args:  cobra.MaximumNArgs(1),
	RunE:  runConfigShow,
}

var configEditCmd = &cobra.Command{
	Use:   "edit [<name>]",
	Short: "Open config.yaml in $EDITOR, then validate",
	Args:  cobra.MaximumNArgs(1),
	RunE:  runConfigEdit,
}

var configCheckCmd = &cobra.Command{
	Use:   "check [<name>]",
	Short: "Validate target templates: placeholders, regex, sample render",
	Args:  cobra.MaximumNArgs(1),
	RunE:  runConfigCheck,
}

var configSetTargetCmd = &cobra.Command{
	Use:   "set-target <name>",
	Short: "Set the active compute target for this session (or persistently with -p)",
	Long: `Set the active compute target. Without -p, writes to ~/.runq/.active-target
(session-scoped, overridden by $RUNQ_TARGET or --target flag). With -p,
writes to config.yaml default_target (persists across sessions).`,
	Example: `  runq config set-target tsubame       # session-scoped
  runq config set-target tsubame -p    # persistent default`,
	Args: cobra.ExactArgs(1),
	RunE: runConfigSetTarget,
}

func runConfigAdd(cmd *cobra.Command, args []string) error {
	name := args[0]
	template, _ := cmd.Flags().GetString("template")
	host, _ := cmd.Flags().GetString("host")
	user, _ := cmd.Flags().GetString("user")
	gpus, _ := cmd.Flags().GetString("gpus")

	cfg, err := config.Load()
	if err != nil {
		return err
	}

	// Check for duplicate.
	for _, t := range cfg.Targets {
		if t.Name == name {
			return fmt.Errorf("target %q already exists — use `runq config edit %s` to modify", name, name)
		}
	}

	tc := config.TargetConfig{Name: name}

	// Apply template preset if given.
	if template != "" {
		tc.Scheduler = template
		if preset, err := config.HPCPreset(template); err == nil {
			tc.SubmitTemplate = preset.SubmitTemplate
			tc.SubmitIDRegex = preset.SubmitIDRegex
			tc.StatusTemplate = preset.StatusTemplate
			tc.StatusParser = preset.StatusParser
			tc.StatusListTemplate = preset.StatusListTemplate
			tc.StatusListParser = preset.StatusListParser
			tc.SignalMap = preset.SignalMap
			tc.KillTemplate = preset.KillTemplate
		}
	}

	// SSH config.
	if host != "" {
		tc.SSH = &config.SSHTargetConfig{Host: host, User: user}
	}

	// GPU list for local targets.
	if gpus != "" {
		var gpuList []int
		for _, s := range strings.Split(gpus, ",") {
			s = strings.TrimSpace(s)
			var idx int
			if _, err := fmt.Sscanf(s, "%d", &idx); err != nil {
				return fmt.Errorf("invalid GPU index %q", s)
			}
			gpuList = append(gpuList, idx)
		}
		tc.GPUs = gpuList
	}

	cfg.Targets = append(cfg.Targets, tc)
	if err := config.SaveGlobal(cfg); err != nil {
		return err
	}
	fmt.Fprintf(cmd.OutOrStdout(), "added target %q\n", name)

	// If this is the first target and no default is set, make it the default.
	if cfg.DefaultTarget == "" && len(cfg.Targets) == 1 {
		if err := config.SetKey("default_target", name); err == nil {
			fmt.Fprintf(cmd.OutOrStdout(), "set as default_target\n")
		}
	}
	return nil
}

func runConfigShow(cmd *cobra.Command, args []string) error {
	cfg, err := config.Load()
	if err != nil {
		return err
	}

	if len(args) == 1 {
		// Show specific target.
		tc, err := cfg.FindTarget(args[0])
		if err != nil {
			return err
		}
		out, err := yaml.Marshal(tc)
		if err != nil {
			return err
		}
		fmt.Fprintf(cmd.OutOrStdout(), "# target: %s\n%s", tc.Name, out)
		return nil
	}

	// Show all targets.
	targets := cfg.ResolveTargets()
	if len(targets) == 0 {
		fmt.Fprintln(cmd.OutOrStdout(), "no targets configured")
		return nil
	}
	defaultTarget := cfg.ResolveDefaultTarget()
	for _, t := range targets {
		marker := " "
		if t.Name == defaultTarget {
			marker = "*"
		}
		typeName := t.Type()
		remote := ""
		if t.IsRemote() {
			remote = fmt.Sprintf(" (%s@%s)", t.SSH.User, t.SSH.Host)
		}
		fmt.Fprintf(cmd.OutOrStdout(), " %s %-16s  %s%s\n", marker, t.Name, typeName, remote)
	}
	return nil
}

func runConfigEdit(cmd *cobra.Command, args []string) error {
	path := config.ConfigPath()
	if _, err := os.Stat(path); os.IsNotExist(err) {
		return fmt.Errorf("no config at %s — run `runq config add <name>` first", path)
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

	// Re-validate after editing.
	return runConfigCheck(cmd, args)
}

func runConfigCheck(cmd *cobra.Command, args []string) error {
	cfg, err := config.Load()
	if err != nil {
		return err
	}

	targets := cfg.ResolveTargets()
	if len(args) == 1 {
		// Check specific target only.
		tc, err := cfg.FindTarget(args[0])
		if err != nil {
			return err
		}
		targets = []config.TargetConfig{*tc}
	}

	failed := 0
	for _, tc := range targets {
		if tc.Scheduler == "" {
			continue // local targets have nothing to check
		}
		fmt.Fprintf(cmd.OutOrStdout(), "── %s ──\n", tc.Name)
		for _, r := range tc.CheckHPC() {
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
			fmt.Fprintf(cmd.OutOrStdout(), "%s %-18s %s\n", mark, r.Name, r.Detail)
		}
	}
	if failed > 0 {
		return fmt.Errorf("%d template check(s) failed", failed)
	}
	return nil
}

func runConfigSetTarget(cmd *cobra.Command, args []string) error {
	name := args[0]
	persist, _ := cmd.Flags().GetBool("persist")

	// Validate target exists.
	cfg, err := config.Load()
	if err != nil {
		return err
	}
	found := false
	for _, t := range cfg.ResolveTargets() {
		if t.Name == name {
			found = true
			break
		}
	}
	if !found {
		return fmt.Errorf("target %q not found in config — see `runq config show`", name)
	}

	if persist {
		if err := config.SetKey("default_target", name); err != nil {
			return err
		}
		fmt.Fprintf(cmd.OutOrStdout(), "default_target=%s (persistent)\n", name)
		return nil
	}

	// Session-scoped: write to .active-target file.
	path := filepath.Join(config.ConfigDir(), ".active-target")
	if err := os.MkdirAll(config.ConfigDir(), 0o755); err != nil {
		return err
	}
	if err := os.WriteFile(path, []byte(name+"\n"), 0o644); err != nil {
		return err
	}
	fmt.Fprintf(cmd.OutOrStdout(), "active target: %s (session-scoped)\n", name)
	return nil
}

func init() {
	configAddCmd.Flags().String("template", "", "HPC scheduler preset: slurm | pbs | sge | tsubame | abci")
	configAddCmd.Flags().String("host", "", "SSH host for remote targets")
	configAddCmd.Flags().String("user", "", "SSH user for remote targets")
	configAddCmd.Flags().String("gpus", "", "Comma-separated GPU indices for local targets (e.g. 0,1,2,3)")
	configSetTargetCmd.Flags().BoolP("persist", "p", false, "Write to config default_target instead of session file")

	configCmd.AddCommand(configSetCmd, configGetCmd, configListCmd)
	configCmd.AddCommand(configAddCmd, configShowCmd, configEditCmd, configCheckCmd, configSetTargetCmd)
	configCmd.GroupID = groupManagement
	rootCmd.AddCommand(configCmd)
}
