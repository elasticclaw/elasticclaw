package cmd

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/elasticclaw/elasticclaw/pkg/provider/daytona"
	"github.com/elasticclaw/elasticclaw/pkg/state"
	"github.com/elasticclaw/elasticclaw/pkg/types"
	"github.com/spf13/cobra"
)

var inspectCmd = &cobra.Command{
	Use:   "inspect <instance>",
	Short: "Show instance details",
	Args:  cobra.ExactArgs(1),
	RunE:  runInspect,
}

func init() {
	rootCmd.AddCommand(inspectCmd)
}

func runInspect(cmd *cobra.Command, args []string) error {
	name := args[0]

	store, err := state.NewStore()
	if err != nil {
		return err
	}

	instance, err := store.Get(name)
	if err != nil {
		return err
	}

	// Check live status via provider
	ctx := context.Background()
	health := checkHealth(ctx, instance)
	instance.Status = health.Status

	if jsonOut {
		data, _ := json.MarshalIndent(instance, "", "  ")
		fmt.Println(string(data))
		return nil
	}

	fmt.Printf("Instance: %s\n", instance.Name)
	fmt.Println()
	fmt.Printf("  ID:         %s\n", instance.ID)
	fmt.Printf("  Provider:   %s\n", instance.Provider)
	fmt.Printf("  Status:     %s\n", instance.Status)
	fmt.Println()
	fmt.Printf("  Template:   %s\n", instance.Template)
	fmt.Printf("  Version:    %s\n", instance.TemplateVersion)
	fmt.Println()
	fmt.Printf("  State:      %s\n", instance.StateBackend)
	fmt.Printf("  Created:    %s\n", instance.CreatedAt.Format("2006-01-02 15:04:05 UTC"))
	if instance.TTL != "" {
		fmt.Printf("  TTL:        %s\n", instance.TTL)
	}

	if len(instance.ProviderMeta) > 0 {
		fmt.Println()
		fmt.Println("  Provider Metadata:")
		for k, v := range instance.ProviderMeta {
			fmt.Printf("    %s: %s\n", k, v)
		}
	}

	if len(instance.Vars) > 0 {
		fmt.Println()
		fmt.Println("  Variables:")
		for k, v := range instance.Vars {
			fmt.Printf("    %s: %s\n", k, v)
		}
	}

	// Connection info
	fmt.Println()
	fmt.Println("  Connect:")
	if instance.Provider == "daytona" {
		fmt.Printf("    Shell: daytona ssh %s\n", instance.Name)
	}
	fmt.Printf("    Chat:  elasticclaw chat %s\n", instance.Name)

	return nil
}

func checkHealth(ctx context.Context, instance *types.Instance) *types.InstanceHealth {
	health := &types.InstanceHealth{
		Status: types.StatusUnknown,
	}

	switch instance.Provider {
	case "daytona":
		p, pErr := daytona.New(nil)
		if pErr != nil {
			health.Status = types.StatusUnknown
			health.Message = "failed to init daytona provider"
			return health
		}

		// Try to run openclaw status inside the instance
		result, err := p.Exec(ctx, instance.Name, []string{"openclaw", "status", "--json"})
		if err != nil {
			// Check if instance exists at all
			status, err := p.Status(ctx, instance.Name)
			if err != nil {
				health.Status = types.StatusUnknown
				health.Message = "failed to check status"
			} else {
				health.Status = status
			}
			return health
		}

		if result.ExitCode == 0 {
			health.Status = types.StatusRunning
		} else if strings.Contains(result.Stdout, "not running") {
			health.Status = types.StatusUnhealthy
			health.Message = "OpenClaw not running"
		} else {
			health.Status = types.StatusUnhealthy
			health.Message = result.Stdout
		}

	default:
		health.Status = instance.Status
	}

	return health
}
