package cmd

import (
	"context"
	"fmt"
	"strings"

	"github.com/elasticclaw/elasticclaw/pkg/config"
	"github.com/elasticclaw/elasticclaw/pkg/hub"
	"github.com/spf13/cobra"
)

var killCmd = &cobra.Command{
	Use:   "kill <claw-id-or-name>",
	Short: "Disconnect and remove a claw from the hub",
	Args:  cobra.ExactArgs(1),
	RunE:  runKill,
}

func init() {
	rootCmd.AddCommand(killCmd)
}

func runKill(cmd *cobra.Command, args []string) error {
	target := args[0]
	h, _, err := config.ResolveHub(profile)
	if err != nil {
		return err
	}
	client := hub.NewClient(h.URL, h.Token)
	ctx := context.Background()

	// Resolve name → ID
	claws, err := client.ListClaws(ctx)
	if err != nil {
		return fmt.Errorf("hub error: %w", err)
	}
	resolvedID := target
	for _, c := range claws {
		if c.Name == target || c.ID == target || strings.HasPrefix(c.ID, target) {
			resolvedID = c.ID
			break
		}
	}

	if err := client.KillClaw(ctx, resolvedID); err != nil {
		return fmt.Errorf("kill failed: %w", err)
	}

	fmt.Printf("✓ Claw %s killed\n", target)
	return nil
}
