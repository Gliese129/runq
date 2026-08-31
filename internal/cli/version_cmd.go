package cli

import (
	"fmt"

	"github.com/gliese129/runq-lab/internal/version"

	"github.com/spf13/cobra"
)

// `runq version` prints exactly the build version and nothing else: it is
// parsed programmatically (`runq connect` runs it on the remote to decide
// whether the installed binary is stale), so the output is the contract.
func init() {
	versionCmd.GroupID = groupDiag
	rootCmd.AddCommand(versionCmd)
}

var versionCmd = &cobra.Command{
	Use:   "version",
	Short: "Print the runq build version",
	Args:  cobra.NoArgs,
	Run: func(cmd *cobra.Command, args []string) {
		fmt.Println(version.Version)
	},
}
