package cli

// ── runq target：target 的一等名词命令组（RQ-65）──
//
// target 是一等实体，它的动词理应住在一个屋檐下：
//
//	runq target add/ls/show/edit/check <name>   配置 CRUD（原 runq config add/...）
//	runq target use <name> [-p]                 选择活动 target（原 config set-target）
//	runq target connect <name>...               连接仪式（host key 信任 + 远端 CLI + forward）
//	runq target disconnect <name>...            收掉 forward，remote_cli: false
//
// `runq config` 回归纯键值心智（set/get/list）。顶层 `runq connect` 保留为
// 别名——它是新用户的第一动作，值得一个短入口。

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"time"

	"github.com/gliese129/runq/internal/api"
	"github.com/gliese129/runq/internal/config"

	"github.com/spf13/cobra"
)

var targetCmd = &cobra.Command{
	Use:   "target",
	Short: "Manage compute targets (add, inspect, connect, select)",
}

var targetUseCmd = &cobra.Command{
	Use:   "use <name>",
	Short: "Select the active compute target for this session (or persistently with -p)",
	Long: `Select the active compute target. Without -p, writes to ~/.runq/.active-target
(session-scoped, overridden by $RUNQ_TARGET or --target flag). With -p,
writes to config.yaml default_target (persists across sessions).`,
	Example: `  runq target use tsubame       # session-scoped
  runq target use tsubame -p    # persistent default`,
	Args: cobra.ExactArgs(1),
	RunE: runConfigSetTarget,
}

var targetDisconnectCmd = &cobra.Command{
	Use:          "disconnect <name>...",
	Short:        "Stop the remote CLI forward and disable remote_cli",
	Args:         cobra.MinimumNArgs(1),
	SilenceUsage: true,
	RunE:         runTargetDisconnect,
}

func runTargetDisconnect(cmd *cobra.Command, args []string) error {
	cfg, err := config.Load()
	if err != nil {
		return err
	}
	changed := false
	for _, name := range args {
		found := false
		for i := range cfg.Targets {
			if cfg.Targets[i].Name != name {
				continue
			}
			found = true
			if cfg.Targets[i].RemoteCLI {
				cfg.Targets[i].RemoteCLI = false
				changed = true
				fmt.Printf("✓ remote_cli disabled for %q\n", name)
			} else {
				fmt.Printf("- %q was not connected\n", name)
			}
		}
		if !found {
			return fmt.Errorf("target %q not found", name)
		}
	}
	if changed {
		if err := config.SaveGlobal(cfg); err != nil {
			return err
		}
	}
	// Live teardown, best-effort: a stopped daemon simply won't start the
	// forward next boot (config already says so).
	ctx, cancel := context.WithTimeout(cmd.Context(), 10*time.Second)
	defer cancel()
	client := api.NewClient(getSocketPath())
	for _, name := range args {
		resp, err := client.Do(ctx, http.MethodPost, "/api/v1/targets/"+url.PathEscape(name)+"/disconnect", nil)
		if err != nil {
			fmt.Println("Daemon not running — nothing live to stop.")
			break
		}
		resp.Body.Close()
		if resp.StatusCode == http.StatusOK {
			fmt.Printf("✓ forward stopped for %q\n", name)
		}
	}
	return nil
}

func init() {
	targetUseCmd.Flags().BoolP("persist", "p", false, "Write to config default_target instead of session file")

	targetCmd.AddCommand(configAddCmd, configShowCmd, configEditCmd, configCheckCmd)
	targetCmd.AddCommand(targetUseCmd, connectCmd, targetDisconnectCmd)
	targetCmd.GroupID = groupManagement
	rootCmd.AddCommand(targetCmd)
}
