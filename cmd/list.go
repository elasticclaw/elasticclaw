package cmd

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/elasticclaw/elasticclaw/pkg/config"
	"github.com/elasticclaw/elasticclaw/pkg/hub"
	"github.com/elasticclaw/elasticclaw/pkg/state"
	"github.com/elasticclaw/elasticclaw/pkg/types"
	"github.com/spf13/cobra"
)

var (
	listProvider    string
	listAll         bool
	listAllProfiles bool
	listTag         string
)

var listCmd = &cobra.Command{
	Use:     "list",
	Aliases: []string{"ls"},
	Short:   "List instances",
	RunE:    runList,
}

func init() {
	rootCmd.AddCommand(listCmd)

	listCmd.Flags().StringVarP(&listProvider, "provider", "p", "", "filter by provider")
	listCmd.Flags().BoolVar(&listAll, "all", false, "include stopped instances")
	listCmd.Flags().BoolVar(&listAllProfiles, "all-profiles", false, "show instances from all profiles")
	listCmd.Flags().StringVar(&listTag, "tag", "", "filter by tag")
}

func runList(cmd *cobra.Command, args []string) error {
	// Use hub if configured
	if h, _, err := config.ResolveHub(profile); err == nil {
		return runListHub(h)
	}

	// Fallback: local state
	store, err := state.NewStore()
	if err != nil {
		return err
	}

	instances, err := store.List()
	if err != nil {
		return err
	}

	if listProvider != "" {
		var filtered []*types.Instance
		for _, inst := range instances {
			if inst.Provider == listProvider {
				filtered = append(filtered, inst)
			}
		}
		instances = filtered
	}

	if len(instances) == 0 {
		fmt.Println("No instances found.")
		fmt.Println()
		fmt.Println("Create one with:")
		fmt.Println("  elasticclaw create --name <name> --provider daytona")
		return nil
	}

	if jsonOut {
		data, _ := json.MarshalIndent(instances, "", "  ")
		fmt.Println(string(data))
		return nil
	}

	w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintln(w, "NAME\tPROVIDER\tSTATUS\tTEMPLATE\tAGE")
	for _, inst := range instances {
		age := formatAge(inst.CreatedAt)
		template := inst.Template
		if inst.TemplateVersion != "" {
			template = fmt.Sprintf("%s@%s", inst.Template, inst.TemplateVersion)
		}
		fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\n", inst.Name, inst.Provider, inst.Status, template, age)
	}
	w.Flush()
	return nil
}

func runListHub(h *types.HubProfile) error {
	client := hub.NewClient(h.URL, h.Token)
	claws, err := client.ListClaws(context.Background())
	if err != nil {
		return fmt.Errorf("hub error: %w", err)
	}

	if len(claws) == 0 {
		fmt.Println("No claws registered.")
		fmt.Println()
		fmt.Println("Claws register automatically when they start up.")
		return nil
	}

	if jsonOut {
		data, _ := json.MarshalIndent(claws, "", "  ")
		fmt.Println(string(data))
		return nil
	}

	w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintln(w, "ID\tNAME\tTEMPLATE\tSTATUS\tAGE\tTAGS")
	for _, c := range claws {
		if listTag != "" && !clawHasTag(c.Tags, listTag) {
			continue
		}
		tags := strings.Join(c.Tags, ", ")
		if tags == "" {
			tags = "-"
		}
		fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\t%s\n",
			c.ID[:8], c.Name, c.Template, c.Status, formatAge(c.CreatedAt), tags)
	}
	w.Flush()
	return nil
}

// clawHasTag checks if a claw has a tag matching the filter.
// Filter can be "key" (match any value for that key) or "key=value" (exact match).
func clawHasTag(tags []string, filter string) bool {
	for _, t := range tags {
		if t == filter {
			return true
		}
		// Filter is a bare key — match any k=v with that key
		if !strings.Contains(filter, "=") {
			if strings.HasPrefix(t, filter+"=") {
				return true
			}
		}
	}
	return false
}

func formatAge(t time.Time) string {
	if t.IsZero() {
		return "-"
	}
	d := time.Since(t)
	if d < time.Minute {
		return fmt.Sprintf("%ds", int(d.Seconds()))
	}
	if d < time.Hour {
		return fmt.Sprintf("%dm", int(d.Minutes()))
	}
	if d < 24*time.Hour {
		return fmt.Sprintf("%dh", int(d.Hours()))
	}
	return fmt.Sprintf("%dd", int(d.Hours()/24))
}
