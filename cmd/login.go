package cmd

import (
	"context"
	"fmt"

	"github.com/elasticclaw/elasticclaw/pkg/config"
	"github.com/elasticclaw/elasticclaw/pkg/hub"
	"github.com/elasticclaw/elasticclaw/pkg/types"
	"github.com/spf13/cobra"
)

var loginCmd = &cobra.Command{
	Use:   "login",
	Short: "Connect the CLI to an ElasticClaw hub",
	Long: `Configure the CLI to talk to an ElasticClaw hub.

  elasticclaw login --hub https://hub.example.com --token mytoken`,
	RunE: runLogin,
}

var (
	loginHub   string
	loginToken string
)

func init() {
	rootCmd.AddCommand(loginCmd)
	loginCmd.Flags().StringVar(&loginHub, "hub", "", "hub URL (e.g. https://hub.example.com)")
	loginCmd.Flags().StringVar(&loginToken, "token", "", "your hub user token")
	loginCmd.MarkFlagRequired("hub")
	loginCmd.MarkFlagRequired("token")
}

func runLogin(cmd *cobra.Command, args []string) error {
	client := hub.NewClient(loginHub, loginToken)
	tenantID, err := client.Login(context.Background())
	if err != nil {
		return fmt.Errorf("login failed: %w", err)
	}

	// Write CLI connection info to config.yaml (separate from hub.yaml server config)
	cfg, err := config.LoadGlobalConfig()
	if err != nil {
		cfg = &types.GlobalConfig{}
	}

	cfg.Hub = &types.HubConfig{
		URL:   loginHub,
		Token: loginToken,
	}

	if err := config.SaveGlobalConfig(cfg); err != nil {
		return fmt.Errorf("failed to save config: %w", err)
	}

	fmt.Printf("✓ Logged in to %s (tenant: %s)\n", loginHub, tenantID)
	fmt.Println("  Connection saved to ~/.elasticclaw/config.yaml")
	return nil
}
