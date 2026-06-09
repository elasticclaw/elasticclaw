package hub

import (
	"encoding/json"
	"slices"
	"strings"
	"testing"

	"github.com/elasticclaw/elasticclaw/pkg/types"
)

func TestWorkflowSetupSnapshotSanitizesHubConfig(t *testing.T) {
	cfg := &types.HubConfig{
		ClawToken:    "claw-secret",
		DefaultModel: "anthropic/claude-sonnet-4-6",
		Providers: map[string]types.ProviderConfig{
			"daytona": {
				APIURL:          "https://daytona.example",
				APIKey:          "daytona-secret",
				DefaultSnapshot: "snap-1",
			},
			"docker": {
				Type:  "docker",
				Image: "elasticclaw/dev",
			},
			"replicated": {
				Type:  "replicated",
				Token: "replicated-secret",
			},
		},
		LLMKeys: types.LLMKeysList{
			{Name: "anthropic-prod", Provider: "anthropic", APIKey: "sk-secret", Default: true, DefaultModel: "claude-sonnet-4-6"},
			{Name: "ollama-local", Provider: "ollama"},
		},
		Secrets: map[string]string{
			"db_password":    "db-secret",
			"github_webhook": "webhook-secret",
		},
		Integrations: &types.IntegrationsConfig{
			Linear:       []*types.LinearIntegrationConfig{{Workspace: "linear-main", Token: "linear-secret", WebhookSecret: "linear-hook-secret"}},
			Shortcut:     []*types.ShortcutIntegrationConfig{{Workspace: "shortcut-main", Token: "shortcut-secret"}},
			GitHubIssues: []*types.GitHubIssuesIntegrationConfig{{Workspace: "issues-main", Token: "issues-secret", WebhookSecret: "issues-hook-secret"}},
		},
		GitHubApps: []*types.GitHubAppConfig{{
			AppID:         12345,
			URL:           "https://github.com/apps/eng",
			PrivateKeyPEM: "-----BEGIN PRIVATE KEY-----\nprivate-key-secret\n-----END PRIVATE KEY-----",
		}},
		ConcurrencyGroups: []*types.ConcurrencyGroup{
			{Name: "global", Limit: 2},
			{Name: "deploys", Limit: 1},
		},
	}
	s, _ := NewTestServerWithConfig(t, cfg, "", "", "")

	snapshot, err := s.WorkflowSetupEnvironment().Snapshot()
	if err != nil {
		t.Fatalf("snapshot: %v", err)
	}

	if !snapshot.ClawTokenSet {
		t.Fatalf("ClawTokenSet = false, want true")
	}
	if snapshot.DefaultModel != "anthropic/claude-sonnet-4-6" {
		t.Fatalf("DefaultModel = %q", snapshot.DefaultModel)
	}
	if snapshot.DefaultProvider != "daytona" {
		t.Fatalf("DefaultProvider = %q, want daytona", snapshot.DefaultProvider)
	}
	if got, want := snapshot.HubSecretNames, []string{"db_password", "github_webhook"}; !slices.Equal(got, want) {
		t.Fatalf("HubSecretNames = %#v, want %#v", got, want)
	}

	providers := map[string]types.ProviderType{}
	provisionable := map[string]bool{}
	credentialsSet := map[string]bool{}
	apiKeySet := map[string]bool{}
	tokenSet := map[string]bool{}
	for _, provider := range snapshot.Providers {
		providers[provider.Name] = types.ProviderType(provider.Type)
		provisionable[provider.Name] = provider.Provisionable
		credentialsSet[provider.Name] = provider.CredentialsSet
		apiKeySet[provider.Name] = provider.APIKeySet
		tokenSet[provider.Name] = provider.TokenSet
	}
	if providers["daytona"] != "daytona" || !provisionable["daytona"] || !credentialsSet["daytona"] || !apiKeySet["daytona"] {
		t.Fatalf("daytona provider ref not populated correctly: %#v", snapshot.Providers)
	}
	if providers["docker"] != "docker" || !provisionable["docker"] || credentialsSet["docker"] {
		t.Fatalf("docker provider ref not populated correctly: %#v", snapshot.Providers)
	}
	if providers["replicated"] != "replicated" || !provisionable["replicated"] || !credentialsSet["replicated"] || !tokenSet["replicated"] {
		t.Fatalf("replicated provider ref not populated correctly: %#v", snapshot.Providers)
	}

	if len(snapshot.LLMKeys) != 2 {
		t.Fatalf("LLMKeys length = %d, want 2", len(snapshot.LLMKeys))
	}
	if snapshot.LLMKeys[0].Name != "anthropic-prod" || !snapshot.LLMKeys[0].KeySet || snapshot.LLMKeys[0].DefaultModel != "claude-sonnet-4-6" {
		t.Fatalf("anthropic LLM key metadata = %#v", snapshot.LLMKeys[0])
	}
	if snapshot.LLMKeys[1].Name != "ollama-local" || !snapshot.LLMKeys[1].KeySet {
		t.Fatalf("ollama LLM key metadata = %#v", snapshot.LLMKeys[1])
	}

	if got, want := len(snapshot.IssueTrackers), 3; got != want {
		t.Fatalf("IssueTrackers length = %d, want %d: %#v", got, want, snapshot.IssueTrackers)
	}
	if got, want := len(snapshot.GitHubApps), 1; got != want {
		t.Fatalf("GitHubApps length = %d, want %d", got, want)
	}
	if snapshot.GitHubApps[0].AppID != 12345 || !snapshot.GitHubApps[0].PrivateKeySet {
		t.Fatalf("GitHubApp metadata = %#v", snapshot.GitHubApps[0])
	}

	data, err := json.Marshal(snapshot)
	if err != nil {
		t.Fatalf("marshal snapshot: %v", err)
	}
	for _, secret := range []string{
		"claw-secret",
		"daytona-secret",
		"replicated-secret",
		"sk-secret",
		"db-secret",
		"webhook-secret",
		"linear-secret",
		"linear-hook-secret",
		"shortcut-secret",
		"issues-secret",
		"issues-hook-secret",
		"private-key-secret",
	} {
		if strings.Contains(string(data), secret) {
			t.Fatalf("snapshot JSON contains secret value %q: %s", secret, data)
		}
	}
}

func TestWorkflowSetupSnapshotLoadersUseExternalStorage(t *testing.T) {
	t.Setenv("ELASTICCLAW_HUB_CONFIG", t.TempDir()+"/hub.yaml")

	rawWorkflow := "# authored workflow\nschema_version: v1\nname: IssueTriage\ntrigger:\n  github_issues:\n    event: labeled\n    repositories:\n      - elasticclaw/elasticclaw\n    labels:\n      - ready\nstages:\n  - id: start\n    entry: true\n"
	SaveWorkspaceForTest(t,
		&types.WorkspaceConfig{
			SchemaVersion: "v1",
			Name:          "engineering",
			Secrets:       []string{"workspace_declared_secret"},
		},
		[]*types.WorkflowConfig{{
			Name:      "IssueTriage",
			RawConfig: rawWorkflow,
		}},
	)
	if err := saveWorkspaceSecret("engineering", "workspace_token", "workspace-secret-value"); err != nil {
		t.Fatalf("save workspace secret: %v", err)
	}
	if err := saveWorkspaceIssueTracker("engineering", "github-issues", "default", workspaceIssueTracker{
		Token:         "workspace-issues-token",
		WebhookSecret: "workspace-issues-hook",
	}); err != nil {
		t.Fatalf("save workspace issue tracker: %v", err)
	}
	if err := saveWorkspaceGitHubApp("engineering", "eng-app", workspaceGitHubApp{
		AppID:         9876,
		URL:           "https://github.com/apps/eng-app",
		Installation:  "elasticclaw",
		PrivateKeyPEM: "workspace-private-key",
	}); err != nil {
		t.Fatalf("save workspace GitHub App: %v", err)
	}

	legacyFactory := &types.FactoryConfig{
		Name:          "legacy-factory",
		Integration:   "linear",
		Workspace:     "linear-main",
		Template:      "engineering",
		WebhookSecret: "legacy-factory-secret",
	}
	externalFactory := &types.FactoryConfig{
		Name:          "external-factory",
		Integration:   "github",
		Template:      "engineering",
		WebhookSecret: "external-factory-secret",
		SecretRefs:    map[string]string{"TOKEN": "github_webhook"},
		PipelineYAML:  "stages:\n  - id: done\n",
	}
	refFactory := &types.FactoryConfig{
		Name:             "ref-factory",
		Integration:      "github",
		Template:         "engineering",
		WebhookSecretRef: "github_webhook",
	}
	if err := saveExternalFactory(externalFactory); err != nil {
		t.Fatalf("save external factory: %v", err)
	}
	if err := saveExternalFactory(refFactory); err != nil {
		t.Fatalf("save ref factory: %v", err)
	}

	s, _ := NewTestServerWithConfig(t, &types.HubConfig{
		Factories: []*types.FactoryConfig{legacyFactory},
	}, "", "", "")
	env := s.WorkflowSetupEnvironment()

	workspace, err := env.LoadWorkspace("engineering")
	if err != nil {
		t.Fatalf("LoadWorkspace: %v", err)
	}
	if workspace.Name != "engineering" || len(workspace.Workflows) != 1 {
		t.Fatalf("workspace = %#v", workspace)
	}

	gotRaw, err := env.LoadWorkflowRaw("engineering", "issuetriage")
	if err != nil {
		t.Fatalf("LoadWorkflowRaw: %v", err)
	}
	if gotRaw != rawWorkflow {
		t.Fatalf("LoadWorkflowRaw returned authored YAML mismatch\n got: %q\nwant: %q", gotRaw, rawWorkflow)
	}

	secretNames, err := env.WorkspaceSecretNames("engineering")
	if err != nil {
		t.Fatalf("WorkspaceSecretNames: %v", err)
	}
	if got, want := secretNames, []string{"workspace_token"}; !slices.Equal(got, want) {
		t.Fatalf("WorkspaceSecretNames = %#v, want %#v", got, want)
	}

	trackers, err := env.WorkspaceIssueTrackers("engineering")
	if err != nil {
		t.Fatalf("WorkspaceIssueTrackers: %v", err)
	}
	if len(trackers) != 1 || trackers[0].Type != "github-issues" || trackers[0].Workspace != "default" || !trackers[0].TokenSet || !trackers[0].WebhookSecretSet {
		t.Fatalf("WorkspaceIssueTrackers = %#v", trackers)
	}

	apps, err := env.WorkspaceGitHubApps("engineering")
	if err != nil {
		t.Fatalf("WorkspaceGitHubApps: %v", err)
	}
	if len(apps) != 1 || apps[0].Name != "eng-app" || apps[0].AppID != 9876 || !apps[0].PrivateKeySet {
		t.Fatalf("WorkspaceGitHubApps = %#v", apps)
	}

	factories, err := env.ListFactories()
	if err != nil {
		t.Fatalf("ListFactories: %v", err)
	}
	factoryNames := make([]string, 0, len(factories))
	for _, factory := range factories {
		factoryNames = append(factoryNames, factory.Name)
	}
	slices.Sort(factoryNames)
	if got, want := factoryNames, []string{"external-factory", "legacy-factory", "ref-factory"}; !slices.Equal(got, want) {
		t.Fatalf("factory names = %#v, want %#v", got, want)
	}
	factoryWebhookSetByName := map[string]bool{}
	factoryWebhookRefByName := map[string]string{}
	for _, factory := range factories {
		factoryWebhookSetByName[factory.Name] = factory.WebhookSecretSet
		factoryWebhookRefByName[factory.Name] = factory.WebhookSecretRef
	}
	if !factoryWebhookSetByName["legacy-factory"] {
		t.Fatalf("legacy factory ref WebhookSecretSet = false, want true")
	}
	if !factoryWebhookSetByName["external-factory"] {
		t.Fatalf("external factory ref WebhookSecretSet = false, want true")
	}
	if !factoryWebhookSetByName["ref-factory"] || factoryWebhookRefByName["ref-factory"] != "github_webhook" {
		t.Fatalf("ref factory webhook metadata: set=%v ref=%q", factoryWebhookSetByName["ref-factory"], factoryWebhookRefByName["ref-factory"])
	}
	factoryRefsJSON, err := json.Marshal(factories)
	if err != nil {
		t.Fatalf("marshal factory refs: %v", err)
	}
	for _, secret := range []string{"legacy-factory-secret", "external-factory-secret", "stages:\n  - id: done"} {
		if strings.Contains(string(factoryRefsJSON), secret) {
			t.Fatalf("factory refs contain sensitive/raw value %q: %s", secret, factoryRefsJSON)
		}
	}

	loadedExternal, err := env.LoadFactory("external-factory")
	if err != nil {
		t.Fatalf("LoadFactory external: %v", err)
	}
	if loadedExternal.WebhookSecret != "" || loadedExternal.PipelineYAML != externalFactory.PipelineYAML {
		t.Fatalf("LoadFactory external = %#v", loadedExternal)
	}
	loadedLegacy, err := env.LoadFactory("legacy-factory")
	if err != nil {
		t.Fatalf("LoadFactory legacy: %v", err)
	}
	if loadedLegacy.WebhookSecret != "" {
		t.Fatalf("LoadFactory legacy exposed raw webhook secret: %q", loadedLegacy.WebhookSecret)
	}
	loadedLegacy.WebhookSecret = "mutated-secret"
	loadedLegacyAgain, err := env.LoadFactory("legacy-factory")
	if err != nil {
		t.Fatalf("LoadFactory legacy again: %v", err)
	}
	if loadedLegacyAgain.WebhookSecret != "" {
		t.Fatalf("LoadFactory exposed mutable hub factory; got %q", loadedLegacyAgain.WebhookSecret)
	}

	loadedRef, err := env.LoadFactory("ref-factory")
	if err != nil {
		t.Fatalf("LoadFactory ref: %v", err)
	}
	if loadedRef.WebhookSecret != "" || loadedRef.WebhookSecretRef != "github_webhook" {
		t.Fatalf("LoadFactory ref webhook metadata = %#v", loadedRef)
	}

	loadedFactoriesJSON, err := json.Marshal([]*types.FactoryConfig{loadedExternal, loadedLegacyAgain, loadedRef})
	if err != nil {
		t.Fatalf("marshal loaded factories: %v", err)
	}
	for _, secret := range []string{"external-factory-secret", "legacy-factory-secret"} {
		if strings.Contains(string(loadedFactoriesJSON), secret) {
			t.Fatalf("loaded factory contains raw webhook secret %q: %s", secret, loadedFactoriesJSON)
		}
	}
}
