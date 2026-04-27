package cmd

import (
	"fmt"
	"os"
	"path/filepath"

	"github.com/elasticclaw/elasticclaw/pkg/config"
	"github.com/elasticclaw/elasticclaw/pkg/hub"
	"github.com/elasticclaw/elasticclaw/pkg/types"
	"github.com/spf13/cobra"
)

var (
	hubAddr        string
	hubDBPath      string
	hubToken       string
	hubClawToken   string
	hubUIPassword     string
	hubNoWebUI     bool
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
	hubCmd.Flags().StringVar(&hubUIPassword, "ui-token", "", "web UI login password (default: value from hub.yaml ui_password, or 'admin')")
	hubCmd.Flags().BoolVar(&hubNoWebUI, "no-web-ui", false, "disable embedded web UI (use when running Next.js dev server separately)")
}

func runHub(cmd *cobra.Command, args []string) error {
	// Use /var/lib/elasticclaw/ when system config exists (/etc/elasticclaw/hub.yaml),
	// otherwise fall back to ~/.elasticclaw/ for local dev.
	var dir string
	if _, err := os.Stat("/etc/elasticclaw/hub.yaml"); err == nil {
		dir = "/var/lib/elasticclaw"
	} else {
		home, err := os.UserHomeDir()
		if err != nil {
			return err
		}
		dir = filepath.Join(home, ".elasticclaw")
	}
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

	// Apply LLM keys from env — backward compat: if ANTHROPIC_API_KEY set and no anthropic key configured, add it
	if apiKey := os.Getenv("ANTHROPIC_API_KEY"); apiKey != "" {
		hasAnthropicKey := false
		for _, k := range hubCfg.LLMKeys {
			if k.Provider == "anthropic" {
				hasAnthropicKey = true
				break
			}
		}
		if !hasAnthropicKey {
			hubCfg.LLMKeys = append(hubCfg.LLMKeys, &types.LLMKeyConfig{
				Name:     "anthropic",
				Provider: "anthropic",
				APIKey:   apiKey,
				Default:  len(hubCfg.LLMKeys) == 0,
			})
		}
	}

	// CLI flag overrides yaml for ui_password
	if hubUIPassword != "" {
		hubCfg.UIPassword = hubUIPassword
	}

	hub.Version = Version // propagate build-time version for bridge download URL
	s, err := hub.NewServer(hubAddr, dbPath, dir, hubCfg)
	if err != nil {
		return fmt.Errorf("failed to start hub: %w", err)
	}

	// Provision tenant from CLI flags or hub.yaml (whichever is set)
	provToken := hubToken
	if provToken == "" {
		provToken = hubCfg.Token
	}
	provClawToken := hubClawToken
	if provClawToken == "" {
		provClawToken = hubCfg.ClawToken
	}
	if provToken != "" && provClawToken != "" {
		if _, err := s.Provision(provToken, provClawToken); err != nil {
			return fmt.Errorf("failed to provision tenant: %w", err)
		}
	}

	fmt.Printf("ElasticClaw Hub\n")
	fmt.Printf("  Address:    %s\n", hubAddr)
	fmt.Printf("  Database:   %s\n", dbPath)
	if hubCfg.URL != "" {
		fmt.Printf("  URL:        %s\n", hubCfg.URL)
	}
	if hubCfg.PublicURL != "" {
		fmt.Printf("  Public URL: %s\n", hubCfg.PublicURL)
	}
	fmt.Printf("  Claw token: %s\n", func() string { if hubCfg.ClawToken != "" { return "(set)" }; return "(not set)" }())
	fmt.Println()

	if len(hubCfg.Providers) == 0 {
		fmt.Println("  Providers: none configured")
	} else {
		fmt.Printf("  Providers:\n")
		for name, pcfg := range hubCfg.Providers {
			switch name {
			case "replicated":
				instType := pcfg.DefaultInstanceType
				if instType == "" {
					instType = "r1.large (default)"
				}
				ttl := pcfg.DefaultTTL
				if ttl == "" {
					ttl = "48h (default)"
				}
				fmt.Printf("    replicated  instance-type=%s  ttl=%s\n", instType, ttl)
			case "daytona":
				apiURL := pcfg.APIURL
				if apiURL == "" {
					apiURL = "app.daytona.io (default)"
				}
				fmt.Printf("    daytona     api=%s\n", apiURL)
			case "local":
				fmt.Printf("    local\n")
			default:
				fmt.Printf("    %s\n", name)
			}
		}
	}
	fmt.Println()

	return s.Run(hub.RunOptions{NoWebUI: hubNoWebUI})
}
