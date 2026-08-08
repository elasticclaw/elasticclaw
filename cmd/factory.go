package cmd

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"text/tabwriter"

	"github.com/elasticclaw/elasticclaw/pkg/config"
	"github.com/spf13/cobra"
	"gopkg.in/yaml.v3"
)

func FactoryCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "factory",
		Short: "Manage elasticclaw factories",
	}
	cmd.AddCommand(factoryCreateCmd())
	cmd.AddCommand(factoryPushCmd())
	cmd.AddCommand(factoryListCmd())
	cmd.AddCommand(factoryShowCmd())
	cmd.AddCommand(factoryRmCmd())
	cmd.AddCommand(factoryTriggerCmd())
	return cmd
}

func init() {
	cmd := FactoryCmd()
	cmd.Hidden = true
	cmd.Deprecated = "factories are retired; use workspace workflows (elasticclaw workflow push --workspace <name>)"
	rootCmd.AddCommand(cmd)
}

// ── factory create ────────────────────────────────────────────────────────────

func factoryCreateCmd() *cobra.Command {
	var (
		name          string
		integration   string
		workspace     string
		triggerStatus string
		doneStatus    string
		tmpl          string
	)
	cmd := &cobra.Command{
		Use:   "create",
		Short: "Bootstrap a new factory directory",
		RunE: func(cmd *cobra.Command, args []string) error {
			if name == "" {
				return fmt.Errorf("--name is required")
			}
			return runFactoryCreate(name, integration, workspace, triggerStatus, doneStatus, tmpl)
		},
	}
	cmd.Flags().StringVar(&name, "name", "", "factory name (slug, used as directory name) [required]")
	cmd.Flags().StringVar(&integration, "integration", "linear", "integration type (linear, shortcut, github)")
	cmd.Flags().StringVar(&workspace, "workspace", "", "workspace name (defaults to --name)")
	cmd.Flags().StringVar(&triggerStatus, "trigger-status", "In Progress", "issue status that triggers agent creation")
	cmd.Flags().StringVar(&doneStatus, "done-status", "In Review", "issue status set when agent sends [DONE]")
	cmd.Flags().StringVar(&tmpl, "template", "elasticclaw", "agent template to use")
	return cmd
}

func runFactoryCreate(name, integration, workspace, triggerStatus, doneStatus, tmpl string) error {
	return fmt.Errorf("factories are retired; use workspace workflows instead:\n  elasticclaw workspace push --path <workspace-dir> <name>\n  elasticclaw workflow push --workspace <name> <workflow.yaml>")
}

// ── factory push ──────────────────────────────────────────────────────────────

func factoryPushCmd() *cobra.Command {
	var name string
	cmd := &cobra.Command{
		Use:   "push [name]",
		Short: "Push factory definitions to the hub",
		RunE: func(cmd *cobra.Command, args []string) error {
			if len(args) > 0 {
				name = args[0]
			}
			return runFactoryPush(name)
		},
	}
	return cmd
}

func runFactoryPush(filterName string) error {
	return fmt.Errorf("factories are retired; use workspace workflows instead:\n  elasticclaw workflow push --workspace <name> <workflow.yaml>")
}

func factoryListCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "list",
		Short: "List factories on the hub",
		RunE: func(cmd *cobra.Command, args []string) error {
			return runFactoryList()
		},
	}
}

type factoryView struct {
	Name          string   `json:"name"`
	Integration   string   `json:"integration"`
	Workspace     string   `json:"workspace"`
	TriggerStatus string   `json:"trigger_status"`
	DoneStatus    string   `json:"done_status"`
	Template      string   `json:"template"`
	Labels        []string `json:"labels"`
	AssignedTo    string   `json:"assigned_to"`
	Enabled       *bool    `json:"enabled"`
}

func runFactoryList() error {
	hubURL, clawToken, err := resolveHubConn()
	if err != nil {
		return err
	}

	req, _ := http.NewRequest(http.MethodGet, hubURL+"/api/factories", nil)
	req.Header.Set("Authorization", "Bearer "+clawToken)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return fmt.Errorf("list failed: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("hub returned %d: %s", resp.StatusCode, strings.TrimSpace(string(body)))
	}

	var factories []factoryView
	if err := json.NewDecoder(resp.Body).Decode(&factories); err != nil {
		return fmt.Errorf("decode: %w", err)
	}

	if len(factories) == 0 {
		fmt.Println("No factories configured.")
		return nil
	}

	w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintln(w, "NAME\tINTEGRATION\tWORKSPACE\tTRIGGER\tTEMPLATE\tENABLED")
	for _, f := range factories {
		enabled := "true"
		if f.Enabled != nil && !*f.Enabled {
			enabled = "false"
		}
		fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\t%s\n",
			f.Name, f.Integration, f.Workspace, f.TriggerStatus, f.Template, enabled)
	}
	w.Flush()
	return nil
}

// ── factory show ──────────────────────────────────────────────────────────────

func factoryShowCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "show <name>",
		Short: "Show a factory's current hub configuration",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return runFactoryShow(args[0])
		},
	}
}

func runFactoryShow(name string) error {
	hubURL, clawToken, err := resolveHubConn()
	if err != nil {
		return err
	}

	req, _ := http.NewRequest(http.MethodGet, hubURL+"/api/factories?name="+url.QueryEscape(name), nil)
	req.Header.Set("Authorization", "Bearer "+clawToken)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("hub returned %d: %s", resp.StatusCode, strings.TrimSpace(string(body)))
	}

	// Pretty print as YAML
	var v []interface{}
	_ = json.Unmarshal(body, &v)
	if len(v) == 0 {
		return fmt.Errorf("factory %q not found", name)
	}
	out, _ := yaml.Marshal(v[0])
	fmt.Print(string(out))
	return nil
}

// ── factory rm ────────────────────────────────────────────────────────────────

func factoryRmCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "rm <name>",
		Short: "Remove a factory from the hub",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return runFactoryRm(args[0])
		},
	}
}

func runFactoryRm(name string) error {
	hubURL, clawToken, err := resolveHubConn()
	if err != nil {
		return err
	}

	req, _ := http.NewRequest(http.MethodDelete, hubURL+"/api/factories?name="+url.QueryEscape(name), nil)
	req.Header.Set("Authorization", "Bearer "+clawToken)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("hub returned %d: %s", resp.StatusCode, strings.TrimSpace(string(body)))
	}

	fmt.Printf("Removed factory %q from hub.\n", name)
	return nil
}

// ── factory trigger ───────────────────────────────────────────────────────────

func factoryTriggerCmd() *cobra.Command {
	var inputs []string
	cmd := &cobra.Command{
		Use:   "trigger <name>",
		Short: "Manually trigger a factory with inputs",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return runFactoryTrigger(args[0], inputs)
		},
	}
	cmd.Flags().StringArrayVar(&inputs, "input", nil, "input values as key=value (can be repeated)")
	return cmd
}

func runFactoryTrigger(name string, inputs []string) error {
	hubURL, clawToken, err := resolveHubConn()
	if err != nil {
		return err
	}

	inputMap, err := parseTriggerInputs(inputs)
	if err != nil {
		return err
	}

	body, _ := json.Marshal(map[string]interface{}{"inputs": inputMap})
	req, _ := http.NewRequest(http.MethodPost, hubURL+"/api/factories/"+url.PathEscape(name)+"/trigger", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+clawToken)

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return fmt.Errorf("trigger failed: %w", err)
	}
	defer resp.Body.Close()
	respBody, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("hub returned %d: %s", resp.StatusCode, strings.TrimSpace(string(respBody)))
	}

	var result struct {
		ClawID string `json:"claw_id"`
		Status string `json:"status"`
	}
	if err := json.Unmarshal(respBody, &result); err != nil {
		return fmt.Errorf("decode response: %w", err)
	}

	fmt.Printf("Triggered factory %q → agent %s (%s)\n", name, shortID(result.ClawID), result.Status)
	return nil
}

// ── helpers ───────────────────────────────────────────────────────────────────

func parseTriggerInputs(inputs []string) (map[string]interface{}, error) {
	inputMap := make(map[string]interface{})
	for _, in := range inputs {
		parts := strings.SplitN(in, "=", 2)
		if len(parts) != 2 {
			return nil, fmt.Errorf("invalid input format %q (expected key=value)", in)
		}
		key := parts[0]
		val := parts[1]

		if strings.EqualFold(val, "true") {
			inputMap[key] = true
			continue
		}
		if strings.EqualFold(val, "false") {
			inputMap[key] = false
			continue
		}
		if num, err := strconv.ParseFloat(val, 64); err == nil {
			inputMap[key] = num
			continue
		}
		inputMap[key] = val
	}
	return inputMap, nil
}

func resolveHubConn() (hubURL, clawToken string, err error) {
	// Try env first
	hubURL = os.Getenv("ELASTICCLAW_HUB_URL")
	clawToken = os.Getenv("ELASTICCLAW_CLAW_TOKEN")

	if hubURL == "" || clawToken == "" {
		h, _, resolveErr := config.ResolveHub(profile)
		if resolveErr == nil && h != nil {
			if hubURL == "" {
				hubURL = h.URL
			}
			if clawToken == "" {
				clawToken = h.Token
			}
		}
	}

	if hubURL == "" {
		return "", "", fmt.Errorf("hub URL not set — use ELASTICCLAW_HUB_URL or configure with `elasticclaw hub init`")
	}
	if clawToken == "" {
		return "", "", fmt.Errorf("agent token not set — use ELASTICCLAW_CLAW_TOKEN or configure with `elasticclaw hub init`")
	}
	return hubURL, clawToken, nil
}
