package cmd

import (
	"context"
	"fmt"
	"sort"
	"text/tabwriter"
	"os"

	"github.com/elasticclaw/elasticclaw/pkg/config"
	"github.com/elasticclaw/elasticclaw/pkg/hub"
	"github.com/spf13/cobra"
)

var profileCmd = &cobra.Command{
	Use:   "profile",
	Short: "Manage hub profiles",
	Long: `Manage ElasticClaw hub profiles.

A profile stores connection details (URL + token) for one hub.
You can have multiple profiles and switch between them.

  elasticclaw profile ls
  elasticclaw profile create work --url https://hub2.example.com --token mytoken
  elasticclaw profile use work
  elasticclaw profile rename work prod
  elasticclaw profile rm work
  elasticclaw profile show`,
}

var profileLsCmd = &cobra.Command{
	Use:     "ls",
	Aliases: []string{"list"},
	Short:   "List all profiles",
	RunE: func(cmd *cobra.Command, args []string) error {
		profiles, active, err := config.ListHubProfiles()
		if err != nil {
			return err
		}
		if len(profiles) == 0 {
			fmt.Println("No profiles configured.")
			fmt.Println()
			fmt.Println("Create one with:")
			fmt.Println("  elasticclaw login --hub <url> --token <token>")
			fmt.Println("  elasticclaw profile create <name> --url <url> --token <token>")
			return nil
		}

		// Sort names for stable output
		names := make([]string, 0, len(profiles))
		for name := range profiles {
			names = append(names, name)
		}
		sort.Strings(names)

		w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
		fmt.Fprintln(w, "  NAME\tHUB URL")
		for _, name := range names {
			p := profiles[name]
			marker := "  "
			if name == active {
				marker = "* "
			}
			fmt.Fprintf(w, "%s%s\t%s\n", marker, name, p.URL)
		}
		w.Flush()
		return nil
	},
}

var (
	profileCreateURL   string
	profileCreateToken string
)

var profileCreateCmd = &cobra.Command{
	Use:   "create <name>",
	Short: "Create a new hub profile",
	Args:  cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		name := args[0]

		// Validate connection before saving
		client := hub.NewClient(profileCreateURL, profileCreateToken)
		if _, err := client.Login(context.Background()); err != nil {
			return fmt.Errorf("could not connect to hub: %w", err)
		}

		if err := config.AddHubProfile(name, profileCreateURL, profileCreateToken); err != nil {
			return err
		}
		fmt.Printf("✓ Profile %q created (%s)\n", name, profileCreateURL)
		fmt.Printf("  Run `elasticclaw profile use %s` to switch to it.\n", name)
		return nil
	},
}

var profileUseCmd = &cobra.Command{
	Use:   "use <name>",
	Short: "Switch the active profile",
	Args:  cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		name := args[0]
		if err := config.UseHubProfile(name); err != nil {
			return err
		}
		fmt.Printf("✓ Now using profile %q\n", name)
		return nil
	},
}

var profileRmCmd = &cobra.Command{
	Use:     "rm <name>",
	Aliases: []string{"delete", "remove"},
	Short:   "Remove a profile",
	Args:    cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		name := args[0]
		if err := config.RemoveHubProfile(name); err != nil {
			return err
		}
		fmt.Printf("✓ Profile %q removed.\n", name)
		return nil
	},
}

var profileRenameCmd = &cobra.Command{
	Use:   "rename <old> <new>",
	Short: "Rename a profile",
	Args:  cobra.ExactArgs(2),
	RunE: func(cmd *cobra.Command, args []string) error {
		if err := config.RenameHubProfile(args[0], args[1]); err != nil {
			return err
		}
		fmt.Printf("✓ Profile %q renamed to %q\n", args[0], args[1])
		return nil
	},
}

var profileShowCmd = &cobra.Command{
	Use:   "show [name]",
	Short: "Show details for a profile",
	Args:  cobra.MaximumNArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		var profileName string
		if len(args) == 1 {
			profileName = args[0]
		}
		h, name, err := config.ResolveHub(profileName)
		if err != nil {
			return err
		}
		fmt.Printf("Profile: %s\n", name)
		fmt.Printf("Hub URL: %s\n", h.URL)
		// Mask token — show first 6 chars only
		token := h.Token
		if len(token) > 6 {
			token = token[:6] + "..."
		}
		fmt.Printf("Token:   %s\n", token)
		return nil
	},
}

func init() {
	rootCmd.AddCommand(profileCmd)
	profileCmd.AddCommand(profileLsCmd)
	profileCmd.AddCommand(profileCreateCmd)
	profileCmd.AddCommand(profileUseCmd)
	profileCmd.AddCommand(profileRmCmd)
	profileCmd.AddCommand(profileRenameCmd)
	profileCmd.AddCommand(profileShowCmd)

	profileCreateCmd.Flags().StringVar(&profileCreateURL, "url", "", "hub URL")
	profileCreateCmd.Flags().StringVar(&profileCreateToken, "token", "", "hub user token")
	profileCreateCmd.MarkFlagRequired("url")
	profileCreateCmd.MarkFlagRequired("token")
}
