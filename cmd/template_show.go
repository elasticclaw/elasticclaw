package cmd

import (
	"context"
	"fmt"
	"sort"
	"strings"

	"github.com/elasticclaw/elasticclaw/pkg/config"
	"github.com/elasticclaw/elasticclaw/pkg/hub"
	"github.com/spf13/cobra"
)

var templateShowFiles bool

var templateShowCmd = &cobra.Command{
	Use:   "show <name>",
	Short: "Show a hub template's config and files",
	Long: `Fetch a pushed template from the connected hub and print its config and file list.

Use --files to also dump the full contents of each workspace file.

Example:
  elasticclaw template show vandoor-developer
  elasticclaw template show vandoor-developer --files`,
	Args: cobra.ExactArgs(1),
	RunE: runTemplateShow,
}

func init() {
	templateCmd.AddCommand(templateShowCmd)
	templateShowCmd.Flags().BoolVar(&templateShowFiles, "files", false, "print full contents of each workspace file")
}

func runTemplateShow(cmd *cobra.Command, args []string) error {
	name := args[0]

	h, _, err := config.ResolveHub(profile)
	if err != nil {
		return err
	}
	client := hub.NewClient(h.URL, h.Token)
	files, err := client.GetHubTemplate(context.Background(), name)
	if err != nil {
		return fmt.Errorf("fetch failed: %w", err)
	}

	fmt.Printf("Template: %s\n", name)
	fmt.Printf("Source:   hub (%s)\n\n", h.URL)

	if cfg, ok := files["elasticclaw-config.yaml"]; ok {
		fmt.Println("elasticclaw-config.yaml:")
		for _, line := range strings.Split(strings.TrimRight(cfg, "\n"), "\n") {
			fmt.Printf("  %s\n", line)
		}
		fmt.Println()
	} else {
		fmt.Println("(no elasticclaw-config.yaml present)")
		fmt.Println()
	}

	// Collect non-config file names, sorted.
	names := make([]string, 0, len(files))
	for n := range files {
		if n == "elasticclaw-config.yaml" {
			continue
		}
		names = append(names, n)
	}
	sort.Strings(names)

	if len(names) == 0 {
		fmt.Println("Files: (none)")
		return nil
	}

	fmt.Println("Files:")
	for _, n := range names {
		fmt.Printf("  %-24s  %s\n", n, humanSize(len(files[n])))
	}

	if templateShowFiles {
		for _, n := range names {
			fmt.Printf("\n--- %s ---\n", n)
			fmt.Println(files[n])
		}
	}
	return nil
}

func humanSize(n int) string {
	switch {
	case n < 1024:
		return fmt.Sprintf("%d B", n)
	case n < 1024*1024:
		return fmt.Sprintf("%.1f KB", float64(n)/1024)
	default:
		return fmt.Sprintf("%.1f MB", float64(n)/(1024*1024))
	}
}
