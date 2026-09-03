package cmd

import (
	"fmt"
	"os"
	"path/filepath"

	"github.com/elasticclaw/elasticclaw/pkg/hub"
	"github.com/elasticclaw/elasticclaw/pkg/types"
	"github.com/spf13/cobra"
	"gopkg.in/yaml.v3"
)

var backfillAccessDB, backfillAccessConfig string
var backfillAccessDryRun bool

var hubBackfillAccessCmd = &cobra.Command{Use: "backfill-access", Short: "Backfill hub access users and roles", RunE: runHubBackfillAccess}

func init() {
	hubCmd.AddCommand(hubBackfillAccessCmd)
	home, _ := os.UserHomeDir()
	base := filepath.Join(home, ".elasticclaw")
	hubBackfillAccessCmd.Flags().StringVar(&backfillAccessDB, "db", filepath.Join(base, "hub.db"), "path to SQLite database")
	hubBackfillAccessCmd.Flags().StringVar(&backfillAccessConfig, "config", filepath.Join(base, "hub.yaml"), "path to hub config")
	hubBackfillAccessCmd.Flags().BoolVar(&backfillAccessDryRun, "dry-run", false, "report changes without writing")
}
func runHubBackfillAccess(cmd *cobra.Command, args []string) error {
	data, err := os.ReadFile(backfillAccessConfig)
	if err != nil {
		return fmt.Errorf("read config: %w", err)
	}
	var cfg types.HubConfig
	if err = yaml.Unmarshal(data, &cfg); err != nil {
		return fmt.Errorf("parse config: %w", err)
	}
	var admins []string
	if cfg.Auth != nil && cfg.Auth.Access != nil {
		admins = cfg.Auth.Access.Admins
	}
	rows, err := hub.BackfillAccess(backfillAccessDB, admins, backfillAccessDryRun)
	if err != nil {
		return err
	}
	fmt.Fprintln(cmd.OutOrStdout(), "tenant\tlogin\tcargo\tação")
	created, updated := 0, 0
	for _, r := range rows {
		fmt.Fprintf(cmd.OutOrStdout(), "%s\t%s\t%s\t%s\n", r.Tenant, r.Login, r.Role, r.Action)
		if r.Action == "created" {
			created++
		}
		if r.Action == "updated" {
			updated++
		}
	}
	fmt.Fprintf(cmd.OutOrStdout(), "%d users: %d created, %d updated, %d unchanged\n", len(rows), created, updated, len(rows)-created-updated)
	return nil
}
