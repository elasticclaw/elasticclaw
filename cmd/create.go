package cmd

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/elasticclaw/elasticclaw/pkg/config"
	"github.com/elasticclaw/elasticclaw/pkg/provider/daytona"
	"github.com/elasticclaw/elasticclaw/pkg/state"
	"github.com/elasticclaw/elasticclaw/pkg/types"
	"github.com/spf13/cobra"
)

var (
	createName            string
	createProvider        string
	createIdentity        string
	createIdentityProfile string
	createState           string
	createTTL             string
	createVars            []string
	createEnvs            []string
	createImage           string
	createDetach          bool
)

var createCmd = &cobra.Command{
	Use:   "create",
	Short: "Create a new instance",
	Long: `Create a new ElasticClaw instance from the initialized template.

Requires 'elasticclaw init' to have been run first.

Example:
  elasticclaw create --name support-01 --provider daytona
  elasticclaw create --name support-01 --var customer=acme`,
	RunE: runCreate,
}

func init() {
	rootCmd.AddCommand(createCmd)

	createCmd.Flags().StringVarP(&createName, "name", "n", "", "instance name (required)")
	createCmd.MarkFlagRequired("name")
	createCmd.Flags().StringVarP(&createProvider, "provider", "p", "", "provider to use (default from profile)")
	createCmd.Flags().StringVar(&createIdentity, "identity", "", "identity binding (e.g., creddy://acme/support)")
	createCmd.Flags().StringVar(&createIdentityProfile, "identity-profile", "default", "identity profile from template")
	createCmd.Flags().StringVar(&createState, "state", "", "state backend (default: local)")
	createCmd.Flags().StringVar(&createTTL, "ttl", "", "instance time-to-live (e.g., 4h, 24h)")
	createCmd.Flags().StringArrayVar(&createVars, "var", nil, "template variable (key=value)")
	createCmd.Flags().StringArrayVar(&createEnvs, "env", nil, "environment variable (KEY=value)")
	createCmd.Flags().StringVar(&createImage, "image", "", "base image to use")
	createCmd.Flags().BoolVar(&createDetach, "detach", false, "don't wait for instance to be ready")
}

func runCreate(cmd *cobra.Command, args []string) error {
	// Check if initialized
	if !config.IsInitialized() {
		return fmt.Errorf("not initialized - run 'elasticclaw init' first")
	}

	// Load manifest
	manifest, err := config.LoadManifest()
	if err != nil {
		return err
	}

	// Load profile for defaults
	activeProfile, err := config.GetActiveProfile()
	if err != nil {
		return fmt.Errorf("failed to get profile: %w", err)
	}

	// Determine provider
	provider := createProvider
	if provider == "" {
		provider = activeProfile.Provider
	}
	if provider == "" && len(manifest.Providers.Supported) > 0 {
		provider = manifest.Providers.Supported[0]
	}
	if provider == "" {
		provider = "daytona"
	}

	// Parse variables
	vars := make(map[string]string)
	for _, v := range createVars {
		parts := strings.SplitN(v, "=", 2)
		if len(parts) != 2 {
			return fmt.Errorf("invalid variable format: %s (expected key=value)", v)
		}
		vars[parts[0]] = parts[1]
	}

	// Parse environment variables
	envs := make(map[string]string)
	for _, e := range createEnvs {
		parts := strings.SplitN(e, "=", 2)
		if len(parts) != 2 {
			return fmt.Errorf("invalid env format: %s (expected KEY=value)", e)
		}
		envs[parts[0]] = parts[1]
	}

	// Check required template variables
	for _, required := range manifest.TemplateVars {
		if _, ok := vars[required]; !ok {
			return fmt.Errorf("missing required template variable: %s", required)
		}
	}

	// Load template files
	templateFiles, err := loadTemplateFiles()
	if err != nil {
		return fmt.Errorf("failed to load template files: %w", err)
	}

	fmt.Printf("Creating instance %s on %s...\n", createName, provider)

	// Create instance record
	instance := state.CreateInstance(createName, provider, manifest.Name, manifest.Version, vars)

	// Store initial state
	store, err := state.NewStore()
	if err != nil {
		return fmt.Errorf("failed to initialize state store: %w", err)
	}

	if err := store.Save(instance); err != nil {
		return fmt.Errorf("failed to save instance state: %w", err)
	}

	// Create via provider
	ctx := context.Background()
	var providerInstance *types.Instance

	switch provider {
	case "daytona":
		p := daytona.New(nil)
		req := types.CreateRequest{
			Name:          createName,
			FromImage:     createImage,
			TemplateFiles: templateFiles,
			Env:           envs,
		}
		providerInstance, err = p.Create(ctx, req)
		if err != nil {
			store.Delete(createName)
			return fmt.Errorf("provider create failed: %w", err)
		}
	default:
		store.Delete(createName)
		return fmt.Errorf("unsupported provider: %s", provider)
	}

	// Update instance with provider info
	instance.Status = providerInstance.Status
	instance.ProviderMeta = providerInstance.ProviderMeta
	if err := store.Save(instance); err != nil {
		return fmt.Errorf("failed to update instance state: %w", err)
	}

	// Print success output
	fmt.Println()
	fmt.Println("✓ Instance created")
	fmt.Println()
	fmt.Printf("  Name:      %s\n", instance.Name)
	fmt.Printf("  Provider:  %s\n", instance.Provider)
	if workspaceID, ok := instance.ProviderMeta["workspace_id"]; ok {
		fmt.Printf("  Workspace: %s\n", workspaceID)
	}
	fmt.Printf("  Template:  %s@%s\n", instance.Template, instance.TemplateVersion)
	if createIdentityProfile != "" {
		fmt.Printf("  Identity:  %s\n", createIdentityProfile)
	}
	fmt.Printf("  Status:    %s\n", instance.Status)
	fmt.Println()
	fmt.Printf("  Chat:      elasticclaw chat %s\n", instance.Name)
	fmt.Printf("  Connect:   daytona ssh %s\n", instance.Name)

	return nil
}

func loadTemplateFiles() (map[string][]byte, error) {
	paths, err := config.GetPaths()
	if err != nil {
		return nil, err
	}

	files := make(map[string][]byte)

	// OpenClaw workspace files to include
	workspaceFiles := []string{
		"AGENTS.md",
		"SOUL.md",
		"TOOLS.md",
		"IDENTITY.md",
		"USER.md",
		"MEMORY.md",
		"BOOTSTRAP.md",
	}

	// Look in template dir first, then current dir
	baseDir := paths.TemplateDir
	if _, err := os.Stat(baseDir); os.IsNotExist(err) {
		baseDir = "."
	}

	for _, filename := range workspaceFiles {
		path := filepath.Join(baseDir, filename)
		if _, err := os.Stat(path); os.IsNotExist(err) {
			// Try current dir
			path = filename
			if _, err := os.Stat(path); os.IsNotExist(err) {
				continue // Skip missing optional files
			}
		}

		content, err := os.ReadFile(path)
		if err != nil {
			return nil, fmt.Errorf("failed to read %s: %w", filename, err)
		}
		files[filename] = content
	}

	// Include memory directory if exists
	memoryDir := filepath.Join(baseDir, "memory")
	if _, err := os.Stat(memoryDir); err == nil {
		filepath.Walk(memoryDir, func(path string, info os.FileInfo, err error) error {
			if err != nil || info.IsDir() {
				return nil
			}
			relPath, _ := filepath.Rel(baseDir, path)
			content, err := os.ReadFile(path)
			if err != nil {
				return nil
			}
			files[relPath] = content
			return nil
		})
	}

	return files, nil
}
