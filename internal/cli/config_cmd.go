package cli

import (
	"fmt"
	"sort"
	"strings"

	"github.com/gliese129/runq/internal/config"
	"github.com/spf13/cobra"
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

func init() {
	configCmd.AddCommand(configSetCmd, configGetCmd, configListCmd)
	configCmd.GroupID = groupManagement
	rootCmd.AddCommand(configCmd)
}
