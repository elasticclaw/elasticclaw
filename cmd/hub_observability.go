package cmd

import (
	"errors"
	"fmt"

	"github.com/elasticclaw/elasticclaw/pkg/config"
	obsinstall "github.com/elasticclaw/elasticclaw/pkg/observability/install"
	"github.com/spf13/cobra"
)

var hubObservabilityCmd = &cobra.Command{
	Use:   "observability",
	Short: "Manage the native, opt-in observability stack (Linux/systemd, root)",
}

func init() {
	var force, restartHub, purgeData bool
	var publicURL, environment string
	installCmd := &cobra.Command{
		Use: "install", Short: "Install and start Collector, VictoriaMetrics, VictoriaLogs, Tempo, and Grafana", Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg, err := config.LoadHubConfig()
			if err != nil {
				return err
			}
			if !config.HubConfigExists() {
				return config.ErrNoHubConfig
			}
			resolvedURL := publicURL
			if resolvedURL == "" {
				resolvedURL = cfg.PublicURL
			}
			installer := obsinstall.New("", cmd.OutOrStdout())
			if err := installer.Install(cmd.Context(), obsinstall.Options{Force: force, PublicURL: resolvedURL}); err != nil {
				return err
			}
			var env *string
			if cmd.Flags().Changed("environment") {
				env = &environment
			}
			if err := config.UpdateHubObservability(true, env); err != nil {
				return err
			}
			if err := installer.UpdateCaddy(cmd.Context(), true); err != nil {
				return err
			}
			if restartHub {
				if _, err := installer.Runner.Run(cmd.Context(), "systemctl", "restart", "elasticclaw"); err != nil {
					return err
				}
			} else {
				fmt.Fprintln(cmd.OutOrStdout(), "Restart the hub to enable telemetry: sudo systemctl restart elasticclaw")
			}
			grafanaURL, _ := obsinstall.GrafanaURL(resolvedURL)
			fmt.Fprintf(cmd.OutOrStdout(), "Grafana: %s\nLogin: admin; password file: %s\nRetention: metrics 15d, logs 7d (4 GiB cap), traces 72h\nStatus: sudo elasticclaw hub observability status\n", grafanaURL, obsinstall.PasswordFile)
			return nil
		},
	}
	installCmd.Flags().BoolVar(&force, "force", false, "override minimum free RAM and disk requirements")
	installCmd.Flags().BoolVar(&restartHub, "restart-hub", false, "restart elasticclaw after enabling telemetry")
	installCmd.Flags().StringVar(&publicURL, "public-url", "", "override hub.yaml public_url for Grafana")
	installCmd.Flags().StringVar(&environment, "environment", "", "set the hub telemetry environment")
	uninstallCmd := &cobra.Command{
		Use: "uninstall", Short: "Remove the stack, keeping telemetry data unless --purge-data", Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			if _, err := config.LoadHubConfig(); err != nil {
				return err
			}
			installer := obsinstall.New("", cmd.OutOrStdout())
			if err := installer.Uninstall(cmd.Context(), purgeData); err != nil {
				return err
			}
			if err := config.UpdateHubObservability(false, nil); errors.Is(err, config.ErrNoHubConfig) {
				fmt.Fprintln(cmd.OutOrStdout(), "No hub.yaml found; nothing to disable.")
			} else if err != nil {
				return err
			}
			fmt.Fprintln(cmd.OutOrStdout(), "Observability stack removed. Apply disabled telemetry: sudo systemctl restart elasticclaw")
			return nil
		},
	}
	uninstallCmd.Flags().BoolVar(&purgeData, "purge-data", false, "also delete persisted telemetry and Grafana data")
	statusCmd := &cobra.Command{Use: "status", Short: "Show service health, versions, data usage, and Grafana URL", Args: cobra.NoArgs, RunE: func(cmd *cobra.Command, args []string) error {
		return obsinstall.New("", cmd.OutOrStdout()).Status(cmd.Context())
	}}
	hubObservabilityCmd.AddCommand(installCmd, uninstallCmd, statusCmd)
	hubCmd.AddCommand(hubObservabilityCmd)
}
