package cli

import (
	"bufio"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"

	"github.com/gliese129/runq-lab/internal/api"
	"github.com/gliese129/runq-lab/internal/backend"
	"github.com/gliese129/runq-lab/internal/logfile"
	"github.com/gliese129/runq-lab/internal/utils"
	"github.com/spf13/cobra"
)

// ── runq logs（spec §7.2: GET /tasks/{id}/log, follow → SSE stream）──
//
// v1 起日志一律走 daemon（远端任务经归属 target 的 FS 读取）；CLI 不再
// 直接摸本地文件 —— fsnotify tail 已退役，remote 任务因此免费获得 logs -f。

var logsCmd = &cobra.Command{
	Use:   "logs <task_id>",
	Short: "Show task output (default: tail + follow)",
	Example: `  runq logs a3f9              # last 200 lines, then follow (SSE)
  runq logs a3f9 --no-follow  # print tail and exit
  runq logs a3f9 -n 500       # tail size`,
	Args: cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		id := args[0]
		noFollow, _ := cmd.Flags().GetBool("no-follow")
		lines, _ := cmd.Flags().GetInt("lines")

		return withBackend(cmd, func(be backend.Backend) error {
			view, err := be.GetTask(cmd.Context(), id)
			if err != nil {
				return err
			}

			fmt.Printf("%s  %s  %s\n",
				utils.IDColor(view.ID),
				utils.StatusColor(view.Status),
				utils.Dimf("%s", view.LogPath))

			// First paint: tail view (spec — the entry point has no byte
			// coordinates; the returned NextOffset seeds the follow).
			page, err := be.TaskLogTail(cmd.Context(), view.ID, lines)
			if err != nil {
				return err
			}
			for _, l := range page.Lines {
				fmt.Println(l)
			}
			if noFollow {
				return nil
			}
			return followLogSSE(cmd, view.ID, page.NextOffset)
		})
	},
}

// followLogSSE consumes GET /tasks/{id}/log/stream (spec §6.3):
// text/event-stream, events named "lines", data = LogPage JSON. The
// next_offset of each page is the reconnect anchor — on stream end
// (daemon restart, transient error) we resume from where we stopped.
func followLogSSE(cmd *cobra.Command, taskID string, offset int64) error {
	// Timeout 0: a follow stream is open-ended; lifetime is ctx-controlled.
	client := api.NewClientWithTimeout(getSocketPath(), 0)
	for {
		path := "/api/v1/tasks/" + taskID + "/log/stream?offset=" + strconv.FormatInt(offset, 10)
		resp, err := client.Do(cmd.Context(), "GET", path, nil)
		if err != nil {
			if cmd.Context().Err() != nil {
				return nil // ^C
			}
			return err
		}

		sc := bufio.NewScanner(resp.Body)
		// A page can carry up to the server's page byte budget.
		sc.Buffer(make([]byte, 0, 64*1024), logfile.MaxPageSize+1<<20)
		for sc.Scan() {
			line := sc.Text()
			if !strings.HasPrefix(line, "data: ") {
				continue // event:/heartbeat/blank lines
			}
			var page backend.LogPage
			if err := json.Unmarshal([]byte(strings.TrimPrefix(line, "data: ")), &page); err != nil {
				continue
			}
			if page.Offset < offset {
				fmt.Println(utils.Dimf("── log rotated, restarting from top ──"))
			}
			for _, l := range page.Lines {
				fmt.Println(l)
			}
			offset = page.NextOffset
		}
		resp.Body.Close()
		if cmd.Context().Err() != nil {
			return nil
		}
		// Stream ended without cancellation → reconnect from offset.
	}
}

func init() {
	logsCmd.Flags().Bool("no-follow", false, "Print tail and exit (no follow)")
	logsCmd.Flags().IntP("lines", "n", logfile.DefaultPageLines, "Tail size (lines)")

	logsCmd.GroupID = groupCore
	rootCmd.AddCommand(logsCmd)
}
