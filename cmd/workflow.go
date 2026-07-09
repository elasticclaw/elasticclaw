package cmd

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"text/tabwriter"

	"github.com/elasticclaw/elasticclaw/pkg/types"
	"github.com/spf13/cobra"
	"gopkg.in/yaml.v3"
)

func WorkflowCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "workflow",
		Short: "Manage elasticclaw workflows",
	}
	cmd.AddCommand(workflowListCmd())
	cmd.AddCommand(workflowShowCmd())
	cmd.AddCommand(workflowPushCmd())
	cmd.AddCommand(workflowTriggerCmd())
	return cmd
}

type workflowCLIView struct {
	Name                 string                   `json:"name"`
	WorkspaceName        string                   `json:"workspaceName"`
	Source               string                   `json:"source"`
	Integration          string                   `json:"integration"`
	IntegrationWorkspace string                   `json:"integrationWorkspace"`
	TriggerStatus        string                   `json:"triggerStatus"`
	DoneStatus           string                   `json:"doneStatus"`
	Labels               []string                 `json:"labels"`
	AssignedTo           string                   `json:"assignedTo"`
	Enabled              bool                     `json:"enabled"`
	HasWebhookSecret     bool                     `json:"hasWebhookSecret"`
	WebhookSecretRef     string                   `json:"webhookSecretRef"`
	PipelineYAML         string                   `json:"pipelineYAML"`
	EnableManualTrigger  bool                     `json:"enableManualTrigger"`
	SecretRefs           map[string]string        `json:"secretRefs"`
	Inputs               []map[string]interface{} `json:"inputs"`
	RawConfig            string                   `json:"rawConfig,omitempty"`
}

func workflowListCmd() *cobra.Command {
	var workspace string
	cmd := &cobra.Command{
		Use:   "list",
		Short: "List workflows in a workspace",
		RunE: func(cmd *cobra.Command, args []string) error {
			return runWorkflowList(workspace)
		},
	}
	cmd.Flags().StringVar(&workspace, "workspace", "default", "workspace name")
	return cmd
}

func runWorkflowList(workspace string) error {
	workflows, err := fetchWorkflowViews(workspace)
	if err != nil {
		return err
	}
	if jsonOut {
		return json.NewEncoder(os.Stdout).Encode(workflows)
	}
	if len(workflows) == 0 {
		fmt.Printf("No workflows configured in workspace %q.\n", workspace)
		return nil
	}

	w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintln(w, "NAME\tINTEGRATION\tTRIGGER\tMANUAL\tENABLED\tSOURCE")
	for _, workflow := range workflows {
		fmt.Fprintf(w, "%s\t%s\t%s\t%t\t%t\t%s\n",
			workflow.Name,
			workflow.Integration,
			workflow.TriggerStatus,
			workflow.EnableManualTrigger,
			workflow.Enabled,
			workflow.Source,
		)
	}
	w.Flush()
	return nil
}

func workflowShowCmd() *cobra.Command {
	var workspace string
	cmd := &cobra.Command{
		Use:   "show <name>",
		Short: "Show a workflow's current hub configuration",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return runWorkflowShow(workspace, args[0])
		},
	}
	cmd.Flags().StringVar(&workspace, "workspace", "default", "workspace name")
	return cmd
}

func runWorkflowShow(workspace, name string) error {
	workflow, err := fetchWorkflowView(workspace, name)
	if err != nil {
		return err
	}
	if jsonOut {
		return json.NewEncoder(os.Stdout).Encode(workflow)
	}
	if strings.TrimSpace(workflow.RawConfig) != "" {
		fmt.Print(workflow.RawConfig)
		if !strings.HasSuffix(workflow.RawConfig, "\n") {
			fmt.Println()
		}
		return nil
	}
	out, err := yaml.Marshal(workflow)
	if err != nil {
		return err
	}
	fmt.Print(string(out))
	return nil
}

func workflowTriggerCmd() *cobra.Command {
	var workspace string
	var inputs []string
	cmd := &cobra.Command{
		Use:   "trigger <name>",
		Short: "Manually trigger a workflow with inputs",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return runWorkflowTrigger(workspace, args[0], inputs)
		},
	}
	cmd.Flags().StringVar(&workspace, "workspace", "default", "workspace name")
	cmd.Flags().StringArrayVar(&inputs, "input", nil, "input values as key=value (can be repeated)")
	return cmd
}

func workflowPushCmd() *cobra.Command {
	var workspace string
	cmd := &cobra.Command{
		Use:   "push <file-or-dir>...",
		Short: "Push workflow definitions to a workspace",
		Args:  cobra.MinimumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return runWorkflowPush(workspace, args)
		},
	}
	cmd.Flags().StringVar(&workspace, "workspace", "", "workspace name [required]")
	return cmd
}

func runWorkflowPush(workspace string, paths []string) error {
	if strings.TrimSpace(workspace) == "" {
		return fmt.Errorf("--workspace is required")
	}
	hubURL, clawToken, err := resolveHubConn()
	if err != nil {
		return err
	}
	workflows, err := readWorkflowFiles(paths)
	if err != nil {
		return err
	}
	if len(workflows) == 0 {
		return fmt.Errorf("no workflow YAML files found")
	}
	for _, workflow := range workflows {
		if err := workflow.Validate(); err != nil {
			return fmt.Errorf("validation failed for workflow %q: %w", workflow.Name, err)
		}
	}

	body, _ := json.Marshal(map[string]interface{}{"workflows": workflows})
	req, _ := http.NewRequest(http.MethodPost, hubURL+"/api/workspaces/"+url.PathEscape(workspace)+"/workflows", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+clawToken)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return fmt.Errorf("push workflows failed: %w", err)
	}
	defer resp.Body.Close()
	respBody, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("hub returned %d: %s", resp.StatusCode, strings.TrimSpace(string(respBody)))
	}

	var result struct {
		Pushed    int `json:"pushed"`
		Workflows []struct {
			Name string `json:"name"`
		} `json:"workflows"`
	}
	_ = json.Unmarshal(respBody, &result)
	fmt.Printf("Pushed %d workflow(s) to workspace %q:\n", result.Pushed, workspace)
	for _, workflow := range result.Workflows {
		fmt.Printf("  ✓ %s\n", workflow.Name)
	}
	return nil
}

func readWorkflowFiles(paths []string) ([]*types.WorkflowConfig, error) {
	var files []string
	for _, path := range paths {
		info, err := os.Stat(path)
		if err != nil {
			return nil, err
		}
		if !info.IsDir() {
			files = append(files, path)
			continue
		}
		entries, err := os.ReadDir(path)
		if err != nil {
			return nil, err
		}
		for _, entry := range entries {
			if entry.IsDir() || strings.HasPrefix(entry.Name(), ".") {
				continue
			}
			if strings.HasSuffix(entry.Name(), ".yaml") || strings.HasSuffix(entry.Name(), ".yml") {
				files = append(files, filepath.Join(path, entry.Name()))
			}
		}
	}

	var workflows []*types.WorkflowConfig
	for _, path := range files {
		data, err := os.ReadFile(path)
		if err != nil {
			return nil, fmt.Errorf("read %s: %w", path, err)
		}
		var workflow types.WorkflowConfig
		if err := yaml.Unmarshal(data, &workflow); err != nil {
			return nil, fmt.Errorf("parse %s: %w", path, err)
		}
		workflow.RawConfig = string(data)
		if workflow.Name == "" {
			workflow.Name = strings.TrimSuffix(strings.TrimSuffix(filepath.Base(path), ".yaml"), ".yml")
		}
		if err := types.NormalizeWorkflowConfig(&workflow); err != nil {
			return nil, fmt.Errorf("normalize %s: %w", path, err)
		}
		workflows = append(workflows, &workflow)
	}
	return workflows, nil
}

func runWorkflowTrigger(workspace, name string, inputs []string) error {
	hubURL, clawToken, err := resolveHubConn()
	if err != nil {
		return err
	}
	inputMap, err := parseTriggerInputs(inputs)
	if err != nil {
		return err
	}

	body, _ := json.Marshal(map[string]interface{}{"inputs": inputMap})
	path := fmt.Sprintf("/api/workspaces/%s/workflows/%s/trigger", url.PathEscape(workspace), url.PathEscape(name))
	req, _ := http.NewRequest(http.MethodPost, hubURL+path, bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+clawToken)

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return fmt.Errorf("trigger workflow failed: %w", err)
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

	fmt.Printf("Triggered workflow %q in workspace %q -> agent %s (%s)\n", name, workspace, shortID(result.ClawID), result.Status)
	return nil
}

func fetchWorkflowViews(workspace string) ([]workflowCLIView, error) {
	hubURL, clawToken, err := resolveHubConn()
	if err != nil {
		return nil, err
	}

	req, _ := http.NewRequest(http.MethodGet, hubURL+"/api/workspaces/"+url.PathEscape(workspace)+"/workflows", nil)
	req.Header.Set("Authorization", "Bearer "+clawToken)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("list workflows failed: %w", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("hub returned %d: %s", resp.StatusCode, strings.TrimSpace(string(body)))
	}

	var workflows []workflowCLIView
	if err := json.Unmarshal(body, &workflows); err != nil {
		return nil, fmt.Errorf("decode: %w", err)
	}
	return workflows, nil
}

func fetchWorkflowView(workspace, name string) (*workflowCLIView, error) {
	hubURL, clawToken, err := resolveHubConn()
	if err != nil {
		return nil, err
	}

	path := fmt.Sprintf("/api/workspaces/%s/workflows/%s", url.PathEscape(workspace), url.PathEscape(name))
	req, _ := http.NewRequest(http.MethodGet, hubURL+path, nil)
	req.Header.Set("Authorization", "Bearer "+clawToken)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("show workflow failed: %w", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("hub returned %d: %s", resp.StatusCode, strings.TrimSpace(string(body)))
	}

	var workflow workflowCLIView
	if err := json.Unmarshal(body, &workflow); err != nil {
		return nil, fmt.Errorf("decode: %w", err)
	}
	return &workflow, nil
}

func init() {
	rootCmd.AddCommand(WorkflowCmd())
}
