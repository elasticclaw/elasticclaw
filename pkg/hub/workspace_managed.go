package hub

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"

	"github.com/elasticclaw/elasticclaw/pkg/types"
	"gopkg.in/yaml.v3"
)

const workspaceManagedDirName = ".elasticclaw-managed"

type workspaceSecretsFile struct {
	Secrets map[string]string `yaml:"secrets"`
}

type workspaceGitHubAppsFile struct {
	GitHubApps map[string]workspaceGitHubApp `yaml:"github_apps"`
}

type workspaceGitHubApp struct {
	AppID         int64  `yaml:"app_id" json:"appId"`
	URL           string `yaml:"url,omitempty" json:"url,omitempty"`
	Installation  string `yaml:"installation,omitempty" json:"installation,omitempty"`
	PrivateKeyPEM string `yaml:"private_key_pem" json:"privateKeyPem,omitempty"`
}

type workspaceGitHubAppView struct {
	Name          string `json:"name"`
	AppID         int64  `json:"appId"`
	URL           string `json:"url,omitempty"`
	Installation  string `json:"installation,omitempty"`
	PrivateKeySet bool   `json:"private_key_set"`
}

func workspaceManagedDir(workspace string) string {
	return filepath.Join(workspacesDir(), workspace, workspaceManagedDirName)
}

func workspaceSecretsPath(workspace string) string {
	return filepath.Join(workspaceManagedDir(workspace), "secrets.yaml")
}

func workspaceGitHubAppsPath(workspace string) string {
	return filepath.Join(workspaceManagedDir(workspace), "github_apps.yaml")
}

func loadWorkspaceSecrets(workspace string) (map[string]string, error) {
	var data workspaceSecretsFile
	if err := readManagedYAML(workspaceSecretsPath(workspace), &data); err != nil {
		return nil, err
	}
	if data.Secrets == nil {
		data.Secrets = map[string]string{}
	}
	return data.Secrets, nil
}

func saveWorkspaceSecret(workspace, name, value string) error {
	if err := ensureWorkspaceExists(workspace); err != nil {
		return err
	}
	secrets, err := loadWorkspaceSecrets(workspace)
	if err != nil {
		return err
	}
	secrets[name] = value
	return writeManagedYAML(workspaceSecretsPath(workspace), workspaceSecretsFile{Secrets: secrets})
}

func deleteWorkspaceSecret(workspace, name string) error {
	if err := ensureWorkspaceExists(workspace); err != nil {
		return err
	}
	secrets, err := loadWorkspaceSecrets(workspace)
	if err != nil {
		return err
	}
	delete(secrets, name)
	return writeManagedYAML(workspaceSecretsPath(workspace), workspaceSecretsFile{Secrets: secrets})
}

func workspaceSecretNames(workspace string) ([]string, error) {
	secrets, err := loadWorkspaceSecrets(workspace)
	if err != nil {
		return nil, err
	}
	names := make([]string, 0, len(secrets))
	for name := range secrets {
		names = append(names, name)
	}
	sort.Strings(names)
	return names, nil
}

func loadWorkspaceGitHubApps(workspace string) (map[string]workspaceGitHubApp, error) {
	var data workspaceGitHubAppsFile
	if err := readManagedYAML(workspaceGitHubAppsPath(workspace), &data); err != nil {
		return nil, err
	}
	if data.GitHubApps == nil {
		data.GitHubApps = map[string]workspaceGitHubApp{}
	}
	return data.GitHubApps, nil
}

func saveWorkspaceGitHubApp(workspace, name string, app workspaceGitHubApp) error {
	if err := ensureWorkspaceExists(workspace); err != nil {
		return err
	}
	apps, err := loadWorkspaceGitHubApps(workspace)
	if err != nil {
		return err
	}
	apps[name] = app
	return writeManagedYAML(workspaceGitHubAppsPath(workspace), workspaceGitHubAppsFile{GitHubApps: apps})
}

func deleteWorkspaceGitHubApp(workspace, name string) error {
	if err := ensureWorkspaceExists(workspace); err != nil {
		return err
	}
	apps, err := loadWorkspaceGitHubApps(workspace)
	if err != nil {
		return err
	}
	delete(apps, name)
	return writeManagedYAML(workspaceGitHubAppsPath(workspace), workspaceGitHubAppsFile{GitHubApps: apps})
}

func workspaceGitHubAppViews(workspace string) ([]workspaceGitHubAppView, error) {
	apps, err := loadWorkspaceGitHubApps(workspace)
	if err != nil {
		return nil, err
	}
	views := make([]workspaceGitHubAppView, 0, len(apps))
	for name, app := range apps {
		views = append(views, workspaceGitHubAppView{
			Name:          name,
			AppID:         app.AppID,
			URL:           app.URL,
			Installation:  app.Installation,
			PrivateKeySet: app.PrivateKeyPEM != "",
		})
	}
	sort.Slice(views, func(i, j int) bool { return views[i].Name < views[j].Name })
	return views, nil
}

func loadWorkspaceGitHubAppConfigs(workspace string) ([]*types.GitHubAppConfig, error) {
	apps, err := loadWorkspaceGitHubApps(workspace)
	if err != nil {
		return nil, err
	}
	names := make([]string, 0, len(apps))
	for name := range apps {
		names = append(names, name)
	}
	sort.Strings(names)
	configs := make([]*types.GitHubAppConfig, 0, len(names))
	for _, name := range names {
		app := apps[name]
		configs = append(configs, &types.GitHubAppConfig{
			AppID:         app.AppID,
			URL:           app.URL,
			PrivateKeyPEM: app.PrivateKeyPEM,
		})
	}
	return configs, nil
}

func ensureWorkspaceExists(workspace string) error {
	if err := validateName(workspace); err != nil {
		return err
	}
	if _, err := os.Stat(filepath.Join(workspacesDir(), workspace, "elasticclaw-config.yaml")); err != nil {
		return fmt.Errorf("workspace %q not found", workspace)
	}
	return nil
}

func readManagedYAML(path string, out interface{}) error {
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	return yaml.Unmarshal(data, out)
}

func writeManagedYAML(path string, value interface{}) error {
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		return err
	}
	data, err := yaml.Marshal(value)
	if err != nil {
		return err
	}
	return os.WriteFile(path, data, 0600)
}
