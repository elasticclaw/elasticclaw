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
	cmd.AddCommand(workspaceConvertCmd())
	return cmd
}

type workspaceCLIView struct {
	Name      string              `json:"name"`
	Source    string              `json:"source"`
	Access    workspaceAccessView `json:"access"`
	Workflows []workflowCLIView   `json:"workflows"`
}

type workspaceAccessView struct {
	Repositories   []types.GitHubRepoAccess `json:"repositories"`
	Env            []string                 `json:"env"`
	Secrets        []string                 `json:"secrets"`
	WebhookSecrets []string                 `json:"webhookSecrets"`
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

	workspaceConfigYAML := fmt.Sprintf(`schema_version: v1
name: %s

repositories: []
env: {}

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

# Optional repository access:
# repositories:
#   - repo: owner/repo
#     permissions: write
#   # Patterns without an owner match repository names. Use "*" for all repos.
#   - repo: "*-infra-*"
#     permissions: read
#   # Patterns with an owner match the full owner/repository name.
#   - repo: "owner/*"
#     permissions: read
#
# Optional workspace environment:
# env:
#   NODE_ENV: development
#   OPENAI_API_KEY:
#     secret: openai_api_key

# Optional environment secrets:
# secret_refs:
#   GITHUB_TOKEN: github_app
`, name)
	workspaceFiles := map[string]string{
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

	if err := os.WriteFile(filepath.Join(dir, "elasticclaw-config.yaml"), []byte(workspaceConfigYAML), 0644); err != nil {
		return fmt.Errorf("write elasticclaw-config.yaml: %w", err)
	}
	for fileName, content := range workspaceFiles {
		if err := os.WriteFile(filepath.Join(dir, fileName), []byte(content), 0644); err != nil {
			return fmt.Errorf("write %s: %w", fileName, err)
		}
	}

	fmt.Printf("\nCreated %s/\n", dir)
	fmt.Printf("  elasticclaw-config.yaml\n")
	fmt.Printf("  AGENTS.md\n")
	fmt.Printf("  TOOLS.md\n")
	fmt.Printf("  SOUL.md\n")
	fmt.Printf("  IDENTITY.md\n")
	fmt.Printf("  USER.md\n")
	fmt.Printf("  MEMORY.md\n")
	fmt.Printf("Next steps:\n")
	fmt.Printf("  1. Edit %s/elasticclaw-config.yaml and workspace files\n", dir)
	fmt.Printf("  2. Push to hub: elasticclaw workspace push %s\n", name)
	return nil
}

func workspacePushCmd() *cobra.Command {
	var path string
	cmd := &cobra.Command{
		Use:   "push [name]",
		Short: "Push workspace definitions to the hub",
		Long: `Push workspace definitions to the hub.

By default, searches .elasticclaw/workspaces/ and pushes all valid workspaces.
Pass a name to push only the matching workspace.
Use --path to push a workspace from a specific directory instead of the default location.`,
		RunE: func(cmd *cobra.Command, args []string) error {
			name := ""
			if len(args) > 0 {
				name = args[0]
			}
			return runWorkspacePush(name, path)
		},
	}
	cmd.Flags().StringVar(&path, "path", "", "path to a specific workspace directory to push (instead of .elasticclaw/workspaces/)")
	return cmd
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

func runWorkspacePush(filterName string, path string) error {
	workspaces, err := collectWorkspacesForPush(filterName, path)
	if err != nil {
		return err
	}
	if len(workspaces) == 0 {
		return fmt.Errorf("no workspaces matched")
	}
	return pushWorkspacesToHub(workspaces)
}

// collectWorkspacesForPush resolves the workspace directories to push.
// If path is set, it pushes only that directory. Otherwise it scans
// .elasticclaw/workspaces/*. The filterName optionally restricts results
// to a single workspace name.
func collectWorkspacesForPush(filterName string, path string) ([]*types.WorkspaceConfig, error) {
	var workspaces []*types.WorkspaceConfig

	if path != "" {
		info, err := os.Stat(path)
		if err != nil {
			return nil, fmt.Errorf("stat workspace path %q: %w", path, err)
		}
		if !info.IsDir() {
			return nil, fmt.Errorf("workspace path %q is not a directory", path)
		}
		workspace, err := readWorkspaceDir(path)
		if err != nil {
			return nil, err
		}
		if filterName != "" && !strings.EqualFold(workspace.Name, filterName) {
			return []*types.WorkspaceConfig{}, nil
		}
		if err := workspace.Validate(); err != nil {
			return nil, fmt.Errorf("validation failed for %s: %w", path, err)
		}
		return []*types.WorkspaceConfig{workspace}, nil
	}

	pattern := filepath.Join(".elasticclaw", "workspaces", "*")
	matches, err := filepath.Glob(pattern)
	if err != nil || len(matches) == 0 {
		return nil, fmt.Errorf("no workspaces found under .elasticclaw/workspaces/")
	}

	for _, match := range matches {
		if info, err := os.Stat(match); err != nil || !info.IsDir() {
			continue
		}
		workspace, err := readWorkspaceDir(match)
		if err != nil {
			return nil, err
		}
		if filterName != "" && !strings.EqualFold(workspace.Name, filterName) {
			continue
		}
		if err := workspace.Validate(); err != nil {
			return nil, fmt.Errorf("validation failed for %s: %w", match, err)
		}
		workspaces = append(workspaces, workspace)
	}
	return workspaces, nil
}

func pushWorkspacesToHub(workspaces []*types.WorkspaceConfig) error {
	hubURL, clawToken, err := resolveHubConn()
	if err != nil {
		return err
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
	configPath := filepath.Join(dir, "elasticclaw-config.yaml")
	data, err := os.ReadFile(configPath)
	if err != nil {
		legacyPath := filepath.Join(dir, "workspace.yaml")
		data, err = os.ReadFile(legacyPath)
		if err != nil {
			return nil, fmt.Errorf("read %s: %w", configPath, err)
		}
		configPath = legacyPath
	}
	var workspace types.WorkspaceConfig
	if err := yaml.Unmarshal(data, &workspace); err != nil {
		return nil, fmt.Errorf("parse %s: %w", configPath, err)
	}
	if workspace.Name == "" {
		workspace.Name = filepath.Base(dir)
	}
	if files, err := config.ReadTemplateFiles(dir); err == nil {
		workspace.Files = files
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
