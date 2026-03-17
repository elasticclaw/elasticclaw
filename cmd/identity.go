package cmd

import (
	"fmt"
	"strings"

	"github.com/elasticclaw/elasticclaw/pkg/config"
	"github.com/spf13/cobra"
	"gopkg.in/yaml.v3"
)

var (
	identityResolveProfile string
	identityResolveBinding string
)

var identityCmd = &cobra.Command{
	Use:   "identity",
	Short: "Identity management commands",
}

var identityResolveCmd = &cobra.Command{
	Use:   "resolve",
	Short: "Show resolved identity configuration",
	Long: `Show the final identity configuration after applying:
  - template defaults
  - profile defaults  
  - explicit CLI overrides

This is useful for debugging identity binding issues.`,
	RunE: runIdentityResolve,
}

func init() {
	rootCmd.AddCommand(identityCmd)
	identityCmd.AddCommand(identityResolveCmd)

	identityResolveCmd.Flags().StringVar(&identityResolveProfile, "identity-profile", "default", "identity profile to resolve")
	identityResolveCmd.Flags().StringVar(&identityResolveBinding, "identity", "", "explicit identity binding to use")
}

func runIdentityResolve(cmd *cobra.Command, args []string) error {
	// Load manifest
	manifest, err := config.LoadManifest()
	if err != nil {
		return err
	}

	// Load active profile
	activeProfile, err := config.GetActiveProfile()
	if err != nil {
		return fmt.Errorf("failed to get profile: %w", err)
	}

	fmt.Println("Identity Resolution")
	fmt.Println()

	// Show source layers
	fmt.Println("Sources (in precedence order):")
	fmt.Println()

	// 1. CLI flags
	if identityResolveBinding != "" {
		fmt.Printf("  1. CLI flag:      %s\n", identityResolveBinding)
	} else {
		fmt.Println("  1. CLI flag:      (not set)")
	}

	// 2. Profile
	if activeProfile.Identity != "" {
		fmt.Printf("  2. Profile (%s): %s\n", activeProfile.Name, activeProfile.Identity)
	} else {
		fmt.Printf("  2. Profile (%s): (not set)\n", activeProfile.Name)
	}

	// 3. Template
	if profile, ok := manifest.Identity.Profiles[identityResolveProfile]; ok {
		if profile.Creddy != nil {
			fmt.Printf("  3. Template (%s): creddy\n", identityResolveProfile)
		} else if profile.Raw != nil {
			fmt.Printf("  3. Template (%s): raw credentials\n", identityResolveProfile)
		} else {
			fmt.Printf("  3. Template (%s): (empty)\n", identityResolveProfile)
		}
	} else {
		fmt.Printf("  3. Template:      profile %q not found\n", identityResolveProfile)
	}

	fmt.Println()
	fmt.Println("Resolved Identity:")
	fmt.Println()

	// Determine resolved identity
	resolved := struct {
		Source  string      `yaml:"source"`
		Binding string      `yaml:"binding,omitempty"`
		Profile interface{} `yaml:"profile,omitempty"`
	}{}

	if identityResolveBinding != "" {
		resolved.Source = "cli"
		resolved.Binding = identityResolveBinding
	} else if activeProfile.Identity != "" {
		resolved.Source = "profile"
		resolved.Binding = activeProfile.Identity
	} else if profile, ok := manifest.Identity.Profiles[identityResolveProfile]; ok {
		resolved.Source = "template"
		resolved.Profile = profile
	} else {
		fmt.Println("  No identity configured")
		return nil
	}

	if jsonOut {
		data, _ := yaml.Marshal(resolved)
		fmt.Println(string(data))
	} else {
		fmt.Printf("  Source:  %s\n", resolved.Source)
		if resolved.Binding != "" {
			fmt.Printf("  Binding: %s\n", resolved.Binding)
		}
		if resolved.Profile != nil {
			fmt.Println("  Config:")
			data, _ := yaml.Marshal(resolved.Profile)
			for _, line := range strings.Split(string(data), "\n") {
				if line != "" {
					fmt.Printf("    %s\n", line)
				}
			}
		}
	}

	return nil
}
