package cmd

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/elasticclaw/elasticclaw/pkg/workflowsetup"
	"github.com/spf13/cobra"
)

type workflowCreateOptions struct {
	Pattern              string
	Workspace            string
	Name                 string
	Repository           string
	Output               string
	Manual               bool
	CreateWorkspace      bool
	ConcurrencyGroup     string
	Labels               []string
	Event                string
	States               []string
	Labelers             []string
	AssignedTo           string
	TriggerLabel         string
	WorkingLabel         string
	ReviewLabel          string
	DoneLabel            string
	ClosedLabel          string
	IntegrationWorkspace string
	Team                 string
	TriggerStatus        string
	WorkingStatus        string
	PROpenedStatus       string
	MergedStatus         string
	ClosedNoMergeStatus  string
	IncludePreCommit     bool
	PreCommitCommand     string
	PreCommitReadySignal string
	DoneSignal           string
}

type workflowValidateOptions struct {
	Workspace string
	File      string
}

type workflowCreateResult struct {
	WorkflowName string                     `json:"workflowName"`
	Path         string                     `json:"path"`
	ConfigHash   string                     `json:"configHash"`
	Warnings     []workflowsetup.Diagnostic `json:"warnings"`
}

type workflowSetupResult struct {
	Created    workflowCreateResult           `json:"created"`
	Validation workflowsetup.ValidateResponse `json:"validation"`
}

func workflowCreateCmd() *cobra.Command {
	opts := workflowCreateOptions{Manual: true}
	cmd := &cobra.Command{
		Use:   "create <pattern>",
		Short: "Render a workflow setup pattern to a local workflow YAML file",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			opts.Pattern = args[0]
			return runWorkflowCreate(cmd.OutOrStdout(), opts)
		},
	}
	addWorkflowSetupFlags(cmd, &opts)
	return cmd
}

func workflowValidateCmd() *cobra.Command {
	opts := workflowValidateOptions{}
	cmd := &cobra.Command{
		Use:   "validate --workspace <name> <file>",
		Short: "Validate a local workflow YAML file",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			opts.File = args[0]
			return runWorkflowValidate(cmd.OutOrStdout(), opts)
		},
	}
	cmd.Flags().StringVar(&opts.Workspace, "workspace", "", "workspace name [required]")
	return cmd
}

func workflowSetupCmd() *cobra.Command {
	opts := workflowCreateOptions{Manual: true}
	cmd := &cobra.Command{
		Use:   "setup <pattern>",
		Short: "Create and validate a local workflow YAML file without writing hub secrets",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			opts.Pattern = args[0]
			return runWorkflowSetup(cmd.OutOrStdout(), opts)
		},
	}
	addWorkflowSetupFlags(cmd, &opts)
	return cmd
}

func addWorkflowSetupFlags(cmd *cobra.Command, opts *workflowCreateOptions) {
	cmd.Flags().StringVar(&opts.Workspace, "workspace", "", "workspace name [required]")
	cmd.Flags().StringVar(&opts.Name, "name", "", "workflow name")
	cmd.Flags().StringVar(&opts.Repository, "repo", "", "GitHub repository in owner/repo format for github-issue")
	cmd.Flags().StringVar(&opts.Output, "output", "", "output YAML path")
	cmd.Flags().BoolVar(&opts.Manual, "manual", true, "enable manual trigger inputs")
	cmd.Flags().BoolVar(&opts.CreateWorkspace, "create-workspace", false, "create a minimal local workspace directory if missing")
	cmd.Flags().StringVar(&opts.ConcurrencyGroup, "concurrency-group", "", "workflow concurrency group")
	cmd.Flags().StringArrayVar(&opts.Labels, "label", nil, "GitHub issue label that can trigger the workflow (can be repeated)")
	cmd.Flags().StringVar(&opts.Event, "event", "", "trigger event override")
	cmd.Flags().StringArrayVar(&opts.States, "state", nil, "issue or tracker state allowed to trigger the workflow (can be repeated)")
	cmd.Flags().StringArrayVar(&opts.Labelers, "labeler", nil, "GitHub user allowed to trigger by labeling (can be repeated)")
	cmd.Flags().StringVar(&opts.AssignedTo, "assigned-to", "", "GitHub assignee filter for issue triggers")
	cmd.Flags().StringVar(&opts.TriggerLabel, "trigger-label", "", "GitHub trigger label removed when work starts")
	cmd.Flags().StringVar(&opts.WorkingLabel, "working-label", "", "GitHub label added when work starts")
	cmd.Flags().StringVar(&opts.ReviewLabel, "review-label", "", "GitHub label added when a pull request opens")
	cmd.Flags().StringVar(&opts.DoneLabel, "done-label", "", "GitHub label added when a pull request merges")
	cmd.Flags().StringVar(&opts.ClosedLabel, "closed-label", "", "GitHub label added when a pull request closes without merge")
	cmd.Flags().StringVar(&opts.IntegrationWorkspace, "integration-workspace", "", "issue tracker workspace for linear-status or shortcut-status")
	cmd.Flags().StringVar(&opts.Team, "team", "", "Linear team key filter")
	cmd.Flags().StringVar(&opts.TriggerStatus, "trigger-status", "", "Linear or Shortcut status/state that starts the workflow")
	cmd.Flags().StringVar(&opts.WorkingStatus, "working-status", "", "status/state to move the issue to when work starts")
	cmd.Flags().StringVar(&opts.PROpenedStatus, "pr-opened-status", "", "status/state to move the issue to when a pull request opens")
	cmd.Flags().StringVar(&opts.MergedStatus, "merged-status", "", "status/state to move the issue to when a pull request merges")
	cmd.Flags().StringVar(&opts.ClosedNoMergeStatus, "closed-no-merge-status", "", "status/state to move the issue to when a pull request closes without merge")
	cmd.Flags().BoolVar(&opts.IncludePreCommit, "include-pre-commit", false, "add a pre-commit command stage before completion or PR handoff")
	cmd.Flags().StringVar(&opts.PreCommitCommand, "pre-commit-command", "", "command to run in the pre-commit stage")
	cmd.Flags().StringVar(&opts.PreCommitReadySignal, "pre-commit-ready-signal", "", "message token that enters the pre-commit stage")
	cmd.Flags().StringVar(&opts.DoneSignal, "done-signal", "", "message token that marks manual workflows complete")
}

func runWorkflowCreate(out io.Writer, opts workflowCreateOptions) error {
	result, err := createWorkflowFile(opts)
	if err != nil {
		return err
	}
	if jsonOut {
		return json.NewEncoder(out).Encode(result)
	}
	printWorkflowCreateResult(out, result)
	return nil
}

func runWorkflowSetup(out io.Writer, opts workflowCreateOptions) error {
	created, err := createWorkflowFile(opts)
	if err != nil {
		return err
	}
	resp, err := validateWorkflowFile(opts.Workspace, created.Path)
	if jsonOut {
		encodeErr := json.NewEncoder(out).Encode(workflowSetupResult{
			Created:    created,
			Validation: resp,
		})
		if encodeErr != nil {
			return encodeErr
		}
		return err
	}

	printWorkflowCreateResult(out, created)
	printWorkflowValidationResult(out, created.Path, resp)
	if err != nil {
		return err
	}
	return workflowValidationResponseError(resp)
}

func runWorkflowValidate(out io.Writer, opts workflowValidateOptions) error {
	resp, err := validateWorkflowFile(opts.Workspace, opts.File)
	if err != nil {
		return err
	}
	if jsonOut {
		if err := json.NewEncoder(out).Encode(resp); err != nil {
			return err
		}
	} else {
		printWorkflowValidationResult(out, opts.File, resp)
	}
	return workflowValidationResponseError(resp)
}

func createWorkflowFile(opts workflowCreateOptions) (workflowCreateResult, error) {
	workspace := strings.TrimSpace(opts.Workspace)
	if workspace == "" {
		return workflowCreateResult{}, fmt.Errorf("--workspace is required")
	}
	if err := validateWorkspaceNameArg(workspace); err != nil {
		return workflowCreateResult{}, err
	}
	if opts.Pattern == workflowsetup.PatternGitHubIssue && strings.TrimSpace(opts.Repository) == "" {
		return workflowCreateResult{}, fmt.Errorf("--repo is required for github-issue")
	}
	if err := ensureLocalWorkflowWorkspace(workspace, opts.CreateWorkspace); err != nil {
		return workflowCreateResult{}, err
	}

	resp, err := workflowsetup.Render(workflowsetup.RenderRequest{
		WorkflowName: opts.Name,
		PatternID:    opts.Pattern,
		Config:       workflowRenderConfig(opts),
	})
	if err != nil {
		return workflowCreateResult{}, err
	}

	outputPath := strings.TrimSpace(opts.Output)
	if outputPath == "" {
		outputPath = filepath.Join(localWorkflowWorkspaceDir(workspace), "workflows", resp.WorkflowName+".yaml")
	}
	if err := os.MkdirAll(filepath.Dir(outputPath), 0755); err != nil {
		return workflowCreateResult{}, fmt.Errorf("create workflow output directory: %w", err)
	}
	if err := os.WriteFile(outputPath, []byte(resp.Config), 0644); err != nil {
		return workflowCreateResult{}, fmt.Errorf("write workflow YAML: %w", err)
	}

	return workflowCreateResult{
		WorkflowName: resp.WorkflowName,
		Path:         outputPath,
		ConfigHash:   resp.ConfigHash,
		Warnings:     resp.Warnings,
	}, nil
}

func workflowRenderConfig(opts workflowCreateOptions) map[string]interface{} {
	config := map[string]interface{}{
		"enableManualTrigger": opts.Manual,
	}
	if repository := strings.TrimSpace(opts.Repository); repository != "" {
		config["repository"] = repository
	}
	if group := strings.TrimSpace(opts.ConcurrencyGroup); group != "" {
		config["concurrencyGroup"] = group
	}
	if len(opts.Labels) > 0 {
		if labels := trimmedStrings(opts.Labels); len(labels) > 0 {
			config["labels"] = labels
		}
	}
	if event := strings.TrimSpace(opts.Event); event != "" {
		config["event"] = event
	}
	if states := trimmedStrings(opts.States); len(states) > 0 {
		config["states"] = states
	}
	if labelers := trimmedStrings(opts.Labelers); len(labelers) > 0 {
		config["labelers"] = labelers
	}
	if assignedTo := strings.TrimSpace(opts.AssignedTo); assignedTo != "" {
		config["assignedTo"] = assignedTo
	}
	if triggerLabel := strings.TrimSpace(opts.TriggerLabel); triggerLabel != "" {
		config["triggerLabel"] = triggerLabel
	}
	if workingLabel := strings.TrimSpace(opts.WorkingLabel); workingLabel != "" {
		config["workingLabel"] = workingLabel
	}
	if reviewLabel := strings.TrimSpace(opts.ReviewLabel); reviewLabel != "" {
		config["reviewLabel"] = reviewLabel
	}
	if doneLabel := strings.TrimSpace(opts.DoneLabel); doneLabel != "" {
		config["doneLabel"] = doneLabel
	}
	if closedLabel := strings.TrimSpace(opts.ClosedLabel); closedLabel != "" {
		config["closedLabel"] = closedLabel
	}
	if integrationWorkspace := strings.TrimSpace(opts.IntegrationWorkspace); integrationWorkspace != "" {
		config["workspace"] = integrationWorkspace
	}
	if team := strings.TrimSpace(opts.Team); team != "" {
		config["team"] = team
	}
	if triggerStatus := strings.TrimSpace(opts.TriggerStatus); triggerStatus != "" {
		config["triggerStatus"] = triggerStatus
	}
	if workingStatus := strings.TrimSpace(opts.WorkingStatus); workingStatus != "" {
		config["workingStatus"] = workingStatus
	}
	if prOpenedStatus := strings.TrimSpace(opts.PROpenedStatus); prOpenedStatus != "" {
		config["prOpenedStatus"] = prOpenedStatus
	}
	if mergedStatus := strings.TrimSpace(opts.MergedStatus); mergedStatus != "" {
		config["mergedStatus"] = mergedStatus
	}
	if closedNoMergeStatus := strings.TrimSpace(opts.ClosedNoMergeStatus); closedNoMergeStatus != "" {
		config["closedNoMergeStatus"] = closedNoMergeStatus
	}
	if opts.IncludePreCommit {
		config["includePreCommit"] = true
	}
	if preCommitCommand := strings.TrimSpace(opts.PreCommitCommand); preCommitCommand != "" {
		config["preCommitCommand"] = preCommitCommand
	}
	if readySignal := strings.TrimSpace(opts.PreCommitReadySignal); readySignal != "" {
		config["preCommitReadySignal"] = readySignal
	}
	if doneSignal := strings.TrimSpace(opts.DoneSignal); doneSignal != "" {
		config["doneSignal"] = doneSignal
	}
	return config
}

func trimmedStrings(values []string) []string {
	trimmed := make([]string, 0, len(values))
	for _, value := range values {
		value = strings.TrimSpace(value)
		if value != "" {
			trimmed = append(trimmed, value)
		}
	}
	return trimmed
}

func validateWorkflowFile(workspace, path string) (workflowsetup.ValidateResponse, error) {
	workspace = strings.TrimSpace(workspace)
	if workspace == "" {
		return workflowsetup.ValidateResponse{}, fmt.Errorf("--workspace is required")
	}
	if err := validateWorkspaceNameArg(workspace); err != nil {
		return workflowsetup.ValidateResponse{}, err
	}

	data, err := os.ReadFile(path)
	if err != nil {
		return workflowsetup.ValidateResponse{}, fmt.Errorf("read workflow YAML: %w", err)
	}
	workspaceConfig, err := readLocalWorkflowWorkspaceConfig(workspace)
	if err != nil {
		return workflowsetup.ValidateResponse{}, err
	}

	return workflowsetup.ValidateStatic(workflowsetup.ValidateRequest{
		WorkflowName:    workflowNameFromPath(path),
		Config:          string(data),
		WorkspaceConfig: workspaceConfig,
	}), nil
}

func ensureLocalWorkflowWorkspace(workspace string, create bool) error {
	dir := localWorkflowWorkspaceDir(workspace)
	info, err := os.Stat(dir)
	if err == nil {
		if !info.IsDir() {
			return fmt.Errorf("workspace path %s exists but is not a directory", dir)
		}
		return nil
	}
	if err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("inspect workspace directory: %w", err)
	}
	if !create {
		return fmt.Errorf("workspace directory %s does not exist; run `elasticclaw workspace create --name %s` or pass --create-workspace", dir, workspace)
	}
	if err := os.MkdirAll(filepath.Join(dir, "workflows"), 0755); err != nil {
		return fmt.Errorf("create workspace directory: %w", err)
	}
	configPath := filepath.Join(dir, "elasticclaw-config.yaml")
	if _, err := os.Stat(configPath); err == nil {
		return nil
	} else if err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("inspect workspace config: %w", err)
	}
	config := fmt.Sprintf("schema_version: v1\nname: %s\n", workspace)
	if err := os.WriteFile(configPath, []byte(config), 0644); err != nil {
		return fmt.Errorf("write minimal workspace config: %w", err)
	}
	return nil
}

func readLocalWorkflowWorkspaceConfig(workspace string) (string, error) {
	path := filepath.Join(localWorkflowWorkspaceDir(workspace), "elasticclaw-config.yaml")
	data, err := os.ReadFile(path)
	if err == nil {
		return string(data), nil
	}
	if os.IsNotExist(err) {
		return "", nil
	}
	return "", fmt.Errorf("read workspace config %s: %w", path, err)
}

func localWorkflowWorkspaceDir(workspace string) string {
	return filepath.Join(".elasticclaw", "workspaces", workspace)
}

func validateWorkspaceNameArg(workspace string) error {
	if strings.ContainsAny(workspace, `/\`) {
		return fmt.Errorf("--workspace must be a workspace name, not a path")
	}
	return nil
}

func workflowNameFromPath(path string) string {
	base := filepath.Base(path)
	base = strings.TrimSuffix(base, filepath.Ext(base))
	return base
}

func printWorkflowCreateResult(out io.Writer, result workflowCreateResult) {
	fmt.Fprintf(out, "Created workflow %q at %s\n", result.WorkflowName, result.Path)
	for _, warning := range result.Warnings {
		fmt.Fprintf(out, "Warning: %s: %s\n", warning.FieldPath, warning.Title)
	}
}

func printWorkflowValidationResult(out io.Writer, path string, resp workflowsetup.ValidateResponse) {
	if resp.OK {
		fmt.Fprintf(out, "Workflow validation OK for %s\n", path)
	} else {
		fmt.Fprintf(out, "Workflow validation failed for %s\n", path)
	}
	fmt.Fprintf(out, "Summary: %d critical, %d warning, %d info\n", resp.Summary.Critical, resp.Summary.Warning, resp.Summary.Info)
	for _, check := range resp.Checks {
		field := check.FieldPath
		if field == "" {
			field = check.Category
		}
		fmt.Fprintf(out, "- %s %s: %s", check.Severity, field, check.Title)
		if check.Detail != "" {
			fmt.Fprintf(out, " - %s", check.Detail)
		}
		fmt.Fprintln(out)
	}
}

type workflowValidationError struct {
	critical int
}

func (e workflowValidationError) Error() string {
	return fmt.Sprintf("workflow validation failed: %d critical diagnostic(s)", e.critical)
}

func workflowValidationResponseError(resp workflowsetup.ValidateResponse) error {
	if resp.Summary.Critical > 0 {
		return workflowValidationError{critical: resp.Summary.Critical}
	}
	return nil
}
