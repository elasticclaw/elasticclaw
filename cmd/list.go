package cmd

import (
	"encoding/json"
	"fmt"
	"os"
	"text/tabwriter"
	"time"

	"github.com/elasticclaw/elasticclaw/pkg/state"
	"github.com/elasticclaw/elasticclaw/pkg/types"
	"github.com/spf13/cobra"
)

var (
	listProvider    string
	listAll         bool
	listAllProfiles bool
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
}

func runList(cmd *cobra.Command, args []string) error {
	store, err := state.NewStore()
	if err != nil {
		return err
	}

	instances, err := store.List()
	if err != nil {
		return err
	}

	// Filter by provider if specified
	if listProvider != "" {
		var filtered []*types.Instance
		for _, inst := range instances {
			if inst.Provider == listProvider {
				filtered = append(filtered, inst)
			}
		}
		instances = filtered
	}

	_ = types.Instance{} // ensure import is used

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
		fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\n",
			inst.Name,
			inst.Provider,
			inst.Status,
			template,
			age,
		)
	}
	w.Flush()

	return nil
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


