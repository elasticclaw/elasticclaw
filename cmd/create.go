package cmd

import (
	"context"
	"fmt"

	"github.com/elasticclaw/elasticclaw/pkg/config"
	"github.com/elasticclaw/elasticclaw/pkg/hub"
	"github.com/spf13/cobra"
)

var (
	createName string
	createEnvs []string
)

var createCmd = &cobra.Command{
	Use:   "create",
	Short: "Create a new claw from a template",
	Long: `Resolve a local template and provision a new claw via the hub.

The template is looked up in (in order):
  ./.elasticclaw/templates/<name>/
  ~/.elasticclaw/templates/<name>/

The template directory must contain elasticclaw-config.yaml specifying the provider.

Example:
  elasticclaw create --name support-01 --template support
  elasticclaw create --name dev-01 --template dev --env GITHUB_TOKEN=xxx`,
	RunE: runCreate,
}

var (
	createTemplate     string
	createInstanceType string
	createTTL          string
)

func init() {
	rootCmd.AddCommand(createCmd)
	createCmd.Flags().StringVarP(&createName, "name", "n", "", "claw name (required)")
	createCmd.MarkFlagRequired("name")
	createCmd.Flags().StringVarP(&createTemplate, "template", "t", "", "template name (required)")
	createCmd.MarkFlagRequired("template")
	createCmd.Flags().StringArrayVar(&createEnvs, "env", nil, "extra env vars to inject (KEY=value)")
	createCmd.Flags().StringVar(&createInstanceType, "instance-type", "", "override instance type (e.g. r1.small for Replicated)")
	createCmd.Flags().StringVar(&createTTL, "ttl", "", "override TTL (e.g. 2h, 24h)")
}

func runCreate(cmd *cobra.Command, args []string) error {
	// Load CLI config → get hub connection
	cfg, err := config.LoadGlobalConfig()
	if err != nil {
		return err
	}
	if cfg.Hub == nil || cfg.Hub.URL == "" {
		return fmt.Errorf("no hub configured — run 'elasticclaw login' first")
	}

	// Resolve template directory
	templateDir, err := config.ResolveTemplate(createTemplate)
	if err != nil {
		return err
	}
	fmt.Printf("Using template: %s\n", templateDir)

	// Load template config (provider, resources, etc.)
	tmplCfg, err := config.LoadTemplateConfig(templateDir)
	if err != nil {
		return err
	}

	// Read template files
	files, err := config.ReadTemplateFiles(templateDir)
	if err != nil {
		return err
	}
	fmt.Printf("Template files: %d files, provider: %s\n", len(files), tmplCfg.Provider)

	// Parse extra env vars
	env := parseEnvVars(createEnvs)

	// CLI flags override template config
	if createInstanceType != "" {
		tmplCfg.InstanceType = createInstanceType
	}
	if createTTL != "" {
		tmplCfg.TTL = createTTL
	}

	// POST to hub
	client := hub.NewClient(cfg.Hub.URL, cfg.Hub.Token)
	claw, err := client.CreateClaw(context.Background(), createName, createTemplate, tmplCfg, files, env)
	if err != nil {
		return fmt.Errorf("hub error: %w", err)
	}

	fmt.Println()
	fmt.Printf("✓ Claw provisioning started\n")
	fmt.Printf("  ID:       %s\n", claw.ID)
	fmt.Printf("  Name:     %s\n", claw.Name)
	fmt.Printf("  Template: %s\n", claw.Template)
	fmt.Printf("  Status:   %s\n", claw.Status)
	fmt.Println()
	fmt.Printf("  Watch:    elasticclaw list\n")
	fmt.Printf("  Chat:     elasticclaw chat %s\n", claw.Name)
	return nil
}

func parseEnvVars(envs []string) map[string]string {
	result := make(map[string]string)
	for _, e := range envs {
		for i, c := range e {
			if c == '=' {
				result[e[:i]] = e[i+1:]
				break
			}
		}
	}
	return result
}
