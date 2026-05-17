package cmd

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/elasticclaw/elasticclaw/pkg/config"
	"github.com/elasticclaw/elasticclaw/pkg/hub"
	"github.com/spf13/cobra"
	"gopkg.in/yaml.v3"
)

var templateCmd = &cobra.Command{
	Use:   "template",
	Short: "Template management commands",
}

var templateCreateCmd = &cobra.Command{
	Use:   "create <name>",
	Short: "Scaffold a new template in .elasticclaw/templates/<name>/",
	Long: `Create a new ElasticClaw template in the current repo.

The template is created at .elasticclaw/templates/<name>/ and contains:
  - elasticclaw-config.yaml   provider, instance type, TTL
  - AGENTS.md                 agent instructions and workspace config
  - SOUL.md                   agent personality and values
  - TOOLS.md                  environment-specific notes
  - IDENTITY.md               agent name and metadata
  - USER.md                   about the human
  - MEMORY.md                 long-term memory (pre-seeded if desired)

Example:
  elasticclaw template create support
  elasticclaw template create dev --provider replicated --instance-type r1.small`,
	Args: cobra.ExactArgs(1),
	RunE: runTemplateCreate,
}

var templateListCmd = &cobra.Command{
	Use:     "list",
	Aliases: []string{"ls"},
	Short:   "List available templates",
	RunE:    runTemplateList,
}

var (
	templateCreateProvider     string
	templateCreateInstanceType string
	templateCreateTTL          string
)

func init() {
	rootCmd.AddCommand(templateCmd)
	templateCmd.AddCommand(templateCreateCmd)
	templateCmd.AddCommand(templateListCmd)

	templateCreateCmd.Flags().StringVar(&templateCreateProvider, "provider", "replicated", "provider to use (replicated, daytona, exedev)")
	templateCreateCmd.Flags().StringVar(&templateCreateInstanceType, "instance-type", "r1.large", "instance type (e.g. r1.small, r1.large)")
	templateCreateCmd.Flags().StringVar(&templateCreateTTL, "ttl", "48h", "time-to-live for the VM (e.g. 4h, 24h, 48h)")
}

func runTemplateCreate(cmd *cobra.Command, args []string) error {
	name := args[0]

	// Resolve destination: .elasticclaw/templates/<name>/
	cwd, err := os.Getwd()
	if err != nil {
		return err
	}
	dest := filepath.Join(cwd, ".elasticclaw", "templates", name)

	if _, err := os.Stat(dest); err == nil {
		return fmt.Errorf("template %q already exists at %s", name, dest)
	}
	if err := os.MkdirAll(dest, 0755); err != nil {
		return fmt.Errorf("create template dir: %w", err)
	}

	title := strings.Title(strings.ReplaceAll(name, "-", " "))

	files := map[string]string{
		"elasticclaw-config.yaml": fmt.Sprintf(`# ElasticClaw template configuration
provider: %s
instance_type: %s
ttl: %s
`, templateCreateProvider, templateCreateInstanceType, templateCreateTTL),

		"AGENTS.md": fmt.Sprintf(`# AGENTS.md - Your Workspace

## First Run

If BOOTSTRAP.md exists, follow it, then delete it.

## Every Session

1. Read SOUL.md
2. Read USER.md
3. Read memory/YYYY-MM-DD.md (today) for recent context

## Memory

- Daily notes: memory/YYYY-MM-DD.md
- Long-term: MEMORY.md

## What You Are

%s agent. [Describe the agent's purpose here.]

## How to Work

[Describe operating procedures, what tools to use, how to handle tasks.]
`, title),

		"SOUL.md": fmt.Sprintf(`# SOUL.md - Who You Are

You are %s.

## Core traits

- [Describe personality]
- [Describe values]
- [Describe communication style]

## Boundaries

- Don't share private data
- Ask before external actions
- When uncertain, ask

## Style

[How should responses be formatted? What tone?]
`, title),

		"TOOLS.md": `# TOOLS.md - Environment Notes

## Setup

[Document any environment-specific setup, credentials, tool locations]

## Tools

[Notes on tools available in this environment]
`,

		"IDENTITY.md": fmt.Sprintf(`# IDENTITY.md - Who Am I?

- **Name:** %s
- **Role:** [describe role]
- **Emoji:** 🤖
`, title),

		"USER.md": `# USER.md - About Your Human

- **Name:** [Name]
- **Timezone:** [e.g. America/Chicago]
- **Notes:** [Preferences, working style, context]
`,

		"MEMORY.md": `# MEMORY.md - Long-Term Memory

[Pre-seed with important context the agent should always have]
`,
	}

	for filename, content := range files {
		path := filepath.Join(dest, filename)
		if err := os.WriteFile(path, []byte(content), 0644); err != nil {
			return fmt.Errorf("write %s: %w", filename, err)
		}
		fmt.Printf("  ✓ %s\n", filename)
	}

	// Create empty memory/ dir
	memDir := filepath.Join(dest, "memory")
	if err := os.MkdirAll(memDir, 0755); err != nil {
		return fmt.Errorf("create memory dir: %w", err)
	}
	fmt.Println("  ✓ memory/")

	fmt.Println()
	fmt.Printf("✓ Template %q created at .elasticclaw/templates/%s/\n", name, name)
	fmt.Println()
	fmt.Println("Next steps:")
	fmt.Printf("  1. Edit .elasticclaw/templates/%s/SOUL.md and AGENTS.md\n", name)
	fmt.Printf("  2. elasticclaw create --name my-claw --template %s\n", name)

	return nil
}

func runTemplateList(cmd *cobra.Command, args []string) error {
	cwd, err := os.Getwd()
	if err != nil {
		return err
	}

	type templateEntry struct {
		name     string
		dir      string
		provider string
	}

	var found []templateEntry

	// Check repo-local templates
	localDir := filepath.Join(cwd, ".elasticclaw", "templates")
	if entries, err := os.ReadDir(localDir); err == nil {
		for _, e := range entries {
			if !e.IsDir() {
				continue
			}
			provider := providerFromConfig(filepath.Join(localDir, e.Name()))
			found = append(found, templateEntry{
				name:     e.Name(),
				dir:      ".elasticclaw/templates/" + e.Name(),
				provider: provider,
			})
		}
	}

	// Check global templates
	if home, err := os.UserHomeDir(); err == nil {
		globalDir := filepath.Join(home, ".elasticclaw", "templates")
		if entries, err := os.ReadDir(globalDir); err == nil {
			for _, e := range entries {
				if !e.IsDir() {
					continue
				}
				provider := providerFromConfig(filepath.Join(globalDir, e.Name()))
				found = append(found, templateEntry{
					name:     e.Name(),
					dir:      "~/.elasticclaw/templates/" + e.Name(),
					provider: provider,
				})
			}
		}
	}

	// Check hub-stored templates
	var hubTemplates []string
	if h, _, err := config.ResolveHub(profile); err == nil {
		client := hub.NewClient(h.URL, h.Token)
		if hubTmpls, err := client.ListHubTemplates(context.Background()); err == nil {
			for _, t := range hubTmpls {
				if name, ok := t["name"]; ok {
					hubTemplates = append(hubTemplates, name)
				}
			}
		}
	}

	if len(found) == 0 && len(hubTemplates) == 0 {
		fmt.Println("No templates found.")
		fmt.Println()
		fmt.Println("Create one with:")
		fmt.Println("  elasticclaw template create <name>")
		fmt.Println("Push to hub with:")
		fmt.Println("  elasticclaw template push <name>")
		return nil
	}

	if len(found) > 0 {
		fmt.Println("Local templates:")
		fmt.Printf("  %-20s  %-12s  %s\n", "NAME", "PROVIDER", "LOCATION")
		for _, t := range found {
			fmt.Printf("  %-20s  %-12s  %s\n", t.name, t.provider, t.dir)
		}
	}

	if len(hubTemplates) > 0 {
		if len(found) > 0 {
			fmt.Println()
		}
		fmt.Println("Hub templates (available for factories):")
		for _, name := range hubTemplates {
			fmt.Printf("  %-20s  hub\n", name)
		}
	}
	return nil
}

func providerFromConfig(templateDir string) string {
	data, err := os.ReadFile(filepath.Join(templateDir, "elasticclaw-config.yaml"))
	if err != nil {
		return "unknown"
	}
	var cfg struct {
		Provider string `yaml:"provider"`
	}
	if err := yaml.Unmarshal(data, &cfg); err != nil || cfg.Provider == "" {
		return "unknown"
	}
	return cfg.Provider
}
