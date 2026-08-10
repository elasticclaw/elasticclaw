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
	"time"

	workflowv2 "github.com/elasticclaw/elasticclaw/pkg/hub/workflowv2"
	"github.com/elasticclaw/elasticclaw/pkg/types"
	v2 "github.com/elasticclaw/elasticclaw/pkg/types/v2"
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
	cmd.AddCommand(workflowRmCmd())
	cmd.AddCommand(workflowTriggerCmd())
	cmd.AddCommand(workflowRunsCmd())
	cmd.AddCommand(workflowInspectCmd())
	cmd.AddCommand(workflowLogsCmd())
	cmd.AddCommand(workflowConvertCmd())
	return cmd
}

func workflowInspectCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "inspect <run-id>",
		Short: "Explain the durable state of a workflow v2 run",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return runWorkflowInspect(args[0])
		},
	}
}

func runWorkflowInspect(runID string) error {
	hubURL, clawToken, err := resolveHubConn()
	if err != nil {
		return err
	}
	path := "/api/v2/workflow-runs/" + url.PathEscape(runID)
	req, _ := http.NewRequest(http.MethodGet, hubURL+path, nil)
	req.Header.Set("Authorization", "Bearer "+clawToken)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return fmt.Errorf("inspect workflow run failed: %w", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode == http.StatusNotFound {
		return fmt.Errorf("workflow v2 run %s not found", runID)
	}
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("hub returned %d: %s", resp.StatusCode, strings.TrimSpace(string(body)))
	}
	var inspection workflowv2.Inspection
	if err := json.Unmarshal(body, &inspection); err != nil {
		return fmt.Errorf("decode workflow run: %w", err)
	}
	if jsonOut {
		return json.NewEncoder(os.Stdout).Encode(inspection)
	}
	printWorkflowInspection(inspection)
	return nil
}

func printWorkflowInspection(inspection workflowv2.Inspection) {
	run := inspection.Run
	fmt.Printf("Run: %s\nWorkflow: %s/%s\nState: %s (phase %s, version %d)\nStatus: %s\n",
		run.ID, run.WorkspaceName, run.WorkflowName, run.State, run.DisplayPhase, run.StateVersion, run.Status)
	if len(inspection.Waiting) > 0 {
		fmt.Println("Waiting:")
		for _, reason := range inspection.Waiting {
			fmt.Printf("  - %s: %s\n", reason.Kind, reason.Detail)
		}
	}
	if len(inspection.ExpectedTransitions) > 0 {
		fmt.Println("Expected transitions:")
		for _, transition := range inspection.ExpectedTransitions {
			fmt.Printf("  - %s --[%s]--> %s\n", run.State, transition.EventKind, transition.ToState)
		}
	}
	fmt.Printf("Delivery: %d active PR(s), %d open, %d merged, %d closed\n",
		inspection.Delivery.ActivePullRequests, inspection.Delivery.OpenPullRequests,
		inspection.Delivery.MergedPullRequests, inspection.Delivery.ClosedPullRequests)
}

type workflowCLIView struct {
	Name                 string                   `json:"name"`
	WorkspaceName        string                   `json:"workspaceName"`
	SchemaVersion        string                   `json:"schemaVersion"`
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

func workflowRunsCmd() *cobra.Command {
	var workspace string
	var limit int
	cmd := &cobra.Command{
		Use:   "runs <name>",
		Short: "Show recent runs for a workflow",
		Long:  "List recent execution history for a workflow, including cron and manual triggers.",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return runWorkflowRuns(workspace, args[0], limit)
		},
	}
	cmd.Flags().StringVar(&workspace, "workspace", "default", "workspace name")
	cmd.Flags().IntVar(&limit, "limit", 50, "maximum number of runs to show")
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
		if v2.IsV2(workflow.SchemaVersion) {
			continue // strict v2 validation happened while reading the authored document
		}
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

func workflowRmCmd() *cobra.Command {
	var workspace string
	cmd := &cobra.Command{
		Use:     "rm <name>",
		Aliases: []string{"delete", "remove"},
		Short:   "Remove a workflow from a workspace",
		Args:    cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return runWorkflowRm(workspace, args[0])
		},
	}
	cmd.Flags().StringVar(&workspace, "workspace", "default", "workspace name")
	return cmd
}

func runWorkflowRm(workspace, name string) error {
	hubURL, clawToken, err := resolveHubConn()
	if err != nil {
		return err
	}
	path := fmt.Sprintf("/api/workspaces/%s/workflows/%s", url.PathEscape(workspace), url.PathEscape(name))
	req, _ := http.NewRequest(http.MethodDelete, hubURL+path, nil)
	req.Header.Set("Authorization", "Bearer "+clawToken)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return fmt.Errorf("remove workflow failed: %w", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode == http.StatusNotFound {
		return fmt.Errorf("workflow %q not found in workspace %q", name, workspace)
	}
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("hub returned %d: %s", resp.StatusCode, strings.TrimSpace(string(body)))
	}
	fmt.Printf("Removed workflow %q from workspace %q.\n", name, workspace)
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
		version, err := v2.DetectSchemaVersion(data)
		if err != nil {
			return nil, fmt.Errorf("parse %s: %w", path, err)
		}
		if v2.IsV2(version) {
			resolved, err := v2.ParseAndValidateWorkflow(data)
			if err != nil {
				return nil, fmt.Errorf("validate %s: %w", path, err)
			}
			enabled := resolved.Workflow.Enabled
			workflows = append(workflows, &types.WorkflowConfig{
				SchemaVersion: "2",
				Name:          resolved.Workflow.Name,
				Enabled:       &enabled,
				RawConfig:     string(data),
			})
			continue
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

func runWorkflowRuns(workspace, name string, limit int) error {
	if limit <= 0 {
		limit = 50
	}
	if limit > 200 {
		limit = 200
	}

	view, err := fetchWorkflowView(workspace, name)
	if err != nil {
		return err
	}
	if v2.IsV2(view.SchemaVersion) {
		return runWorkflowV2Runs(workspace, name, limit)
	}
	return runWorkflowV1Runs(workspace, name, limit)
}

func runWorkflowV1Runs(workspace, name string, limit int) error {
	hubURL, clawToken, err := resolveHubConn()
	if err != nil {
		return err
	}

	path := fmt.Sprintf("/api/workspaces/%s/workflows/%s/cron/runs?limit=%d", url.PathEscape(workspace), url.PathEscape(name), limit)
	req, _ := http.NewRequest(http.MethodGet, hubURL+path, nil)
	req.Header.Set("Authorization", "Bearer "+clawToken)

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return fmt.Errorf("fetch workflow runs failed: %w", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("hub returned %d: %s", resp.StatusCode, strings.TrimSpace(string(body)))
	}

	var result struct {
		Runs  []types.WorkflowRun `json:"runs"`
		Count int                 `json:"count"`
	}
	if err := json.Unmarshal(body, &result); err != nil {
		return fmt.Errorf("decode response: %w", err)
	}

	if jsonOut {
		return json.NewEncoder(os.Stdout).Encode(result.Runs)
	}
	if len(result.Runs) == 0 {
		fmt.Printf("No runs found for workflow %q in workspace %q.\n", name, workspace)
		return nil
	}

	w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintln(w, "RUN ID\tSTATUS\tTRIGGER\tSTARTED\tFINISHED\tRESULT\tAGENT")
	for _, run := range result.Runs {
		started := "—"
		if run.StartedAt != nil && !run.StartedAt.IsZero() {
			started = run.StartedAt.Format("2006-01-02 15:04:05")
		}
		finished := "—"
		if run.FinishedAt != nil && !run.FinishedAt.IsZero() {
			finished = run.FinishedAt.Format("2006-01-02 15:04:05")
		}
		clawID := "—"
		if run.ClawID != "" {
			clawID = shortID(run.ClawID)
		}
		resultText := sanitizeWorkflowResultForTable(run.Result, 80)
		fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\t%s\t%s\n",
			run.ID,
			run.Status,
			run.TriggerType,
			started,
			finished,
			resultText,
			clawID,
		)
	}
	w.Flush()
	fmt.Printf("\nShowing %d run(s).\n", result.Count)
	return nil
}

type workflowV2RunHistoryRow struct {
	RunID             string     `json:"run_id"`
	AttemptID         string     `json:"attempt_id"`
	AttemptNumber     int        `json:"attempt_number"`
	RunStatus         string     `json:"run_status"`
	AttemptStatus     string     `json:"attempt_status"`
	DisplayPhase      string     `json:"display_phase"`
	TriggerType       string     `json:"trigger_type"`
	ClawID            string     `json:"claw_id,omitempty"`
	StartedAt         time.Time  `json:"started_at"`
	UpdatedAt         time.Time  `json:"updated_at"`
	FinishedAt        *time.Time `json:"finished_at,omitempty"`
	AttemptFinishedAt *time.Time `json:"attempt_finished_at,omitempty"`
}

func runWorkflowV2Runs(workspace, name string, limit int) error {
	hubURL, clawToken, err := resolveHubConn()
	if err != nil {
		return err
	}

	path := fmt.Sprintf("/api/v2/workspaces/%s/workflows/%s/runs?limit=%d", url.PathEscape(workspace), url.PathEscape(name), limit)
	req, _ := http.NewRequest(http.MethodGet, hubURL+path, nil)
	req.Header.Set("Authorization", "Bearer "+clawToken)

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return fmt.Errorf("fetch workflow runs failed: %w", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("hub returned %d: %s", resp.StatusCode, strings.TrimSpace(string(body)))
	}

	var result struct {
		Runs  []workflowV2RunHistoryRow `json:"runs"`
		Count int                       `json:"count"`
	}
	if err := json.Unmarshal(body, &result); err != nil {
		return fmt.Errorf("decode response: %w", err)
	}

	if jsonOut {
		return json.NewEncoder(os.Stdout).Encode(result.Runs)
	}
	if len(result.Runs) == 0 {
		fmt.Printf("No runs found for workflow %q in workspace %q.\n", name, workspace)
		return nil
	}

	w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintln(w, "RUN ID\tATTEMPT\tSTATUS\tTRIGGER\tSTARTED\tFINISHED\tRESULT\tAGENT")
	for _, run := range result.Runs {
		started := "—"
		if !run.StartedAt.IsZero() {
			started = run.StartedAt.Format("2006-01-02 15:04:05")
		}
		finished := "—"
		if run.FinishedAt != nil && !run.FinishedAt.IsZero() {
			finished = run.FinishedAt.Format("2006-01-02 15:04:05")
		}
		clawID := "—"
		if run.ClawID != "" {
			clawID = shortID(run.ClawID)
		}
		display := string(run.DisplayPhase)
		if display == "" {
			display = "—"
		}
		status := run.RunStatus
		if run.AttemptStatus != "" && run.AttemptStatus != run.RunStatus {
			status = fmt.Sprintf("%s (%s)", run.RunStatus, run.AttemptStatus)
		}
		fmt.Fprintf(w, "%s\t%d\t%s\t%s\t%s\t%s\t%s\t%s\n",
			run.RunID,
			run.AttemptNumber,
			status,
			run.TriggerType,
			started,
			finished,
			display,
			clawID,
		)
	}
	w.Flush()
	fmt.Printf("\nShowing %d run(s).\n", result.Count)
	return nil
}

// sanitizeWorkflowResultForTable makes a workflow result safe for tabwriter output.
// It strips control characters (tabs, newlines, carriage returns), collapses whitespace,
// and truncates by rune length so multibyte characters are not sliced in half.
func sanitizeWorkflowResultForTable(result string, maxRunes int) string {
	if result == "" {
		return "—"
	}
	replacer := strings.NewReplacer("\t", " ", "\n", " ", "\r", " ")
	s := replacer.Replace(result)
	for strings.Contains(s, "  ") {
		s = strings.ReplaceAll(s, "  ", " ")
	}
	s = strings.TrimSpace(s)
	runes := []rune(s)
	if len(runes) > maxRunes {
		return string(runes[:maxRunes-3]) + "..."
	}
	return s
}

func workflowLogsCmd() *cobra.Command {
	var workspace, attempt string
	cmd := &cobra.Command{
		Use:   "logs <workflow> <run-id>",
		Short: "Show detailed agent logs for a workflow run",
		Long:  "Show detailed agent activity logs for a workflow run by run ID. For v2 workflows, use --attempt to read logs for a specific attempt.",
		Args:  cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			return runWorkflowLogs(workspace, args[0], args[1], attempt)
		},
	}
	cmd.Flags().StringVar(&workspace, "workspace", "default", "workspace name")
	cmd.Flags().StringVar(&attempt, "attempt", "", "attempt id (v2 workflows only)")
	return cmd
}

func runWorkflowLogs(workspace, name, runID, attemptID string) error {
	view, err := fetchWorkflowView(workspace, name)
	if err != nil {
		return err
	}
	if v2.IsV2(view.SchemaVersion) {
		return runWorkflowV2Logs(workspace, name, runID, attemptID)
	}
	if attemptID != "" {
		return fmt.Errorf("--attempt is only supported for v2 workflows")
	}
	return runWorkflowV1Logs(workspace, name, runID)
}

func runWorkflowV1Logs(workspace, name, runID string) error {
	hubURL, clawToken, err := resolveHubConn()
	if err != nil {
		return err
	}

	runPath := fmt.Sprintf("/api/workspaces/%s/workflows/%s/cron/runs/%s", url.PathEscape(workspace), url.PathEscape(name), url.PathEscape(runID))
	runReq, _ := http.NewRequest(http.MethodGet, hubURL+runPath, nil)
	runReq.Header.Set("Authorization", "Bearer "+clawToken)
	runResp, err := http.DefaultClient.Do(runReq)
	if err != nil {
		return fmt.Errorf("fetch workflow run failed: %w", err)
	}
	defer runResp.Body.Close()
	runBody, _ := io.ReadAll(runResp.Body)
	if runResp.StatusCode == http.StatusNotFound {
		return fmt.Errorf("workflow run %s not found", runID)
	}
	if runResp.StatusCode != http.StatusOK {
		return fmt.Errorf("hub returned %d: %s", runResp.StatusCode, strings.TrimSpace(string(runBody)))
	}
	var run types.WorkflowRun
	if err := json.Unmarshal(runBody, &run); err != nil {
		return fmt.Errorf("decode run: %w", err)
	}
	if run.ClawID == "" {
		return fmt.Errorf("workflow run %s is not linked to an agent", runID)
	}
	return fetchAndPrintActivityLogs(hubURL, clawToken, runID, run.ClawID, run.Status)
}

func runWorkflowV2Logs(workspace, name, runID, attemptID string) error {
	hubURL, clawToken, err := resolveHubConn()
	if err != nil {
		return err
	}

	var logPath string
	if attemptID != "" {
		logPath = fmt.Sprintf("/api/v2/workflow-runs/%s/attempts/%s/logs", url.PathEscape(runID), url.PathEscape(attemptID))
	} else {
		logPath = fmt.Sprintf("/api/v2/workflow-runs/%s/logs", url.PathEscape(runID))
	}
	logReq, _ := http.NewRequest(http.MethodGet, hubURL+logPath, nil)
	logReq.Header.Set("Authorization", "Bearer "+clawToken)
	logResp, err := http.DefaultClient.Do(logReq)
	if err != nil {
		return fmt.Errorf("fetch agent logs failed: %w", err)
	}
	defer logResp.Body.Close()
	logBody, _ := io.ReadAll(logResp.Body)
	if logResp.StatusCode == http.StatusNotFound {
		return fmt.Errorf("workflow run %s not found", runID)
	}
	if logResp.StatusCode != http.StatusOK {
		return fmt.Errorf("hub returned %d: %s", logResp.StatusCode, strings.TrimSpace(string(logBody)))
	}
	var messages []types.HubMessage
	if err := json.Unmarshal(logBody, &messages); err != nil {
		return fmt.Errorf("decode logs: %w", err)
	}
	return printActivityLogs(runID, attemptID, "", "", messages)
}

func fetchAndPrintActivityLogs(hubURL, clawToken, runID, clawID, status string) error {
	msgPath := fmt.Sprintf("/api/messages/%s/activity?limit=500", url.PathEscape(clawID))
	msgReq, _ := http.NewRequest(http.MethodGet, hubURL+msgPath, nil)
	msgReq.Header.Set("Authorization", "Bearer "+clawToken)
	msgResp, err := http.DefaultClient.Do(msgReq)
	if err != nil {
		return fmt.Errorf("fetch agent logs failed: %w", err)
	}
	defer msgResp.Body.Close()
	msgBody, _ := io.ReadAll(msgResp.Body)
	if msgResp.StatusCode != http.StatusOK {
		return fmt.Errorf("hub returned %d: %s", msgResp.StatusCode, strings.TrimSpace(string(msgBody)))
	}
	var messages []types.HubMessage
	if err := json.Unmarshal(msgBody, &messages); err != nil {
		return fmt.Errorf("decode logs: %w", err)
	}
	return printActivityLogs(runID, "", clawID, status, messages)
}

func printActivityLogs(runID, attemptID, clawID, status string, messages []types.HubMessage) error {
	if jsonOut {
		return json.NewEncoder(os.Stdout).Encode(messages)
	}
	if len(messages) == 0 {
		if attemptID != "" {
			fmt.Printf("No agent logs found for run %s attempt %s.\n", runID, shortID(attemptID))
		} else {
			fmt.Printf("No agent logs found for run %s.\n", runID)
		}
		return nil
	}
	if attemptID != "" {
		fmt.Printf("Agent logs for run %s attempt %s", runID, shortID(attemptID))
	} else {
		fmt.Printf("Agent logs for run %s", runID)
	}
	if clawID != "" {
		fmt.Printf(" (agent %s", shortID(clawID))
		if status != "" {
			fmt.Printf(", status %s", status)
		}
		fmt.Print(")")
	} else if status != "" {
		fmt.Printf(" (status %s)", status)
	}
	fmt.Print(":\n\n")
	printCollapsedActivityMessages(messages)
	return nil
}

// printCollapsedActivityMessages prints activity messages, collapsing bursts of
// identical or prefix-extended reasoning messages so repeated model activity
// does not drown out tool events and errors.
func printCollapsedActivityMessages(messages []types.HubMessage) {
	var pending *types.HubMessage
	var pendingActivity *agentActivity
	pendingCount := 0

	flush := func() {
		if pending == nil {
			return
		}
		printActivityMessage(*pending, pendingCount-1)
		pending = nil
		pendingActivity = nil
		pendingCount = 0
	}

	for i := range messages {
		msg := &messages[i]
		activity, ok := parseAgentActivity(*msg)
		if !ok {
			flush()
			printActivityMessage(*msg, 0)
			continue
		}
		if pending == nil {
			pending = msg
			pendingActivity = &activity
			pendingCount = 1
			continue
		}
		if canCollapseActivity(*pendingActivity, activity) {
			if len(activity.Message) > len(pendingActivity.Message) {
				pending = msg
				pendingActivity = &activity
			}
			pendingCount++
			continue
		}
		flush()
		pending = msg
		pendingActivity = &activity
		pendingCount = 1
	}
	flush()
}

func parseAgentActivity(msg types.HubMessage) (agentActivity, bool) {
	if !strings.HasPrefix(msg.Format, "activity:") {
		return agentActivity{}, false
	}
	var activity agentActivity
	if err := json.Unmarshal([]byte(strings.TrimPrefix(msg.Format, "activity:")), &activity); err != nil {
		return agentActivity{}, false
	}
	return activity, true
}

// canCollapseActivity returns true when two activities are the same kind of
// event and one message is a prefix of the other. This catches the common case
// where a model streams its reasoning as a series of ever-longer messages.
func canCollapseActivity(a, b agentActivity) bool {
	return a.Kind == b.Kind &&
		a.Tool == b.Tool &&
		a.Phase == b.Phase &&
		a.Command == b.Command &&
		a.Path == b.Path &&
		a.URL == b.URL &&
		a.Error == b.Error &&
		a.Detail == b.Detail &&
		(a.Message == b.Message ||
			strings.HasPrefix(a.Message, b.Message) ||
			strings.HasPrefix(b.Message, a.Message))
}

type agentActivity struct {
	Kind           string `json:"kind"`
	Tool           string `json:"tool"`
	Phase          string `json:"phase"`
	Detail         string `json:"detail"`
	Command        string `json:"command"`
	Path           string `json:"path"`
	URL            string `json:"url"`
	Message        string `json:"message"`
	Error          string `json:"error"`
	SubagentName   string `json:"subagent_name,omitempty"`
	SubagentType   string `json:"subagent_type,omitempty"`
	SubagentModel  string `json:"subagent_model,omitempty"`
	SubagentPrompt string `json:"subagent_prompt,omitempty"`
}

func printActivityMessage(msg types.HubMessage, collapsedSimilar int) {
	ts := msg.CreatedAt.Format("2006-01-02 15:04:05")
	if !strings.HasPrefix(msg.Format, "activity:") {
		fmt.Printf("%s  %s\n", ts, msg.Content)
		return
	}
	var activity agentActivity
	if err := json.Unmarshal([]byte(strings.TrimPrefix(msg.Format, "activity:")), &activity); err != nil {
		fmt.Printf("%s  %s\n", ts, msg.Content)
		return
	}
	label := activity.Tool
	if label == "" {
		label = activity.Kind
	}
	if label == "" {
		label = "activity"
	}
	fmt.Printf("%s  [%s]", ts, label)
	if activity.Phase != "" {
		fmt.Printf(" (%s)", activity.Phase)
	}
	var parts []string
	if activity.Command != "" {
		parts = append(parts, fmt.Sprintf("cmd: %s", activity.Command))
	}
	if activity.Path != "" {
		parts = append(parts, fmt.Sprintf("path: %s", activity.Path))
	}
	if activity.URL != "" {
		parts = append(parts, fmt.Sprintf("url: %s", activity.URL))
	}
	if activity.Detail != "" {
		parts = append(parts, activity.Detail)
	}
	if activity.Message != "" {
		parts = append(parts, activity.Message)
	}
	if activity.Error != "" {
		parts = append(parts, fmt.Sprintf("error: %s", activity.Error))
	}
	if collapsedSimilar > 0 {
		parts = append(parts, fmt.Sprintf("(+ %d similar)", collapsedSimilar))
	}
	if len(parts) == 0 {
		fmt.Println(" " + msg.Content)
	} else {
		fmt.Println(" " + strings.Join(parts, " | "))
	}
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
