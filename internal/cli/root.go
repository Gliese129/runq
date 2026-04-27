package cli

import (
	"os"
	"text/template"

	"github.com/gliese129/runq/internal/utils"
	"github.com/spf13/cobra"
)

// ANSI helpers for help template (only active when stdout is a terminal).
var helpFuncs = template.FuncMap{
	"bold":      func(s string) string { return utils.Bold(s) },
	"underline": func(s string) string { return utils.Underline(s) },
	"dim":       func(s string) string { return utils.Dimf("%s", s) },
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
	Long: `runq — single-machine GPU job scheduler for research labs

  Get started in 3 steps:

    1. runq init                   Scan your train.py and generate configs
    2. runq project add .          Register the project
    3. runq submit job.yaml        Submit and go

  Or skip YAML entirely:

    runq sweep --project myproj lr=1e-4,3e-4 batch=32,64`,
}

func init() {
	rootCmd.PersistentFlags().String("socket", "", "path to daemon unix socket")

	// Register command groups.
	rootCmd.AddGroup(
		&cobra.Group{ID: groupCore, Title: "Core Commands:"},
		&cobra.Group{ID: groupManagement, Title: "Management:"},
		&cobra.Group{ID: groupDiag, Title: "Setup & Diagnostics:"},
	)

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
	return rootCmd.Execute()
}

// usageTemplate is a customized Cobra usage template with grouped commands
// and ANSI formatting.
const usageTemplate = `{{bold .Short}}

{{if .Long}}{{.Long}}
{{end}}
{{if .Runnable}}{{underline "Usage:"}}
  {{.UseLine}}
{{end}}
{{- if .HasAvailableSubCommands}}
{{- range .Groups}}

{{bold .Title}}
{{- range ($.CommandsWithGroup .ID)}}
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
`
