package cmd

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"text/tabwriter"

	"github.com/elasticclaw/elasticclaw/pkg/config"
	"github.com/elasticclaw/elasticclaw/pkg/types"
	"github.com/spf13/cobra"
	"gopkg.in/yaml.v3"
)

func WorkspaceCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "workspace",
		Short: "Manage elasticclaw workspaces",
	}
	cmd.AddCommand(workspaceCreateCmd())
	cmd.AddCommand(workspacePushCmd())
	cmd.AddCommand(workspaceListCmd())
	cmd.AddCommand(workspaceShowCmd())
	cmd.AddCommand(workspaceRmCmd())
	return cmd
}

type workspaceCLIView struct {
	Name      string              `json:"name"`
	Source    string              `json:"source"`
	Access    workspaceAccessView `json:"access"`
	Workflows []workflowCLIView   `json:"workflows"`
}

type workspaceAccessView struct {
	Repositories   []string `json:"repositories"`
	Secrets        []string `json:"secrets"`
	WebhookSecrets []string `json:"webhookSecrets"`
}

func workspaceCreateCmd() *cobra.Command {
	var name string
	cmd := &cobra.Command{
		Use:   "create",
		Short: "Bootstrap a new workspace directory",
		RunE: func(cmd *cobra.Command, args []string) error {
			if name == "" {
				return fmt.Errorf("--name is required")
			}
			return runWorkspaceCreate(name)
		},
	}
	cmd.Flags().StringVar(&name, "name", "", "workspace name [required]")
	return cmd
}

func runWorkspaceCreate(name string) error {
	name = strings.ToLower(strings.ReplaceAll(name, " ", "-"))
	dir := filepath.Join(".elasticclaw", "workspaces", name)
	if err := os.MkdirAll(dir, 0755); err != nil {
		return fmt.Errorf("create workspace directory: %w", err)
	}

	workspaceYAML := fmt.Sprintf(`name: %s
repositories: []
secrets: []
webhook_secrets: []
`, name)
	workspaceFiles := map[string]string{
		"elasticclaw-config.yaml": `schema_version: v1
provider: replicated

# Optional runtime overrides:
# instance_type: r1.large
# ttl: 48h
# default_model: anthropic/claude-sonnet-4-6
# llm_key: anthropic-prod

# Optional workspace features:
# nix: false
# docker: false
# tags: ["backend"]
# color: teal

# Optional tool and repository access:
# github:
#   repos:
#     - repo: owner/repo
#       permissions: write

# Optional environment secrets:
# secret_refs:
#   GITHUB_TOKEN: github_app
`,
		"AGENTS.md": `# Agent Instructions

Work in this workspace as a focused coding agent.

When the task is complete, open a pull request when appropriate and report the result clearly.
`,
		"TOOLS.md": `# Tools

Use the tools and credentials made available by this workspace.

Prefer small, verifiable changes. Run the relevant tests before reporting completion.
`,
		"SOUL.md": `# Persona

You are a pragmatic software engineer. Be direct, careful, and useful.
`,
		"IDENTITY.md": fmt.Sprintf(`# Identity

Workspace: %s
`, name),
		"USER.md": `# User

The user owns the product direction. Ask only when a decision cannot be inferred from the repository or task.
`,
		"MEMORY.md": `# Memory

Persistent notes for this workspace can go here.
`,
	}

	if err := os.WriteFile(filepath.Join(dir, "workspace.yaml"), []byte(workspaceYAML), 0644); err != nil {
		return fmt.Errorf("write workspace.yaml: %w", err)
	}
	for fileName, content := range workspaceFiles {
		if err := os.WriteFile(filepath.Join(dir, fileName), []byte(content), 0644); err != nil {
			return fmt.Errorf("write %s: %w", fileName, err)
		}
	}

	fmt.Printf("\nCreated %s/\n", dir)
	fmt.Printf("  workspace.yaml\n")
	fmt.Printf("  elasticclaw-config.yaml\n")
	fmt.Printf("  AGENTS.md\n")
	fmt.Printf("  TOOLS.md\n")
	fmt.Printf("  SOUL.md\n")
	fmt.Printf("  IDENTITY.md\n")
	fmt.Printf("  USER.md\n")
	fmt.Printf("  MEMORY.md\n")
	fmt.Printf("Next steps:\n")
	fmt.Printf("  1. Edit %s/workspace.yaml and workspace files\n", dir)
	fmt.Printf("  2. Push to hub: elasticclaw workspace push %s\n", name)
	return nil
}

func workspacePushCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "push [name]",
		Short: "Push workspace definitions to the hub",
		RunE: func(cmd *cobra.Command, args []string) error {
			name := ""
			if len(args) > 0 {
				name = args[0]
			}
			return runWorkspacePush(name)
		},
	}
}

func workspaceRmCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "rm <name>",
		Short: "Remove a workspace from the hub",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return runWorkspaceRm(args[0])
		},
	}
}

func runWorkspaceRm(name string) error {
	hubURL, clawToken, err := resolveHubConn()
	if err != nil {
		return err
	}
	req, _ := http.NewRequest(http.MethodDelete, hubURL+"/api/workspaces?name="+url.QueryEscape(name), nil)
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
	fmt.Printf("Removed workspace %q from hub.\n", name)
	return nil
}

func runWorkspacePush(filterName string) error {
	hubURL, clawToken, err := resolveHubConn()
	if err != nil {
		return err
	}

	pattern := filepath.Join(".elasticclaw", "workspaces", "*", "workspace.yaml")
	matches, err := filepath.Glob(pattern)
	if err != nil || len(matches) == 0 {
		return fmt.Errorf("no workspaces found under .elasticclaw/workspaces/")
	}

	var workspaces []*types.WorkspaceConfig
	for _, match := range matches {
		workspace, err := readWorkspaceDir(filepath.Dir(match))
		if err != nil {
			return err
		}
		if filterName != "" && !strings.EqualFold(workspace.Name, filterName) {
			continue
		}
		if err := workspace.Validate(); err != nil {
			return fmt.Errorf("validation failed for %s: %w", match, err)
		}
		workspaces = append(workspaces, workspace)
	}
	if len(workspaces) == 0 {
		return fmt.Errorf("no workspaces matched")
	}

	body, _ := json.Marshal(map[string]interface{}{"workspaces": workspaces})
	req, _ := http.NewRequest(http.MethodPost, hubURL+"/api/workspaces", strings.NewReader(string(body)))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+clawToken)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return fmt.Errorf("push workspaces failed: %w", err)
	}
	defer resp.Body.Close()
	respBody, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("hub returned %d: %s", resp.StatusCode, strings.TrimSpace(string(respBody)))
	}

	var result struct {
		Pushed     int `json:"pushed"`
		Workspaces []struct {
			Name string `json:"name"`
		} `json:"workspaces"`
	}
	_ = json.Unmarshal(respBody, &result)

	fmt.Printf("Pushed %d workspace(s) to hub:\n", result.Pushed)
	for _, workspace := range result.Workspaces {
		fmt.Printf("  ✓ %s\n", workspace.Name)
	}
	return nil
}

func readWorkspaceDir(dir string) (*types.WorkspaceConfig, error) {
	data, err := os.ReadFile(filepath.Join(dir, "workspace.yaml"))
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", filepath.Join(dir, "workspace.yaml"), err)
	}
	var workspace types.WorkspaceConfig
	if err := yaml.Unmarshal(data, &workspace); err != nil {
		return nil, fmt.Errorf("parse %s: %w", filepath.Join(dir, "workspace.yaml"), err)
	}
	if workspace.Name == "" {
		workspace.Name = filepath.Base(dir)
	}
	if files, err := config.ReadTemplateFiles(dir); err == nil {
		workspace.Files = files
	}

	workflowDir := filepath.Join(dir, "workflows")
	entries, err := os.ReadDir(workflowDir)
	if err != nil {
		if os.IsNotExist(err) {
			return &workspace, nil
		}
		return nil, fmt.Errorf("read %s: %w", workflowDir, err)
	}
	for _, entry := range entries {
		if entry.IsDir() || strings.HasPrefix(entry.Name(), ".") || !strings.HasSuffix(entry.Name(), ".yaml") {
			continue
		}
		path := filepath.Join(workflowDir, entry.Name())
		data, err := os.ReadFile(path)
		if err != nil {
			return nil, fmt.Errorf("read %s: %w", path, err)
		}
		var workflow types.WorkflowConfig
		if err := yaml.Unmarshal(data, &workflow); err != nil {
			return nil, fmt.Errorf("parse %s: %w", path, err)
		}
		workspace.Workflows = append(workspace.Workflows, &workflow)
	}
	return &workspace, nil
}

func workspaceListCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "list",
		Short: "List workspaces on the hub",
		RunE: func(cmd *cobra.Command, args []string) error {
			return runWorkspaceList()
		},
	}
}

func runWorkspaceList() error {
	workspaces, err := fetchWorkspaceViews()
	if err != nil {
		return err
	}
	if jsonOut {
		return json.NewEncoder(os.Stdout).Encode(workspaces)
	}
	if len(workspaces) == 0 {
		fmt.Println("No workspaces configured.")
		return nil
	}

	w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintln(w, "NAME\tWORKFLOWS\tREPOS\tSECRETS\tSOURCE")
	for _, workspace := range workspaces {
		fmt.Fprintf(w, "%s\t%d\t%d\t%d\t%s\n",
			workspace.Name,
			len(workspace.Workflows),
			len(workspace.Access.Repositories),
			len(workspace.Access.Secrets),
			workspace.Source,
		)
	}
	w.Flush()
	return nil
}

func workspaceShowCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "show <name>",
		Short: "Show a workspace's current hub configuration",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return runWorkspaceShow(args[0])
		},
	}
}

func runWorkspaceShow(name string) error {
	workspaces, err := fetchWorkspaceViews()
	if err != nil {
		return err
	}
	for _, workspace := range workspaces {
		if strings.EqualFold(workspace.Name, name) {
			if jsonOut {
				return json.NewEncoder(os.Stdout).Encode(workspace)
			}
			out, err := yaml.Marshal(workspace)
			if err != nil {
				return err
			}
			fmt.Print(string(out))
			return nil
		}
	}
	return fmt.Errorf("workspace %q not found", name)
}

func fetchWorkspaceViews() ([]workspaceCLIView, error) {
	hubURL, clawToken, err := resolveHubConn()
	if err != nil {
		return nil, err
	}

	req, _ := http.NewRequest(http.MethodGet, hubURL+"/api/workspaces", nil)
	req.Header.Set("Authorization", "Bearer "+clawToken)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("list workspaces failed: %w", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("hub returned %d: %s", resp.StatusCode, strings.TrimSpace(string(body)))
	}

	var workspaces []workspaceCLIView
	if err := json.Unmarshal(body, &workspaces); err != nil {
		return nil, fmt.Errorf("decode: %w", err)
	}
	return workspaces, nil
}

func init() {
	rootCmd.AddCommand(WorkspaceCmd())
}
