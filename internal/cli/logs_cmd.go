package cli

import (
	"bufio"
	"fmt"
	"io"
	"os"

	"github.com/fsnotify/fsnotify"
	"github.com/gliese129/runq/internal/backend"
	"github.com/gliese129/runq/internal/utils"
	"github.com/gosuri/uilive"
	"github.com/spf13/cobra"
)

// ── runq logs (shortcut for task logs) ──

var logsCmd = &cobra.Command{
	Use:   "logs <task_id>",
	Short: "Tail task output (default: follow mode)",
	Example: `  runq logs a3f9              # tail -f style
  runq logs a3f9 --no-follow  # print all and exit`,
	Args: cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		id := args[0]
		noFollow, _ := cmd.Flags().GetBool("no-follow")

		return withBackend(func(be backend.Backend) error {
			view, logPath, err := be.GetTask(cmd.Context(), id)
			if err != nil {
				return err
			}

			fmt.Printf("%s  %s  %s\n",
				utils.IDColor(view.ID),
				utils.StatusColor(view.Status),
				utils.Dimf("%s", logPath))

			if noFollow {
				data, err := os.ReadFile(logPath)
				if err != nil {
					return err
				}
				fmt.Print(string(data))
				return nil
			}

			return tailLog(logPath)
		})
	},
}

// tailLog follows a log file (tail -f style) until interrupted.
func tailLog(logfile string) error {
	writer := uilive.New()
	writer.Start()
	defer writer.Stop()

	file, err := os.Open(logfile)
	if err != nil {
		return err
	}
	if _, err := file.Seek(0, io.SeekEnd); err != nil {
		return err
	}

	watcher, err := fsnotify.NewWatcher()
	if err != nil {
		return err
	}
	defer watcher.Close()
	watcher.Add(logfile)

	reader := bufio.NewReader(file)
	for {
		select {
		case event, ok := <-watcher.Events:
			if !ok {
				return nil
			}
			if event.Op&fsnotify.Write == fsnotify.Write {
				for {
					line, err := reader.ReadString('\n')
					if err == io.EOF {
						break
					}
					if err != nil {
						return err
					}
					fmt.Fprintf(writer.Bypass(), "%s", line)
				}
			}
		case err, ok := <-watcher.Errors:
			if !ok {
				return nil
			}
			return err
		}
	}
}

func init() {
	logsCmd.Flags().Bool("no-follow", false, "Print log and exit (no tail -f)")

	logsCmd.GroupID = groupCore
	rootCmd.AddCommand(logsCmd)
}
