package cmd

import (
	"fmt"
	"os"
	"text/tabwriter"

	"github.com/elasticclaw/elasticclaw/pkg/config"
	"github.com/elasticclaw/elasticclaw/pkg/types"
	"github.com/spf13/cobra"
	"gopkg.in/yaml.v3"
)

var (
	profileFrom      string
	profileProvider  string
	profileState     string
	profileIdentity  string
	profileNamespace string
	profileSetActive bool
)

var profileCmd = &cobra.Command{
	Use:   "profile",
	Short: "Manage profiles",
	Long:  "Profiles are named execution contexts containing defaults for provider, state, identity, and namespace.",
}

var profileLsCmd = &cobra.Command{
	Use:     "ls",
	Aliases: []string{"list"},
	Short:   "List profiles",
	RunE:    runProfileLs,
}

var profileCreateCmd = &cobra.Command{
	Use:   "create <name>",
	Short: "Create a new profile",
	Args:  cobra.ExactArgs(1),
	RunE:  runProfileCreate,
}

var profileUseCmd = &cobra.Command{
	Use:   "use <name>",
	Short: "Set the active profile",
	Args:  cobra.ExactArgs(1),
	RunE:  runProfileUse,
}

var profileShowCmd = &cobra.Command{
	Use:   "show [name]",
	Short: "Show profile details",
	Args:  cobra.MaximumNArgs(1),
	RunE:  runProfileShow,
}

var profileDeleteCmd = &cobra.Command{
	Use:     "delete <name>",
	Aliases: []string{"rm"},
	Short:   "Delete a profile",
	Args:    cobra.ExactArgs(1),
	RunE:    runProfileDelete,
}

var profileCurrentCmd = &cobra.Command{
	Use:   "current",
	Short: "Show the current active profile",
	RunE:  runProfileCurrent,
}

func init() {
	rootCmd.AddCommand(profileCmd)
	profileCmd.AddCommand(profileLsCmd)
	profileCmd.AddCommand(profileCreateCmd)
	profileCmd.AddCommand(profileUseCmd)
	profileCmd.AddCommand(profileShowCmd)
	profileCmd.AddCommand(profileDeleteCmd)
	profileCmd.AddCommand(profileCurrentCmd)

	profileCreateCmd.Flags().StringVar(&profileFrom, "from", "", "copy settings from existing profile")
	profileCreateCmd.Flags().StringVar(&profileProvider, "provider", "", "default provider")
	profileCreateCmd.Flags().StringVar(&profileState, "state", "", "default state backend")
	profileCreateCmd.Flags().StringVar(&profileIdentity, "identity", "", "default identity")
	profileCreateCmd.Flags().StringVar(&profileNamespace, "namespace", "", "namespace")
	profileCreateCmd.Flags().BoolVar(&profileSetActive, "set-active", false, "set as active profile")
}

func runProfileLs(cmd *cobra.Command, args []string) error {
	profiles, err := config.ListProfiles()
	if err != nil {
		return err
	}

	cfg, err := config.LoadGlobalConfig()
	if err != nil {
		return err
	}

	if len(profiles) == 0 {
		fmt.Println("No profiles configured.")
		fmt.Println()
		fmt.Println("Create one with:")
		fmt.Println("  elasticclaw profile create <name> --provider daytona")
		return nil
	}

	if jsonOut {
		data, _ := yaml.Marshal(profiles)
		fmt.Println(string(data))
		return nil
	}

	w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintln(w, "NAME\tACTIVE\tPROVIDER\tSTATE\tIDENTITY")

	for _, p := range profiles {
		active := ""
		if p.Name == cfg.ActiveProfile {
			active = "*"
		}
		identity := p.Identity
		if identity == "" {
			identity = "-"
		}
		state := p.State
		if state == "" {
			state = "local"
		}
		provider := p.Provider
		if provider == "" {
			provider = "-"
		}
		fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\n", p.Name, active, provider, state, identity)
	}
	w.Flush()

	return nil
}

func runProfileCreate(cmd *cobra.Command, args []string) error {
	name := args[0]

	// Check if exists
	existing, _ := config.LoadProfile(name)
	if existing != nil {
		return fmt.Errorf("profile %q already exists", name)
	}

	profile := &types.Profile{Name: name}

	// Copy from existing profile if specified
	if profileFrom != "" {
		from, err := config.LoadProfile(profileFrom)
		if err != nil {
			return fmt.Errorf("failed to load source profile: %w", err)
		}
		profile.Provider = from.Provider
		profile.State = from.State
		profile.Identity = from.Identity
		profile.Namespace = from.Namespace
		profile.Providers = from.Providers
	}

	// Apply overrides
	if profileProvider != "" {
		profile.Provider = profileProvider
	}
	if profileState != "" {
		profile.State = profileState
	}
	if profileIdentity != "" {
		profile.Identity = profileIdentity
	}
	if profileNamespace != "" {
		profile.Namespace = profileNamespace
	}

	if err := config.SaveProfile(profile); err != nil {
		return err
	}

	fmt.Printf("✓ Created profile %q\n", name)

	if profileSetActive {
		cfg, err := config.LoadGlobalConfig()
		if err != nil {
			return err
		}
		cfg.ActiveProfile = name
		if err := config.SaveGlobalConfig(cfg); err != nil {
			return err
		}
		fmt.Printf("✓ Set as active profile\n")
	}

	return nil
}

func runProfileUse(cmd *cobra.Command, args []string) error {
	name := args[0]

	// Verify profile exists
	if _, err := config.LoadProfile(name); err != nil {
		return err
	}

	cfg, err := config.LoadGlobalConfig()
	if err != nil {
		return err
	}

	cfg.ActiveProfile = name
	if err := config.SaveGlobalConfig(cfg); err != nil {
		return err
	}

	fmt.Printf("✓ Now using profile %q\n", name)
	return nil
}

func runProfileShow(cmd *cobra.Command, args []string) error {
	var profile *types.Profile
	var err error

	if len(args) > 0 {
		profile, err = config.LoadProfile(args[0])
	} else {
		profile, err = config.GetActiveProfile()
	}
	if err != nil {
		return err
	}

	if jsonOut {
		data, _ := yaml.Marshal(profile)
		fmt.Println(string(data))
		return nil
	}

	fmt.Printf("Profile: %s\n", profile.Name)
	fmt.Println()
	if profile.Provider != "" {
		fmt.Printf("  Provider:  %s\n", profile.Provider)
	}
	if profile.State != "" {
		fmt.Printf("  State:     %s\n", profile.State)
	}
	if profile.Identity != "" {
		fmt.Printf("  Identity:  %s\n", profile.Identity)
	}
	if profile.Namespace != "" {
		fmt.Printf("  Namespace: %s\n", profile.Namespace)
	}

	return nil
}

func runProfileDelete(cmd *cobra.Command, args []string) error {
	name := args[0]

	if err := config.DeleteProfile(name); err != nil {
		return err
	}

	// Clear active profile if it was deleted
	cfg, err := config.LoadGlobalConfig()
	if err != nil {
		return err
	}
	if cfg.ActiveProfile == name {
		cfg.ActiveProfile = ""
		config.SaveGlobalConfig(cfg)
	}

	fmt.Printf("✓ Deleted profile %q\n", name)
	return nil
}

func runProfileCurrent(cmd *cobra.Command, args []string) error {
	cfg, err := config.LoadGlobalConfig()
	if err != nil {
		return err
	}

	if cfg.ActiveProfile == "" {
		fmt.Println("No active profile set (using defaults)")
		return nil
	}

	fmt.Println(cfg.ActiveProfile)
	return nil
}
