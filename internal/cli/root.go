package cli

import (
	"fmt"
	"os"
	"strings"
	"text/template"

	"github.com/gliese129/runq-lab/internal/utils"
	"github.com/spf13/cobra"
)

// ANSI helpers for help template (only active when stdout is a terminal).
var helpFuncs = template.FuncMap{
	"bold":      func(s string) string { return utils.Bold(s) },
	"underline": func(s string) string { return utils.Underline(s) },
	"dim":       func(s string) string { return utils.Dimf("%s", s) },
	"commandsWithGroup": func(cmd *cobra.Command, groupID string) []*cobra.Command {
		var commands []*cobra.Command
		for _, child := range cmd.Commands() {
			if child.GroupID == groupID && !child.Hidden && child.IsAvailableCommand() {
				commands = append(commands, child)
			}
		}
		return commands
	},
}

// Command group IDs.
const (
	groupCore       = "core"
	groupManagement = "mgmt"
	groupDiag       = "diag"
)

var rootCmd = &cobra.Command{
	Use:   "runq",
	Short: "A lightweight GPU job scheduler for research labs",
	Long: `runq — a GPU job scheduler for research labs

  Get started in 3 steps:

    1. runq init                   Scan your train.py and generate configs
    2. runq project add .          Register the project
    3. runq submit job.yaml        Submit and go

  Or skip YAML entirely:

    runq sweep --project myproj lr=1e-4,3e-4 batch=32,64`,
	// main() prints the returned error; without this cobra prints it too
	// and every failure shows up twice.
	SilenceErrors: true,
	// Business errors (preflight failed, target unreachable) are not usage
	// errors — burying them under 30 lines of flags helps nobody (RQ-65
	// #6). Cobra checks Root().SilenceUsage, so this covers every command;
	// genuine usage errors (unknown flag) get a --help hint via the
	// FlagErrorFunc below instead of the full usage dump.
	SilenceUsage: true,
}

func init() {
	rootCmd.PersistentFlags().String("socket", "", "path to daemon unix socket")
	// --fresh（spec §7 全局 flag）：绕过 daemon 缓存 TTL 强制刷新（受服务端
	// 5min 下限保护）；未真刷时提示「x 秒前已刷新」而非静默 no-op（D22）。
	rootCmd.PersistentFlags().Bool("fresh", false, "force-refresh cached target data before the command (rate-limited server-side)")

	// Register command groups.
	rootCmd.AddGroup(
		&cobra.Group{ID: groupCore, Title: "Core Commands:"},
		&cobra.Group{ID: groupManagement, Title: "Management:"},
		&cobra.Group{ID: groupDiag, Title: "Setup & Diagnostics:"},
	)

	// Flag errors ARE usage errors — point at --help without the dump.
	rootCmd.SetFlagErrorFunc(func(c *cobra.Command, err error) error {
		return fmt.Errorf("%w\nRun '%s --help' for usage", err, c.CommandPath())
	})

	// Custom help template with bold headers and underlined section titles.
	rootCmd.SetUsageTemplate(usageTemplate)

	// Merge help functions for bold/underline.
	cobra.AddTemplateFuncs(helpFuncs)

	// Don't show "completion" command.
	rootCmd.CompletionOptions.HiddenDefaultCmd = true
}

// Execute is the entry point called from main.
func Execute() error {
	rootCmd.SetOut(os.Stdout)
	printRemoteContextBanner()
	err := rootCmd.Execute()
	if err != nil && os.Getenv("RUNQ_SOCKET") != "" && isSocketDialError(err) {
		// Remote-CLI perspective (RQ-74): a dead forwarded socket means the
		// OWNING machine is gone, not this login node — every command says
		// so, not just doctor, and says what (not) to do about it.
		return fmt.Errorf("%w\n\nremote CLI: the runq daemon on your own machine is not reachable through the forward (asleep / offline / stopped). It reconnects automatically once that machine is back — nothing to fix on this login node. Details: runq doctor", err)
	}
	return err
}

// isSocketDialError matches the failure shapes of a unix-socket dial against
// a forward whose owning daemon is gone (refused / file missing / EOF).
func isSocketDialError(err error) bool {
	msg := strings.ToLower(err.Error())
	for _, sig := range []string{"dial unix", "connection refused", "no such file or directory", "broken pipe"} {
		if strings.Contains(msg, sig) {
			return true
		}
	}
	return false
}

// printRemoteContextBanner prints ONE stderr line before every command when
// runq is running as a remote CLI (RUNQ_SOCKET set by the login-node env
// file): which target this shell is bound to, i.e. whose daemon the command
// will reach. With several machines forwarding to one cluster account this
// is the cheapest guard against submitting through the wrong context
// (RQ-74). stderr on purpose — `runq ps --json | jq` must stay parseable.
func printRemoteContextBanner() {
	if os.Getenv("RUNQ_SOCKET") == "" {
		return
	}
	// Never during shell-completion callbacks: cobra's __complete protocol
	// runs `runq __complete ...` on every TAB, and some shells surface
	// stderr — a banner there would garble completion for remote users.
	if len(os.Args) > 1 {
		switch os.Args[1] {
		case cobra.ShellCompRequestCmd, cobra.ShellCompNoDescRequestCmd, "completion":
			return
		}
	}
	target := os.Getenv("RUNQ_TARGET")
	if target == "" {
		target = "(RUNQ_TARGET unset)"
	}
	fmt.Fprintf(os.Stderr, "%s\n", utils.Dimf("runq target: %s (remote CLI)", target))
}

// usageTemplate is a customized Cobra usage template with grouped commands
// and ANSI formatting.
// usageTemplate renders USAGE ONLY — no .Short/.Long here: cobra's help
// template already prints LongOrShort before calling usage, so including
// them again double-prints every description (RQ-65 #6).
const usageTemplate = `{{if .Runnable}}{{underline "Usage:"}}
  {{.UseLine}}
{{end}}
{{- if .HasAvailableSubCommands}}
{{- range .Groups}}

{{bold .Title}}
{{- range (commandsWithGroup $ .ID)}}
  {{rpad .Name .NamePadding}}  {{.Short}}
{{- end}}
{{- end}}

{{- if not .AllChildCommandsHaveGroup}}

{{underline "Additional Commands:"}}
{{- range .Commands}}
{{- if (and (eq .GroupID "") (not .Hidden) .IsAvailableCommand)}}
  {{rpad .Name .NamePadding}}  {{.Short}}
{{- end}}
{{- end}}
{{- end}}
{{- end}}

{{- if .HasAvailableLocalFlags}}

{{underline "Flags:"}}
{{.LocalFlags.FlagUsages | trimTrailingWhitespaces}}
{{- end}}

{{- if .HasAvailableInheritedFlags}}

{{underline "Global Flags:"}}
{{.InheritedFlags.FlagUsages | trimTrailingWhitespaces}}
{{- end}}

{{- if .HasExample}}

{{underline "Examples:"}}
{{.Example}}
{{- end}}

Use "{{.CommandPath}} [command] --help" for more information about a command.
{{dim "Docs & annotated config examples: https://github.com/gliese129/runq-lab — docs/ and examples/"}}
`
