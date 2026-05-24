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
)

func GitHubAppCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "github-app",
		Short: "Manage workspace GitHub Apps",
	}
	cmd.AddCommand(githubAppListCmd())
	cmd.AddCommand(githubAppCreateCmd())
	cmd.AddCommand(githubAppRmCmd())
	return cmd
}

type githubAppCLIView struct {
	Name          string `json:"name"`
	AppID         int64  `json:"appId"`
	URL           string `json:"url,omitempty"`
	Installation  string `json:"installation,omitempty"`
	PrivateKeySet bool   `json:"private_key_set"`
}

func githubAppListCmd() *cobra.Command {
	var workspace string
	cmd := &cobra.Command{
		Use:   "list",
		Short: "List GitHub Apps in a workspace",
		RunE: func(cmd *cobra.Command, args []string) error {
			return runGitHubAppList(workspace)
		},
	}
	cmd.Flags().StringVar(&workspace, "workspace", "", "workspace name [required]")
	return cmd
}

func githubAppCreateCmd() *cobra.Command {
	var workspace string
	var appID int64
	var appURL string
	var installation string
	var privateKeyFile string
	cmd := &cobra.Command{
		Use:   "create <name>",
		Short: "Create or update a workspace GitHub App",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return runGitHubAppCreate(workspace, args[0], appID, appURL, installation, privateKeyFile)
		},
	}
	cmd.Flags().StringVar(&workspace, "workspace", "", "workspace name [required]")
	cmd.Flags().Int64Var(&appID, "app-id", 0, "GitHub App ID [required]")
	cmd.Flags().StringVar(&appURL, "url", "", "GitHub App URL")
	cmd.Flags().StringVar(&installation, "installation", "", "installation owner or slug for display")
	cmd.Flags().StringVar(&privateKeyFile, "private-key-file", "", "path to the GitHub App private key PEM")
	return cmd
}

func githubAppRmCmd() *cobra.Command {
	var workspace string
	cmd := &cobra.Command{
		Use:     "rm <name>",
		Aliases: []string{"delete", "remove"},
		Short:   "Delete a workspace GitHub App",
		Args:    cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return runGitHubAppRm(workspace, args[0])
		},
	}
	cmd.Flags().StringVar(&workspace, "workspace", "", "workspace name [required]")
	return cmd
}

func runGitHubAppList(workspace string) error {
	if strings.TrimSpace(workspace) == "" {
		return fmt.Errorf("--workspace is required")
	}
	hubURL, clawToken, err := resolveHubConn()
	if err != nil {
		return err
	}
	req, _ := http.NewRequest(http.MethodGet, hubURL+"/api/workspaces/"+url.PathEscape(workspace)+"/github-apps", nil)
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
	if jsonOut {
		fmt.Print(string(body))
		if !strings.HasSuffix(string(body), "\n") {
			fmt.Println()
		}
		return nil
	}
	var result struct {
		GitHubApps []githubAppCLIView `json:"githubApps"`
	}
	_ = json.Unmarshal(body, &result)
	if len(result.GitHubApps) == 0 {
		fmt.Printf("No GitHub Apps configured in workspace %q.\n", workspace)
		return nil
	}
	w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintln(w, "NAME\tAPP ID\tINSTALLATION\tKEY")
	for _, app := range result.GitHubApps {
		key := "no"
		if app.PrivateKeySet {
			key = "yes"
		}
		fmt.Fprintf(w, "%s\t%d\t%s\t%s\n", app.Name, app.AppID, app.Installation, key)
	}
	w.Flush()
	return nil
}

func runGitHubAppCreate(workspace, name string, appID int64, appURL, installation, privateKeyFile string) error {
	if strings.TrimSpace(workspace) == "" {
		return fmt.Errorf("--workspace is required")
	}
	if strings.TrimSpace(name) == "" {
		return fmt.Errorf("github app name is required")
	}
	if appID == 0 {
		return fmt.Errorf("--app-id is required")
	}
	privateKeyPEM := ""
	if privateKeyFile != "" {
		data, err := os.ReadFile(privateKeyFile)
		if err != nil {
			return fmt.Errorf("read private key: %w", err)
		}
		privateKeyPEM = string(data)
	}
	hubURL, clawToken, err := resolveHubConn()
	if err != nil {
		return err
	}
	body, _ := json.Marshal(map[string]interface{}{
		"name":          name,
		"appId":         appID,
		"url":           appURL,
		"installation":  installation,
		"privateKeyPem": privateKeyPEM,
	})
	req, _ := http.NewRequest(http.MethodPut, hubURL+"/api/workspaces/"+url.PathEscape(workspace)+"/github-apps", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+clawToken)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	respBody, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("hub returned %d: %s", resp.StatusCode, strings.TrimSpace(string(respBody)))
	}
	if jsonOut {
		fmt.Print(string(respBody))
		if !strings.HasSuffix(string(respBody), "\n") {
			fmt.Println()
		}
		return nil
	}
	fmt.Printf("Stored GitHub App %q in workspace %q.\n", name, workspace)
	return nil
}

func runGitHubAppRm(workspace, name string) error {
	if strings.TrimSpace(workspace) == "" {
		return fmt.Errorf("--workspace is required")
	}
	hubURL, clawToken, err := resolveHubConn()
	if err != nil {
		return err
	}
	path := hubURL + "/api/workspaces/" + url.PathEscape(workspace) + "/github-apps?name=" + url.QueryEscape(name)
	req, _ := http.NewRequest(http.MethodDelete, path, nil)
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
	if jsonOut {
		fmt.Print(string(body))
		if !strings.HasSuffix(string(body), "\n") {
			fmt.Println()
		}
		return nil
	}
	fmt.Printf("Deleted GitHub App %q from workspace %q.\n", name, workspace)
	return nil
}

func init() {
	rootCmd.AddCommand(GitHubAppCmd())
}
