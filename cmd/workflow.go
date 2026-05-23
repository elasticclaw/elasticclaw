package cmd

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
	"text/tabwriter"

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
	Template             string                   `json:"template"`
	Labels               []string                 `json:"labels"`
	AssignedTo           string                   `json:"assignedTo"`
	Enabled              bool                     `json:"enabled"`
	HasWebhookSecret     bool                     `json:"hasWebhookSecret"`
	WebhookSecretRef     string                   `json:"webhookSecretRef"`
	PipelineYAML         string                   `json:"pipelineYAML"`
	EnableManualTrigger  bool                     `json:"enableManualTrigger"`
	SecretRefs           map[string]string        `json:"secretRefs"`
	Inputs               []map[string]interface{} `json:"inputs"`
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
	fmt.Fprintln(w, "NAME\tINTEGRATION\tTRIGGER\tTEMPLATE\tMANUAL\tENABLED\tSOURCE")
	for _, workflow := range workflows {
		fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%t\t%t\t%s\n",
			workflow.Name,
			workflow.Integration,
			workflow.TriggerStatus,
			workflow.Template,
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

	fmt.Printf("Triggered workflow %q in workspace %q -> claw %s (%s)\n", name, workspace, shortID(result.ClawID), result.Status)
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
