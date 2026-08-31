package utils

import (
	"fmt"
	"os"

	"golang.org/x/term"
)

// ANSI color and style codes.
const (
	reset     = "\033[0m"
	bold      = "\033[1m"
	underline = "\033[4m"
	red       = "\033[31m"
	green     = "\033[32m"
	yellow    = "\033[33m"
	cyan      = "\033[36m"
	gray      = "\033[90m"
)

// isTTY reports whether stdout is connected to a terminal.
// Colors are disabled when piping output.
var isTTY = term.IsTerminal(int(os.Stdout.Fd()))

// colorize wraps text in ANSI color codes if stdout is a terminal.
// Returns text unchanged when isTTY is false (piped output).
func colorize(color, text string) string {
	if !isTTY || color == "" {
		return text
	}
	return color + text + reset
}

// StatusColor returns the status string with an appropriate color.
// Mapping mirrors the dashboard's statusGrammar (CI convention):
// running/submitting→cyan, pending→yellow, success/done→green, failed→red,
// killed/partial→yellow (human decision / mixed outcome, not failure red),
// unknown→gray (no verdict yet — RQ-74), paused→no color.
func StatusColor(status string) string {
	var color string
	switch status {
	case "running", "submitting":
		color = cyan
	case "pending":
		color = yellow
	case "success", "done":
		color = green
	case "failed":
		color = red
	case "killed", "partial":
		color = yellow
	case "unknown":
		// RQ-74: outcome-unknown submission — gray, mirroring the dashboard
		// statusGrammar's slate ("no verdict" earns no outcome color).
		color = gray
	default:
		color = ""
	}
	return colorize(color, status)
}

// IDColor returns a task/job ID with cyan styling for quick visual scanning.
func IDColor(id string) string {
	return colorize(cyan, id)
}

// PassFail returns a colored ✓ (green) or ✗ (red).
func PassFail(ok bool) string {
	if ok {
		return colorize(green, "✓")
	}
	return colorize(red, "✗")
}

// Dimf formats and dims text in gray (for secondary information).
func Dimf(format string, a ...any) string {
	str := fmt.Sprintf(format, a...)
	return colorize(gray, str)
}

// Bold wraps text in ANSI bold.
func Bold(s string) string {
	return colorize(bold, s)
}

// Underline wraps text in ANSI underline.
func Underline(s string) string {
	return colorize(underline, s)
}
