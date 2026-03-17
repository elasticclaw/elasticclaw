package cmd

import (
	"fmt"
	"os"
	"path/filepath"

	"github.com/elasticclaw/elasticclaw/pkg/config"
	"github.com/elasticclaw/elasticclaw/pkg/types"
	"github.com/spf13/cobra"
	"gopkg.in/yaml.v3"
)

var (
	templateNewName     string
	templateNewProvider string
)

var templateCmd = &cobra.Command{
	Use:   "template",
	Short: "Template management commands",
}

var templateNewCmd = &cobra.Command{
	Use:   "new",
	Short: "Scaffold a new template from scratch",
	Long: `Create a new ElasticClaw template with all required files.

Example:
  elasticclaw template new --name support-agent
  elasticclaw template new --name support-agent --provider daytona`,
	RunE: runTemplateNew,
}

var templateValidateCmd = &cobra.Command{
	Use:   "validate [path]",
	Short: "Validate a template",
	Long: `Validate a template's structure and manifest.

Checks:
  - Required files exist
  - Manifest schema is valid
  - Identity profiles are valid
  - Provider compatibility`,
	RunE: runTemplateValidate,
}

func init() {
	rootCmd.AddCommand(templateCmd)
	templateCmd.AddCommand(templateNewCmd)
	templateCmd.AddCommand(templateValidateCmd)

	templateNewCmd.Flags().StringVarP(&templateNewName, "name", "n", "", "template name (required)")
	templateNewCmd.MarkFlagRequired("name")
	templateNewCmd.Flags().StringVar(&templateNewProvider, "provider", "daytona", "default provider")
}

func runTemplateNew(cmd *cobra.Command, args []string) error {
	// Create directory structure
	dirs := []string{"memory"}
	for _, dir := range dirs {
		if err := os.MkdirAll(dir, 0755); err != nil {
			return fmt.Errorf("failed to create directory %s: %w", dir, err)
		}
	}

	// Create manifest
	manifest := &types.Manifest{
		Name:        templateNewName,
		Version:     "0.1.0",
		Description: fmt.Sprintf("%s OpenClaw agent template", templateNewName),
		OpenClaw: types.OpenClawConfig{
			RequiredFiles: []string{
				"AGENTS.md",
				"SOUL.md",
				"TOOLS.md",
				"IDENTITY.md",
				"USER.md",
				"MEMORY.md",
			},
		},
		Providers: types.ProvidersConfig{
			Supported: []string{templateNewProvider},
		},
		Identity: types.IdentityConfig{
			Profiles: map[string]types.IdentityProfile{
				"default": {
					Creddy: &types.CreddyConfig{
						Bindings: []types.CredentialBinding{
							{
								Audience: "github",
								Scopes:   []string{"repo:read"},
								TTL:      "4h",
							},
						},
					},
				},
			},
		},
		State: types.StateConfig{
			Default: "local",
			PromotableTargets: []string{
				"MEMORY.md",
				"TOOLS.md",
			},
		},
	}

	if err := config.SaveManifest(manifest); err != nil {
		return err
	}
	fmt.Println("✓ Created elasticclaw.yaml")

	// Create OpenClaw files
	files := map[string]string{
		"AGENTS.md": fmt.Sprintf(`# AGENTS.md - %s

## Overview

This is the %s agent workspace.

## Instructions

[Add agent operating instructions here]

## Memory

- Daily notes: memory/YYYY-MM-DD.md
- Long-term: MEMORY.md
`, templateNewName, templateNewName),

		"SOUL.md": fmt.Sprintf(`# SOUL.md - Who You Are

## Identity

You are %s, an AI assistant.

## Core Values

- Be helpful and direct
- Be honest about limitations
- Respect privacy and security

## Boundaries

- Don't share private information
- Ask before taking external actions
- When in doubt, ask

## Style

- Be concise but thorough
- Use clear language
- Stay focused on the task
`, templateNewName),

		"TOOLS.md": `# TOOLS.md - Local Notes

This file contains environment-specific notes and tool configurations.

## Environment

[Add environment-specific notes here]

## Tools

[Document tool configurations and usage notes]
`,

		"IDENTITY.md": fmt.Sprintf(`# IDENTITY.md - Who Am I?

- **Name:** %s
- **Role:** AI Assistant
- **Emoji:** 🤖
`, templateNewName),

		"USER.md": `# USER.md - About Your Human

- **Name:** [User name]
- **Timezone:** [Timezone]
- **Notes:** [Preferences and context]
`,

		"MEMORY.md": `# MEMORY.md - Long-Term Memory

## Key Information

[Important context that persists across sessions]

## Lessons Learned

[Things discovered that should be remembered]
`,

		".elasticclawignore": `# ElasticClaw ignore file
# Files matching these patterns won't be included in the template

.git/
.elasticclaw/
*.log
*.tmp
.DS_Store
`,

		".gitignore": `# ElasticClaw
.elasticclaw/

# OS
.DS_Store

# Editor
*.swp
*.swo
.idea/
.vscode/
`,
	}

	for filename, content := range files {
		if err := os.WriteFile(filename, []byte(content), 0644); err != nil {
			return fmt.Errorf("failed to create %s: %w", filename, err)
		}
		fmt.Printf("✓ Created %s\n", filename)
	}

	// Create empty memory/.gitkeep
	if err := os.WriteFile("memory/.gitkeep", []byte(""), 0644); err != nil {
		return fmt.Errorf("failed to create memory/.gitkeep: %w", err)
	}
	fmt.Println("✓ Created memory/")

	fmt.Println()
	fmt.Printf("Template %s created successfully!\n", templateNewName)
	fmt.Println()
	fmt.Println("Next steps:")
	fmt.Println("  1. Edit the template files to customize your agent")
	fmt.Println("  2. Run: elasticclaw init")
	fmt.Println("  3. Run: elasticclaw create --name <instance-name>")

	return nil
}

func runTemplateValidate(cmd *cobra.Command, args []string) error {
	path := "."
	if len(args) > 0 {
		path = args[0]
	}

	manifestPath := filepath.Join(path, "elasticclaw.yaml")

	// Load manifest
	data, err := os.ReadFile(manifestPath)
	if err != nil {
		if os.IsNotExist(err) {
			return fmt.Errorf("elasticclaw.yaml not found in %s", path)
		}
		return fmt.Errorf("failed to read manifest: %w", err)
	}

	var manifest types.Manifest
	if err := yaml.Unmarshal(data, &manifest); err != nil {
		return fmt.Errorf("invalid manifest YAML: %w", err)
	}

	fmt.Printf("Validating template: %s v%s\n", manifest.Name, manifest.Version)
	fmt.Println()

	errors := []string{}
	warnings := []string{}

	// Check required fields
	if manifest.Name == "" {
		errors = append(errors, "manifest missing 'name' field")
	}
	if manifest.Version == "" {
		warnings = append(warnings, "manifest missing 'version' field")
	}

	// Check required files
	requiredFiles := manifest.OpenClaw.RequiredFiles
	if len(requiredFiles) == 0 {
		requiredFiles = []string{"AGENTS.md", "SOUL.md"}
	}

	for _, file := range requiredFiles {
		filePath := filepath.Join(path, file)
		if _, err := os.Stat(filePath); os.IsNotExist(err) {
			errors = append(errors, fmt.Sprintf("missing required file: %s", file))
		}
	}

	// Check identity profiles
	for name, profile := range manifest.Identity.Profiles {
		if profile.Creddy == nil && profile.Raw == nil {
			warnings = append(warnings, fmt.Sprintf("identity profile %q has no creddy or raw config", name))
		}
		if profile.Creddy != nil {
			for i, binding := range profile.Creddy.Bindings {
				if binding.Audience == "" {
					errors = append(errors, fmt.Sprintf("identity profile %q binding %d missing audience", name, i))
				}
			}
		}
	}

	// Check providers
	if len(manifest.Providers.Supported) == 0 {
		warnings = append(warnings, "no supported providers specified")
	}

	// Print results
	if len(errors) > 0 {
		fmt.Println("Errors:")
		for _, e := range errors {
			fmt.Printf("  ✗ %s\n", e)
		}
		fmt.Println()
	}

	if len(warnings) > 0 {
		fmt.Println("Warnings:")
		for _, w := range warnings {
			fmt.Printf("  ⚠ %s\n", w)
		}
		fmt.Println()
	}

	if len(errors) == 0 {
		fmt.Println("✓ Template is valid")
		if len(warnings) > 0 {
			fmt.Printf("  (%d warnings)\n", len(warnings))
		}
		return nil
	}

	return fmt.Errorf("validation failed with %d errors", len(errors))
}
