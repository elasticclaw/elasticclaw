package cmd

import (
	"context"
	"fmt"
	"os"
	"strings"

	"github.com/elasticclaw/elasticclaw/pkg/config"
	"github.com/elasticclaw/elasticclaw/pkg/hub"
	"github.com/elasticclaw/elasticclaw/pkg/types"
	"github.com/spf13/cobra"
)

var (
	createName string
	createEnvs []string
)

var createCmd = &cobra.Command{
	Use:   "create",
	Short: "Create a new claw",
	Long: `Resolve a template and provision a new claw via the hub.

With --source auto (default), the template is looked up in order:
  ./.elasticclaw/templates/<name>/       (repo-local)
  ~/.elasticclaw/templates/<name>/       (global cache)
  public registry                         (downloaded into the cache)
  hub-pushed templates                    (via 'elasticclaw template push')

Use --source local to restrict resolution to the filesystem/registry, or
--source hub to force using a hub-pushed template (useful when a local
template shares a name with a hub-pushed one).

The resolved template must contain elasticclaw-config.yaml specifying the provider.

Example:
  elasticclaw create --name support-01 --template support
  elasticclaw create --name dev-01 --template dev --env GITHUB_TOKEN=xxx
  elasticclaw create --name pinned --template support --source hub`,
	RunE: runCreate,
}

var (
	createTemplate     string
	createSource       string
	createInstanceType string
	createTTL          string
	createTags         []string
)

func init() {
	rootCmd.AddCommand(createCmd)
	createCmd.Flags().StringVarP(&createName, "name", "n", "", "claw name (required)")
	createCmd.MarkFlagRequired("name")
	createCmd.Flags().StringVarP(&createTemplate, "template", "t", "", "template name (required)")
	createCmd.MarkFlagRequired("template")
	createCmd.Flags().StringVar(&createSource, "source", "auto", "template source: auto (local, then hub), local, or hub")
	createCmd.Flags().StringArrayVar(&createEnvs, "env", nil, "extra env vars to inject (KEY=value)")
	createCmd.Flags().StringVar(&createInstanceType, "instance-type", "", "override instance type (e.g. r1.small for Replicated)")
	createCmd.Flags().StringVar(&createTTL, "ttl", "", "override TTL (e.g. 2h, 24h)")
	createCmd.Flags().StringArrayVar(&createTags, "tag", nil, "tag to apply (repeatable: --tag nix --tag env:prod)")
}

func runCreate(cmd *cobra.Command, args []string) error {
	if createTemplate == "" {
		return fmt.Errorf("--template is required (e.g. --template canio)")
	}
	// Resolve hub connection from profile
	h, _, err := config.ResolveHub(profile)
	if err != nil {
		return err
	}
	client := hub.NewClient(h.URL, h.Token)

	// Resolve template based on --source (auto|local|hub).
	var (
		tmplCfg        *types.TemplateConfig
		files          map[string]string
		resolvedSource string
	)
	switch createSource {
	case "auto", "local":
		templateDir, resolveErr := config.ResolveTemplate(createTemplate)
		if resolveErr == nil {
			fmt.Printf("Using template: %s\n", templateDir)
			if data, err := os.ReadFile(templateDir + "/elasticclaw-config.yaml"); err == nil {
				fmt.Printf("  Config:\n%s\n", string(data))
			}
			tmplCfg, err = config.LoadTemplateConfig(templateDir)
			if err != nil {
				return err
			}
			files, err = config.ReadTemplateFiles(templateDir)
			if err != nil {
				return err
			}
			resolvedSource = "local"
			break
		}
		if createSource == "local" {
			return fmt.Errorf("template %q not found locally: %w (use --source hub to pull from hub)", createTemplate, resolveErr)
		}
		// auto: fall through to hub
		tmplCfg, files, err = loadHubTemplate(client, createTemplate)
		if err != nil {
			return fmt.Errorf("%w; hub lookup also failed: %v", resolveErr, err)
		}
		resolvedSource = "hub"
	case "hub":
		tmplCfg, files, err = loadHubTemplate(client, createTemplate)
		if err != nil {
			return err
		}
		resolvedSource = "hub"
	default:
		return fmt.Errorf("invalid --source %q: must be auto, local, or hub", createSource)
	}
	fmt.Printf("Template files: %d files, provider: %s\n", len(files), tmplCfg.Provider)
	if tmplCfg.Nix {
		fmt.Println("  Nix: enabled (Determinate Systems)")
	}
	if tmplCfg.GitHub != nil && len(tmplCfg.GitHub.Repos) > 0 {
		fmt.Printf("  GitHub repos: %d\n", len(tmplCfg.GitHub.Repos))
	}
	// Merge tags: auto (template:<name>, source:<local|hub>) + config yaml tags + --tag flags
	tmplCfg.Tags = mergeCLITags(createTemplate, resolvedSource, tmplCfg.Tags, createTags)
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

func loadHubTemplate(client *hub.Client, name string) (*types.TemplateConfig, map[string]string, error) {
	hubFiles, err := client.GetHubTemplate(context.Background(), name)
	if err != nil {
		return nil, nil, err
	}
	fmt.Printf("Using template: hub:%s\n", name)
	configYAML, ok := hubFiles["elasticclaw-config.yaml"]
	if !ok {
		return nil, nil, fmt.Errorf("hub template %q is missing elasticclaw-config.yaml", name)
	}
	fmt.Printf("  Config:\n%s\n", configYAML)
	tmplCfg, err := config.ParseTemplateConfig([]byte(configYAML))
	if err != nil {
		return nil, nil, err
	}
	if tmplCfg == nil || tmplCfg.Provider == "" {
		return nil, nil, fmt.Errorf("hub template %q has no provider in elasticclaw-config.yaml", name)
	}
	delete(hubFiles, "elasticclaw-config.yaml")
	return tmplCfg, hubFiles, nil
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

func mergeCLITags(templateName, source string, configTags, cliTags []string) []string {
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
	if source != "" {
		add("source:" + source)
	}
	for _, t := range configTags {
		add(t)
	}
	for _, t := range cliTags {
		add(t)
	}
	return result
}
