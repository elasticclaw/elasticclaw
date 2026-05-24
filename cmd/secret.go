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

	"github.com/spf13/cobra"
)

func SecretCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "secret",
		Short: "Manage workspace secrets",
	}
	cmd.AddCommand(secretListCmd())
	cmd.AddCommand(secretCreateCmd())
	cmd.AddCommand(secretRmCmd())
	return cmd
}

func secretListCmd() *cobra.Command {
	var workspace string
	cmd := &cobra.Command{
		Use:   "list",
		Short: "List secret names in a workspace",
		RunE: func(cmd *cobra.Command, args []string) error {
			return runSecretList(workspace)
		},
	}
	cmd.Flags().StringVar(&workspace, "workspace", "", "workspace name [required]")
	return cmd
}

func secretCreateCmd() *cobra.Command {
	var workspace string
	var value string
	cmd := &cobra.Command{
		Use:   "create <name>",
		Short: "Create or update a workspace secret",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return runSecretCreate(workspace, args[0], value)
		},
	}
	cmd.Flags().StringVar(&workspace, "workspace", "", "workspace name [required]")
	cmd.Flags().StringVar(&value, "value", "", "secret value; if omitted, a piped stdin value is used")
	return cmd
}

func secretRmCmd() *cobra.Command {
	var workspace string
	cmd := &cobra.Command{
		Use:     "rm <name>",
		Aliases: []string{"delete", "remove"},
		Short:   "Delete a workspace secret",
		Args:    cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return runSecretRm(workspace, args[0])
		},
	}
	cmd.Flags().StringVar(&workspace, "workspace", "", "workspace name [required]")
	return cmd
}

func runSecretList(workspace string) error {
	if strings.TrimSpace(workspace) == "" {
		return fmt.Errorf("--workspace is required")
	}
	hubURL, clawToken, err := resolveHubConn()
	if err != nil {
		return err
	}
	req, _ := http.NewRequest(http.MethodGet, hubURL+"/api/workspaces/"+url.PathEscape(workspace)+"/secrets", nil)
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
		Secrets []string `json:"secrets"`
	}
	_ = json.Unmarshal(body, &result)
	if len(result.Secrets) == 0 {
		fmt.Printf("No secrets configured in workspace %q.\n", workspace)
		return nil
	}
	for _, name := range result.Secrets {
		fmt.Println(name)
	}
	return nil
}

func runSecretCreate(workspace, name, value string) error {
	if strings.TrimSpace(workspace) == "" {
		return fmt.Errorf("--workspace is required")
	}
	if strings.TrimSpace(name) == "" {
		return fmt.Errorf("secret name is required")
	}
	if value == "" {
		stdinInfo, err := os.Stdin.Stat()
		if err != nil {
			return err
		}
		if stdinInfo.Mode()&os.ModeCharDevice != 0 {
			return fmt.Errorf("--value is required unless a secret value is piped on stdin")
		}
		data, err := io.ReadAll(os.Stdin)
		if err != nil {
			return err
		}
		value = strings.TrimRight(string(data), "\r\n")
	}
	hubURL, clawToken, err := resolveHubConn()
	if err != nil {
		return err
	}
	body, _ := json.Marshal(map[string]string{"name": name, "value": value})
	req, _ := http.NewRequest(http.MethodPut, hubURL+"/api/workspaces/"+url.PathEscape(workspace)+"/secrets", bytes.NewReader(body))
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
	fmt.Printf("Stored secret %q in workspace %q.\n", name, workspace)
	return nil
}

func runSecretRm(workspace, name string) error {
	if strings.TrimSpace(workspace) == "" {
		return fmt.Errorf("--workspace is required")
	}
	hubURL, clawToken, err := resolveHubConn()
	if err != nil {
		return err
	}
	path := hubURL + "/api/workspaces/" + url.PathEscape(workspace) + "/secrets?name=" + url.QueryEscape(name)
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
	fmt.Printf("Deleted secret %q from workspace %q.\n", name, workspace)
	return nil
}

func init() {
	rootCmd.AddCommand(SecretCmd())
}
