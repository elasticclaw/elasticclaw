package cmd

import (
	"fmt"

	"github.com/spf13/cobra"
)

var (
	Version   = "dev"
	Commit    = "none"
	BuildDate = "unknown"
)

var versionCmd = &cobra.Command{
	Use:   "version",
	Short: "Print the version number",
	Run: func(cmd *cobra.Command, args []string) {
		if jsonOut {
			fmt.Printf(`{"version":"%s","commit":"%s","buildDate":"%s"}`+"\n", Version, Commit, BuildDate)
		} else {
			fmt.Printf("elasticclaw %s (commit: %s, built: %s)\n", Version, Commit, BuildDate)
		}
	},
}

func init() {
	rootCmd.AddCommand(versionCmd)
}
