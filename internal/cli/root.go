package cli

import (
	"github.com/spf13/cobra"
)

var rootCmd = &cobra.Command{
	Use:   "runq",
	Short: "A lightweight GPU job scheduler for research labs",
	Long: `runq is a single-machine GPU job scheduler designed for research labs.

It manages GPU allocation, parameter sweep expansion, and task lifecycle
with minimal configuration overhead.

Data model: Project → Job → Task
  - Project: a registered experiment type (command template, GPU defaults, resume config)
  - Job:     a submitted sweep that expands into multiple tasks
  - Task:    the smallest schedulable unit (one command on N GPUs)

Quick start:
  runq daemon start              Start the scheduler daemon
  runq project add .             Register a project from ./project.yaml
  runq submit .                  Submit a job from ./job.yaml
  runq ps                        See running and pending tasks
  runq logs <task_id>            Tail task output`,
}

func init() {
	rootCmd.PersistentFlags().String("socket", "", "path to daemon unix socket")
}

// Execute is the entry point called from main.
func Execute() error {
	return rootCmd.Execute()
}
