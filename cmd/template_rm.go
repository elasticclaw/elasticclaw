package cmd

import (
	"context"
	"fmt"

	"github.com/elasticclaw/elasticclaw/pkg/config"
	"github.com/elasticclaw/elasticclaw/pkg/hub"
	"github.com/spf13/cobra"
)

var templateRmCmd = &cobra.Command{
	Use:     "rm <name>",
	Aliases: []string{"delete"},
	Short:   "Delete a template from the hub",
	Long: `Delete a pushed template from the connected hub.

Example:
  elasticclaw template rm vandoor-developer`,
	Args: cobra.ExactArgs(1),
	RunE: runTemplateRm,
}

func init() {
	templateCmd.AddCommand(templateRmCmd)
}

func runTemplateRm(cmd *cobra.Command, args []string) error {
	name := args[0]

	h, _, err := config.ResolveHub(profile)
	if err != nil {
		return err
	}
	client := hub.NewClient(h.URL, h.Token)
	if err := client.DeleteHubTemplate(context.Background(), name); err != nil {
		return fmt.Errorf("delete failed: %w", err)
	}
	fmt.Printf("✓ Template %q deleted from hub\n", name)
	return nil
}
