package cmd

import (
	"context"
	"fmt"
	"os"
	"strings"

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
	createTags         []string
)

func init() {
	rootCmd.AddCommand(createCmd)
	createCmd.Flags().StringVarP(&createName, "name", "n", "", "claw name (required)")
	createCmd.MarkFlagRequired("name")
	createCmd.Flags().StringVarP(&createTemplate, "template", "t", "base", "template name (default: base)")
	createCmd.Flags().StringArrayVar(&createEnvs, "env", nil, "extra env vars to inject (KEY=value)")
	createCmd.Flags().StringVar(&createInstanceType, "instance-type", "", "override instance type (e.g. r1.small for Replicated)")
	createCmd.Flags().StringVar(&createTTL, "ttl", "", "override TTL (e.g. 2h, 24h)")
	createCmd.Flags().StringArrayVar(&createTags, "tag", nil, "tag to apply (repeatable: --tag nix --tag env:prod)")
}

func runCreate(cmd *cobra.Command, args []string) error {
	// Resolve hub connection from profile
	h, _, err := config.ResolveHub(profile)
	if err != nil {
		return err
	}

	// Resolve template directory
	templateDir, err := config.ResolveTemplate(createTemplate)
	if err != nil {
		return err
	}
	fmt.Printf("Using template: %s\n", templateDir)
	if data, err := os.ReadFile(templateDir + "/elasticclaw-config.yaml"); err == nil {
		fmt.Printf("  Config:\n%s\n", string(data))
	}

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
	if tmplCfg.Nix {
		fmt.Println("  Nix: enabled (Determinate Systems)")
	}
	if tmplCfg.GitHub != nil && len(tmplCfg.GitHub.Repos) > 0 {
		fmt.Printf("  GitHub repos: %d\n", len(tmplCfg.GitHub.Repos))
	}
	// Merge tags: auto (template:<name>) + config yaml tags + --tag flags
	tmplCfg.Tags = mergeCLITags(createTemplate, tmplCfg.Tags, createTags)
	if len(tmplCfg.Tags) > 0 {
		fmt.Printf("  Tags: %s\n", strings.Join(tmplCfg.Tags, ", "))
	}

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
	client := hub.NewClient(h.URL, h.Token)
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

func mergeCLITags(templateName string, configTags, cliTags []string) []string {
	seen := make(map[string]bool)
	var result []string
	add := func(t string) {
		t = strings.TrimSpace(t)
		if t == "" {
			return
		}
		if !seen[t] {
			seen[t] = true
			result = append(result, t)
		}
	}
	add("template:" + templateName)
	for _, t := range configTags {
		add(t)
	}
	for _, t := range cliTags {
		add(t)
	}
	return result
}
