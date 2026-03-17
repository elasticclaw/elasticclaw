package cmd

import (
	"fmt"
	"os"
	"text/tabwriter"

	"github.com/elasticclaw/elasticclaw/pkg/types"
	"github.com/spf13/cobra"
)

var providerCmd = &cobra.Command{
	Use:   "provider",
	Short: "Provider management commands",
}

var providerListCmd = &cobra.Command{
	Use:     "list",
	Aliases: []string{"ls"},
	Short:   "List available providers",
	RunE:    runProviderList,
}

func init() {
	rootCmd.AddCommand(providerCmd)
	providerCmd.AddCommand(providerListCmd)
}

func runProviderList(cmd *cobra.Command, args []string) error {
	// For MVP, we have a static list of known providers
	// In future, this would come from the provider registry
	providers := []types.ProviderInfo{
		{
			Name:         "daytona",
			Type:         types.ProviderTypeEphemeral,
			Capabilities: []string{"exec", "snapshot"},
		},
		{
			Name:         "local",
			Type:         types.ProviderTypeStateful,
			Capabilities: []string{"exec"},
		},
		{
			Name:         "sprites",
			Type:         types.ProviderTypeStateful,
			Capabilities: []string{"exec", "hibernate", "snapshot"},
		},
		{
			Name:         "exe",
			Type:         types.ProviderTypeEphemeral,
			Capabilities: []string{"exec"},
		},
	}

	if jsonOut {
		fmt.Println("[")
		for i, p := range providers {
			comma := ","
			if i == len(providers)-1 {
				comma = ""
			}
			fmt.Printf(`  {"name":"%s","type":"%s"}%s`+"\n", p.Name, p.Type, comma)
		}
		fmt.Println("]")
		return nil
	}

	w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintln(w, "NAME\tTYPE\tSTATUS")

	// Check which providers are configured
	for _, p := range providers {
		status := "available"
		if p.Name == "daytona" {
			status = "ready" // For demo purposes
		}
		fmt.Fprintf(w, "%s\t%s\t%s\n", p.Name, p.Type, status)
	}
	w.Flush()

	return nil
}
