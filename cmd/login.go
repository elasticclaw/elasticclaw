package cmd

import (
	"context"
	"fmt"

	"github.com/elasticclaw/elasticclaw/pkg/config"
	"github.com/elasticclaw/elasticclaw/pkg/hub"
	"github.com/spf13/cobra"
)

var loginCmd = &cobra.Command{
	Use:   "login",
	Short: "Connect the CLI to ElasticClaw Server",
	Long: `Configure the CLI to talk to ElasticClaw Server.

  elasticclaw login --hub https://hub.example.com --token mytoken
  elasticclaw login --hub https://hub.example.com --token mytoken --profile work`,
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
	_, err := client.Login(context.Background())
	if err != nil {
		return fmt.Errorf("login failed: %w", err)
	}

	// Determine which profile to save to
	profileName := profile // root --profile flag
	if profileName == "" {
		profileName = "default"
	}

	if err := config.AddHubProfile(profileName, loginHub, loginToken); err != nil {
		return fmt.Errorf("failed to save config: %w", err)
	}

	fmt.Printf("✓ Logged in to %s\n", loginHub)
	fmt.Printf("  Saved as profile %q (~/.elasticclaw/config.yaml)\n", profileName)
	return nil
}
