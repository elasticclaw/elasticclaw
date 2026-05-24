package cmd

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/elasticclaw/elasticclaw/pkg/config"
	"github.com/elasticclaw/elasticclaw/pkg/types"
	"github.com/spf13/cobra"
	"gopkg.in/yaml.v3"
)

var (
	initTemplate string
	initUpgrade  bool
)

var initCmd = &cobra.Command{
	Use:   "init",
	Short: "Initialize working directory",
	Long: `Initialize the working directory for ElasticClaw.

This command prepares the working directory for 'elasticclaw create'.

Two paths to init:

  # Path 1: Init in a cloned template repo
  git clone github.com/acme/support-claw
  cd support-claw
  elasticclaw init

  # Path 2: Init from scratch pointing at remote template
  mkdir my-agents && cd my-agents
  elasticclaw init --template github.com/acme/support-claw

What it does:
  1. Pulls/clones template to local cache (.elasticclaw/template/)
  2. Validates template structure
  3. Creates .elasticclaw/ working directory
  4. Writes lock.yaml with pinned versions
  5. Ready for 'elasticclaw create'`,
	RunE: runInit,
}

func init() {
	initCmd.Hidden = true
	initCmd.Deprecated = "template init is deprecated; use workspace create instead"
	rootCmd.AddCommand(initCmd)

	initCmd.Flags().StringVarP(&initTemplate, "template", "t", "", "template source (e.g., github.com/acme/support-claw)")
	initCmd.Flags().BoolVar(&initUpgrade, "upgrade", false, "refresh template to latest version")
}

func runInit(cmd *cobra.Command, args []string) error {
	paths, err := config.GetPaths()
	if err != nil {
		return err
	}

	// Check if already initialized
	if config.IsInitialized() && !initUpgrade {
		fmt.Println("Already initialized. Use --upgrade to refresh template.")
		return nil
	}

	// Determine template source
	templateSource := initTemplate
	if templateSource == "" {
		// Check if elasticclaw.yaml exists in current dir (cloned template repo)
		if _, err := os.Stat(paths.ManifestFile); err == nil {
			templateSource = "."
			fmt.Println("Found elasticclaw.yaml in current directory")
		} else {
			return fmt.Errorf("no template specified and no elasticclaw.yaml found\n\nUsage:\n  elasticclaw init --template github.com/acme/support-claw")
		}
	}

	// Create working directory
	if err := os.MkdirAll(paths.WorkDir, 0755); err != nil {
		return fmt.Errorf("failed to create .elasticclaw directory: %w", err)
	}

	// Handle template
	if templateSource != "." {
		fmt.Printf("Pulling template from %s...\n", templateSource)
		if err := pullTemplate(templateSource, paths.TemplateDir); err != nil {
			return fmt.Errorf("failed to pull template: %w", err)
		}

		// Copy manifest to current dir if not exists
		srcManifest := filepath.Join(paths.TemplateDir, "elasticclaw.yaml")
		if _, err := os.Stat(paths.ManifestFile); os.IsNotExist(err) {
			if err := copyFile(srcManifest, paths.ManifestFile); err != nil {
				return fmt.Errorf("failed to copy manifest: %w", err)
			}
		}
	}

	// Validate manifest
	manifest, err := config.LoadManifest()
	if err != nil {
		return err
	}

	fmt.Printf("Template: %s v%s\n", manifest.Name, manifest.Version)

	// Validate required files
	if err := validateTemplateFiles(manifest, paths); err != nil {
		return err
	}

	// Create lock file
	lock := &types.LockFile{
		Template: types.TemplateLock{
			Source:  templateSource,
			Version: manifest.Version,
		},
		LockedAt:      time.Now().UTC().Format(time.RFC3339),
		SchemaVersion: "v1",
		ToolVersion:   Version,
	}

	if err := config.SaveLockFile(lock); err != nil {
		return fmt.Errorf("failed to write lock file: %w", err)
	}

	// Create state directory
	stateDir := filepath.Join(paths.WorkDir, "state")
	if err := os.MkdirAll(stateDir, 0755); err != nil {
		return fmt.Errorf("failed to create state directory: %w", err)
	}

	fmt.Println()
	fmt.Println("✓ Initialized successfully")
	fmt.Println()
	fmt.Println("Next steps:")
	fmt.Println("  elasticclaw create --name <instance-name> --provider daytona")

	return nil
}

func pullTemplate(source, dest string) error {
	// Clean destination
	os.RemoveAll(dest)

	// Handle GitHub URLs
	if strings.HasPrefix(source, "github.com/") {
		source = "https://" + source
	}

	// Clone the repo
	cmd := exec.Command("git", "clone", "--depth", "1", source, dest)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	return cmd.Run()
}

func validateTemplateFiles(manifest *types.Manifest, paths *config.Paths) error {
	// Default required files if not specified
	requiredFiles := manifest.OpenClaw.RequiredFiles
	if len(requiredFiles) == 0 {
		requiredFiles = []string{
			"AGENTS.md",
			"SOUL.md",
		}
	}

	// Check in template dir first, then current dir
	baseDir := paths.TemplateDir
	if _, err := os.Stat(baseDir); os.IsNotExist(err) {
		baseDir = "."
	}

	var missing []string
	for _, file := range requiredFiles {
		path := filepath.Join(baseDir, file)
		if _, err := os.Stat(path); os.IsNotExist(err) {
			// Also check current dir
			if _, err := os.Stat(file); os.IsNotExist(err) {
				missing = append(missing, file)
			}
		}
	}

	if len(missing) > 0 {
		return fmt.Errorf("missing required files: %s", strings.Join(missing, ", "))
	}

	fmt.Printf("✓ Validated %d required files\n", len(requiredFiles))
	return nil
}

func copyFile(src, dst string) error {
	data, err := os.ReadFile(src)
	if err != nil {
		return err
	}
	return os.WriteFile(dst, data, 0644)
}

// Helper to write YAML
func writeYAML(path string, v interface{}) error {
	data, err := yaml.Marshal(v)
	if err != nil {
		return err
	}
	return os.WriteFile(path, data, 0644)
}
