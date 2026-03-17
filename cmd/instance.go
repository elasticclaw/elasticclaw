package cmd

import (
	"github.com/spf13/cobra"
)

var instanceCmd = &cobra.Command{
	Use:   "instance",
	Short: "Instance management commands",
	Long:  "Commands for managing ElasticClaw instances. These are aliases for top-level commands.",
}

func init() {
	rootCmd.AddCommand(instanceCmd)

	// Add subcommands that alias to top-level commands
	instanceCmd.AddCommand(&cobra.Command{
		Use:   "list",
		Short: "List instances",
		RunE:  runList,
	})

	instanceCmd.AddCommand(&cobra.Command{
		Use:   "inspect <instance>",
		Short: "Show instance details",
		Args:  cobra.ExactArgs(1),
		RunE:  runInspect,
	})

	instanceCmd.AddCommand(&cobra.Command{
		Use:   "chat <instance> [message]",
		Short: "Send messages to an instance",
		Args:  cobra.MinimumNArgs(1),
		RunE:  runChat,
	})

	instanceCmd.AddCommand(&cobra.Command{
		Use:   "destroy <instance>",
		Short: "Destroy an instance",
		Args:  cobra.MaximumNArgs(1),
		RunE:  runDestroy,
	})
}
