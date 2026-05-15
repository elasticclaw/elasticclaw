package cmd

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/elasticclaw/elasticclaw/pkg/config"
	"github.com/elasticclaw/elasticclaw/pkg/hub"
	"github.com/spf13/cobra"
)

var templatePushCmd = &cobra.Command{
	Use:   "push [name-or-path]",
	Short: "Push a template to the hub",
	Long: `Push a template to the connected hub so it can be used by factories and remote claw creation.

Push a local template by name:
  elasticclaw template push elasticclaw-dev

Push a template from a specific directory:
  elasticclaw template push ./my-template

Push a template from the public registry to the hub:
  elasticclaw template push base`,
	Args: cobra.ExactArgs(1),
	RunE: runTemplatePush,
}

func init() {
	templateCmd.AddCommand(templatePushCmd)
}

func runTemplatePush(cmd *cobra.Command, args []string) error {
	nameOrPath := args[0]

	// Resolve template directory
	var templateDir string
	var templateName string

	// Check if it looks like a path
	if strings.HasPrefix(nameOrPath, "./") || strings.HasPrefix(nameOrPath, "/") || strings.HasPrefix(nameOrPath, "../") {
		// Explicit path
		abs, err := filepath.Abs(nameOrPath)
		if err != nil {
			return fmt.Errorf("invalid path: %w", err)
		}
		templateDir = abs
		templateName = filepath.Base(abs)
	} else {
		// Name — resolve via normal template resolution (local, then registry)
		templateName = nameOrPath
		dir, err := config.ResolveTemplate(nameOrPath)
		if err != nil {
			return fmt.Errorf("template %q not found: %w\nUse 'elasticclaw template list' to see available templates", nameOrPath, err)
		}
		templateDir = dir
	}

	// Read template files
	fsFiles, err := config.ReadTemplateFiles(templateDir)
	if err != nil {
		return fmt.Errorf("failed to read template: %w", err)
	}

	// Also include the config yaml
	configPath := filepath.Join(templateDir, "elasticclaw-config.yaml")
	if data, err := os.ReadFile(configPath); err == nil {
		fsFiles["elasticclaw-config.yaml"] = string(data)
	}

	if len(fsFiles) == 0 {
		return fmt.Errorf("template directory is empty")
	}

	// Validate template configuration before pushing
	if configData, ok := fsFiles["elasticclaw-config.yaml"]; ok {
		cfg, err := config.ParseTemplateConfig([]byte(configData))
		if err != nil {
			return fmt.Errorf("validation failed: invalid elasticclaw-config.yaml: %w", err)
		}
		if cfg != nil {
			if err := cfg.Validate(); err != nil {
				return fmt.Errorf("validation failed: %w", err)
			}
		}
	} else {
		return fmt.Errorf("validation failed: elasticclaw-config.yaml is required")
	}

	// Push to hub
	h, _, err := config.ResolveHub(profile)
	if err != nil {
		return err
	}
	client := hub.NewClient(h.URL, h.Token)
	if err := client.PushTemplate(templateName, fsFiles); err != nil {
		return fmt.Errorf("push failed: %w", err)
	}

	fmt.Printf("✓ Template %q pushed to hub (%d files)\n", templateName, len(fsFiles))
	return nil
}
