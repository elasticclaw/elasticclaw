package cmd

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/elasticclaw/elasticclaw/pkg/config"
	"github.com/elasticclaw/elasticclaw/pkg/hub"
	"github.com/elasticclaw/elasticclaw/pkg/hub/logger"
	"github.com/elasticclaw/elasticclaw/pkg/hub/telemetry"
	"github.com/elasticclaw/elasticclaw/pkg/types"
	"github.com/spf13/cobra"
)

var (
	hubAddr       string
	hubDBPath     string
	hubToken      string
	hubClawToken  string
	hubUIPassword string
	hubNoWebUI    bool
)

var hubCmd = &cobra.Command{
	Use:   "hub",
	Short: "Run the ElasticClaw Server",
	Long: `Start ElasticClaw Server — the control plane that agents register with and clients connect to.

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
	hubCmd.Flags().StringVar(&hubClawToken, "claw-token", "", "agent authentication token (required with --token)")
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

	// Root structured logger: created once at boot and installed as the
	// process default; the hub server picks it up via slog.Default().
	slog.SetDefault(logger.New())

	// Optional OTel tracing: only active when ELASTICCLAW_OTLP_ENDPOINT is
	// set; otherwise this is a no-op and the global tracer stays no-op.
	shutdownTraces, err := telemetry.Setup(context.Background())
	if err != nil {
		return fmt.Errorf("otel tracing setup: %w", err)
	}
	defer func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = shutdownTraces(ctx)
	}()
	if telemetry.Enabled() {
		slog.Info("OTel tracing enabled", "endpoint", os.Getenv(telemetry.EndpointEnv))
	}

	hub.Version = Version
	hub.Commit = Commit

	// Ensure external storage directories exist (factories/, templates/)
	if err := hub.EnsureExternalDirs(); err != nil {
		return fmt.Errorf("failed to create external storage dirs: %w", err)
	}

	s, err := hub.NewServer(hubAddr, dbPath, dir, hubCfg)
	if err != nil {
		return fmt.Errorf("failed to start hub: %w", err)
	}

	// Migrate legacy templates from SQLite and factories from hub.yaml.
	// Migration is mandatory — if it fails, the hub refuses to start to prevent
	// split-brain where some data lives in external storage and some in legacy locations.
	// Also re-runs if hub.yaml still has inline factories (e.g. prior migration didn't
	// clean up, or user manually edited hub.yaml after migration).
	needsMigrate := !hub.HasMigratedV2()
	needsCleanup := len(hubCfg.Factories) > 0
	if needsMigrate || needsCleanup {
		fmt.Println("[hub] migrating legacy templates and factories to external storage...")
		if needsMigrate {
			if err := s.MigrateLegacyTemplates(); err != nil {
				return fmt.Errorf("template migration failed: %w", err)
			}
		}
		migrated, err := hub.MigrateLegacyFactories(hubCfg)
		if err != nil {
			return fmt.Errorf("factory migration failed: %w", err)
		}
		if len(migrated) > 0 {
			fmt.Printf("[hub] migrated factories: %s\n", strings.Join(migrated, ", "))
		}
		if needsMigrate {
			if err := hub.MarkMigratedV2(); err != nil {
				return fmt.Errorf("migration marker write failed: %w", err)
			}
			fmt.Println("[hub] migration complete — future runs will skip this step")
		} else {
			fmt.Println("[hub] cleaned up lingering factories from hub.yaml")
		}
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

	fmt.Printf("ElasticClaw Server\n")
	fmt.Printf("  Address:    %s\n", hubAddr)
	fmt.Printf("  Database:   %s\n", dbPath)
	if hubCfg.URL != "" {
		fmt.Printf("  URL:        %s\n", hubCfg.URL)
	}
	if hubCfg.PublicURL != "" {
		fmt.Printf("  Public URL: %s\n", hubCfg.PublicURL)
	}
	fmt.Printf("  Agent token: %s\n", func() string {
		if hubCfg.ClawToken != "" {
			return "(set)"
		}
		return "(not set)"
	}())
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
			default:
				fmt.Printf("    %s\n", name)
			}
		}
	}
	fmt.Println()

	return s.Run(hub.RunOptions{NoWebUI: hubNoWebUI})
}
