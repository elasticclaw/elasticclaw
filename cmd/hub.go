package cmd

import (
	"fmt"
	"os"
	"path/filepath"

	"github.com/elasticclaw/elasticclaw/pkg/config"
	"github.com/elasticclaw/elasticclaw/pkg/hub"
	"github.com/spf13/cobra"
)

var (
	hubAddr      string
	hubDBPath    string
	hubToken     string
	hubClawToken string
)

var hubCmd = &cobra.Command{
	Use:   "hub",
	Short: "Run the ElasticClaw hub server",
	Long: `Start the ElasticClaw hub — the control plane that claws register with and clients connect to.

For first-time setup, provide --token and --claw-token to create the default tenant:

  elasticclaw hub --token mytoken --claw-token myclawtoken

The hub stores data in a SQLite database (~/.elasticclaw/hub.db by default).
`,
	RunE: runHub,
}

func init() {
	rootCmd.AddCommand(hubCmd)

	hubCmd.Flags().StringVar(&hubAddr, "addr", ":8080", "address to listen on")
	hubCmd.Flags().StringVar(&hubDBPath, "db", "", "path to SQLite database (default: ~/.elasticclaw/hub.db)")
	hubCmd.Flags().StringVar(&hubToken, "token", "", "create/update default tenant with this user token")
	hubCmd.Flags().StringVar(&hubClawToken, "claw-token", "", "claw authentication token (required with --token)")
}

func runHub(cmd *cobra.Command, args []string) error {
	home, err := os.UserHomeDir()
	if err != nil {
		return err
	}
	dir := filepath.Join(home, ".elasticclaw")
	if err := os.MkdirAll(dir, 0755); err != nil {
		return err
	}

	dbPath := hubDBPath
	if dbPath == "" {
		dbPath = filepath.Join(dir, "hub.db")
	}

	hubCfg, err := config.LoadHubConfig()
	if err != nil {
		return fmt.Errorf("failed to load hub config: %w", err)
	}

	s, err := hub.NewServer(hubAddr, dbPath, dir, hubCfg)
	if err != nil {
		return fmt.Errorf("failed to start hub: %w", err)
	}

	if hubToken != "" {
		if hubClawToken == "" {
			return fmt.Errorf("--claw-token is required when --token is set")
		}
		tenantID, err := s.Provision(hubToken, hubClawToken)
		if err != nil {
			return fmt.Errorf("failed to provision tenant: %w", err)
		}
		fmt.Printf("Tenant provisioned: %s\n", tenantID)
		fmt.Printf("  User token:  %s\n", hubToken)
		fmt.Printf("  Claw token:  %s\n", hubClawToken)
		fmt.Println()
	}

	fmt.Printf("ElasticClaw Hub starting on %s (db: %s)\n", hubAddr, dbPath)
	return s.Run()
}
