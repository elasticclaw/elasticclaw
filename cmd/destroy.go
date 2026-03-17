package cmd

import (
	"bufio"
	"context"
	"fmt"
	"os"
	"strings"

	"github.com/elasticclaw/elasticclaw/pkg/provider/daytona"
	"github.com/elasticclaw/elasticclaw/pkg/provider/local"
	"github.com/elasticclaw/elasticclaw/pkg/state"
	"github.com/spf13/cobra"
)

var (
	destroyKeepState      bool
	destroyRevokeIdentity bool
	destroyForce          bool
	destroyAll            bool
)

var destroyCmd = &cobra.Command{
	Use:   "destroy <instance>",
	Short: "Destroy an instance",
	Args:  cobra.MaximumNArgs(1),
	RunE:  runDestroy,
}

func init() {
	rootCmd.AddCommand(destroyCmd)

	destroyCmd.Flags().BoolVar(&destroyKeepState, "keep-state", false, "preserve instance state")
	destroyCmd.Flags().BoolVar(&destroyRevokeIdentity, "revoke-identity", false, "revoke identity credentials")
	destroyCmd.Flags().BoolVarP(&destroyForce, "force", "f", false, "skip confirmation")
	destroyCmd.Flags().BoolVar(&destroyAll, "all", false, "destroy all instances")
}

func runDestroy(cmd *cobra.Command, args []string) error {
	store, err := state.NewStore()
	if err != nil {
		return err
	}

	var names []string

	if destroyAll {
		instances, err := store.List()
		if err != nil {
			return err
		}
		for _, inst := range instances {
			names = append(names, inst.Name)
		}
		if len(names) == 0 {
			fmt.Println("No instances to destroy.")
			return nil
		}
	} else {
		if len(args) == 0 {
			return fmt.Errorf("instance name required (or use --all)")
		}
		names = []string{args[0]}
	}

	// Confirm unless --force or --yes
	if !destroyForce && !yes {
		if destroyAll {
			fmt.Printf("This will destroy %d instance(s):\n", len(names))
			for _, name := range names {
				fmt.Printf("  - %s\n", name)
			}
		} else {
			fmt.Printf("This will destroy instance %q\n", names[0])
		}
		fmt.Print("\nContinue? [y/N] ")

		reader := bufio.NewReader(os.Stdin)
		response, _ := reader.ReadString('\n')
		response = strings.TrimSpace(strings.ToLower(response))
		if response != "y" && response != "yes" {
			fmt.Println("Aborted.")
			return nil
		}
	}

	ctx := context.Background()

	for _, name := range names {
		if err := destroyInstance(ctx, store, name); err != nil {
			fmt.Fprintf(os.Stderr, "Failed to destroy %s: %v\n", name, err)
			if !destroyAll {
				return err
			}
			continue
		}
		fmt.Printf("✓ Destroyed %s\n", name)
	}

	return nil
}

func destroyInstance(ctx context.Context, store *state.Store, name string) error {
	instance, err := store.Get(name)
	if err != nil {
		return err
	}

	// Destroy via provider
	switch instance.Provider {
	case "daytona":
		p, pErr := daytona.New(nil)
		if pErr != nil {
			fmt.Fprintf(os.Stderr, "Warning: failed to init daytona provider: %v\n", pErr)
		} else if err := p.Destroy(ctx, instance.Name, destroyKeepState); err != nil {
			// Log but continue - instance might already be gone
			fmt.Fprintf(os.Stderr, "Warning: provider destroy: %v\n", err)
		}
	case "local":
		p := local.New()
		if err := p.Destroy(ctx, instance.Name, destroyKeepState); err != nil {
			fmt.Fprintf(os.Stderr, "Warning: provider destroy: %v\n", err)
		}
	}

	// TODO: Revoke identity if requested

	// Remove from state store
	if err := store.Delete(name); err != nil {
		return fmt.Errorf("failed to remove from state: %w", err)
	}

	return nil
}
