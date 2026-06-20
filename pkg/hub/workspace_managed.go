package hub

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

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

type workspaceIssueTrackersFile struct {
	IssueTrackers workspaceIssueTrackers `yaml:"issue_trackers"`
}

type workspaceIssueTrackers struct {
	Linear       map[string]workspaceIssueTracker `yaml:"linear,omitempty"`
	Shortcut     map[string]workspaceIssueTracker `yaml:"shortcut,omitempty"`
	GitHubIssues map[string]workspaceIssueTracker `yaml:"github_issues,omitempty"`
	Jira         map[string]workspaceIssueTracker `yaml:"jira,omitempty"`
}

type workspaceIssueTracker struct {
	BaseURL       string `yaml:"base_url,omitempty" json:"baseUrl,omitempty"`
	Username      string `yaml:"username,omitempty" json:"username,omitempty"`
	Token         string `yaml:"token,omitempty" json:"token,omitempty"`
	WebhookSecret string `yaml:"webhook_secret,omitempty" json:"webhookSecret,omitempty"`
}

type workspaceIssueTrackerView struct {
	Type             string `json:"type"`
	Workspace        string `json:"workspace"`
	TokenSet         bool   `json:"tokenSet"`
	WebhookSecretSet bool   `json:"webhookSecretSet"`
	BaseURL          string `json:"baseUrl,omitempty"`
	Username         string `json:"username,omitempty"`
}

type workspaceGitHubApp struct {
	AppID         int64  `yaml:"app_id" json:"appId"`
	URL           string `yaml:"url,omitempty" json:"url,omitempty"`
	Installation  string `yaml:"installation,omitempty" json:"installation,omitempty"`
	PrivateKeyPEM string `yaml:"private_key_pem" json:"privateKeyPem,omitempty"`
}

type workspaceGitHubAppView struct {
	Name          string   `json:"name"`
	AppID         int64    `json:"appId"`
	URL           string   `json:"url,omitempty"`
	Installation  string   `json:"installation,omitempty"`
	Installations []string `json:"installations,omitempty"`
	PrivateKeySet bool     `json:"private_key_set"`
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

func workspaceIssueTrackersPath(workspace string) string {
	return filepath.Join(workspaceManagedDir(workspace), "issue_trackers.yaml")
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

func loadWorkspaceIssueTrackers(workspace string) (workspaceIssueTrackers, error) {
	var data workspaceIssueTrackersFile
	if err := readManagedYAML(workspaceIssueTrackersPath(workspace), &data); err != nil {
		return workspaceIssueTrackers{}, err
	}
	ensureIssueTrackerMaps(&data.IssueTrackers)
	return data.IssueTrackers, nil
}

func saveWorkspaceIssueTracker(workspace, trackerType, name string, tracker workspaceIssueTracker) error {
	if err := ensureWorkspaceExists(workspace); err != nil {
		return err
	}
	trackers, err := loadWorkspaceIssueTrackers(workspace)
	if err != nil {
		return err
	}
	ensureIssueTrackerMaps(&trackers)
	switch trackerType {
	case "linear":
		trackers.Linear[name] = tracker
	case "shortcut":
		trackers.Shortcut[name] = tracker
	case "github-issues":
		trackers.GitHubIssues[name] = tracker
	case "jira":
		trackers.Jira[name] = tracker
	default:
		return fmt.Errorf("invalid issue tracker type %q", trackerType)
	}
	return writeManagedYAML(workspaceIssueTrackersPath(workspace), workspaceIssueTrackersFile{IssueTrackers: trackers})
}

func deleteWorkspaceIssueTracker(workspace, trackerType, name string) error {
	if err := ensureWorkspaceExists(workspace); err != nil {
		return err
	}
	trackers, err := loadWorkspaceIssueTrackers(workspace)
	if err != nil {
		return err
	}
	switch trackerType {
	case "linear":
		delete(trackers.Linear, name)
	case "shortcut":
		delete(trackers.Shortcut, name)
	case "github-issues":
		delete(trackers.GitHubIssues, name)
	case "jira":
		delete(trackers.Jira, name)
	default:
		return fmt.Errorf("invalid issue tracker type %q", trackerType)
	}
	return writeManagedYAML(workspaceIssueTrackersPath(workspace), workspaceIssueTrackersFile{IssueTrackers: trackers})
}

func workspaceIssueTrackerViews(workspace string) ([]workspaceIssueTrackerView, error) {
	trackers, err := loadWorkspaceIssueTrackers(workspace)
	if err != nil {
		return nil, err
	}
	views := make([]workspaceIssueTrackerView, 0, len(trackers.Linear)+len(trackers.Shortcut)+len(trackers.GitHubIssues)+len(trackers.Jira))
	appendViews := func(trackerType string, values map[string]workspaceIssueTracker) {
		names := make([]string, 0, len(values))
		for name := range values {
			names = append(names, name)
		}
		sort.Strings(names)
		for _, name := range names {
			tracker := values[name]
			views = append(views, workspaceIssueTrackerView{
				Type:             trackerType,
				Workspace:        name,
				TokenSet:         tracker.Token != "",
				WebhookSecretSet: tracker.WebhookSecret != "",
				BaseURL:          tracker.BaseURL,
				Username:         tracker.Username,
			})
		}
	}
	appendViews("linear", trackers.Linear)
	appendViews("shortcut", trackers.Shortcut)
	appendViews("github-issues", trackers.GitHubIssues)
	appendViews("jira", trackers.Jira)
	return views, nil
}

func findWorkspaceIssueTracker(workspace, trackerType, name string) (workspaceIssueTracker, bool) {
	trackers, err := loadWorkspaceIssueTrackers(workspace)
	if err != nil {
		return workspaceIssueTracker{}, false
	}
	find := func(values map[string]workspaceIssueTracker) (workspaceIssueTracker, bool) {
		if name == "" {
			if len(values) != 1 {
				return workspaceIssueTracker{}, false
			}
			for _, tracker := range values {
				return tracker, true
			}
		}
		for trackerName, tracker := range values {
			if strings.EqualFold(trackerName, name) {
				return tracker, true
			}
		}
		return workspaceIssueTracker{}, false
	}
	switch trackerType {
	case "linear":
		return find(trackers.Linear)
	case "shortcut":
		return find(trackers.Shortcut)
	case "github-issues":
		return find(trackers.GitHubIssues)
	case "jira":
		return find(trackers.Jira)
	default:
		return workspaceIssueTracker{}, false
	}
}

func workspaceIssueTrackerWebhookSecrets(workspace, trackerType string) []string {
	trackers, err := loadWorkspaceIssueTrackers(workspace)
	if err != nil {
		return nil
	}
	var values map[string]workspaceIssueTracker
	switch trackerType {
	case "linear":
		values = trackers.Linear
	case "shortcut":
		values = trackers.Shortcut
	case "github-issues":
		values = trackers.GitHubIssues
	case "jira":
		values = trackers.Jira
	default:
		return nil
	}
	secrets := make([]string, 0, len(values))
	for _, tracker := range values {
		if tracker.WebhookSecret != "" {
			secrets = append(secrets, tracker.WebhookSecret)
		}
	}
	return secrets
}

func ensureIssueTrackerMaps(trackers *workspaceIssueTrackers) {
	if trackers.Linear == nil {
		trackers.Linear = map[string]workspaceIssueTracker{}
	}
	if trackers.Shortcut == nil {
		trackers.Shortcut = map[string]workspaceIssueTracker{}
	}
	if trackers.GitHubIssues == nil {
		trackers.GitHubIssues = map[string]workspaceIssueTracker{}
	}
	if trackers.Jira == nil {
		trackers.Jira = map[string]workspaceIssueTracker{}
	}
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
