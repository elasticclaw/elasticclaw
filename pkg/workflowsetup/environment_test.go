package workflowsetup

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/elasticclaw/elasticclaw/pkg/types"
)

type fakeEnvironment struct{}

func (fakeEnvironment) Snapshot() (SetupEnvironmentSnapshot, error) {
	return SetupEnvironmentSnapshot{}, nil
}

func (fakeEnvironment) LoadWorkspace(string) (*types.WorkspaceConfig, error) {
	return nil, nil
}

func (fakeEnvironment) LoadWorkflowRaw(string, string) (string, error) {
	return "", nil
}

func (fakeEnvironment) WorkspaceSecretNames(string) ([]string, error) {
	return nil, nil
}

func (fakeEnvironment) WorkspaceIssueTrackers(string) ([]IssueTrackerRef, error) {
	return nil, nil
}

func (fakeEnvironment) WorkspaceGitHubApps(string) ([]GitHubAppRef, error) {
	return nil, nil
}

func (fakeEnvironment) ListFactories() ([]FactoryRef, error) {
	return nil, nil
}

func (fakeEnvironment) LoadFactory(string) (*types.FactoryConfig, error) {
	return nil, nil
}

var _ Environment = fakeEnvironment{}

func TestSetupEnvironmentSnapshotJSONDoesNotContainSecretValues(t *testing.T) {
	snapshot := SetupEnvironmentSnapshot{
		ClawTokenSet:    true,
		DefaultProvider: "daytona",
		DefaultModel:    "anthropic/claude-sonnet-4-6",
		Providers: []ProviderRef{{
			Name:           "daytona",
			Type:           "daytona",
			Provisionable:  true,
			CredentialsSet: true,
			APIKeySet:      true,
		}},
		LLMKeys: []LLMKeyRef{{
			Name:         "anthropic-prod",
			Provider:     "anthropic",
			KeySet:       true,
			Default:      true,
			DefaultModel: "anthropic/claude-sonnet-4-6",
		}},
		ConcurrencyGroups: []ConcurrencyGroupRef{{
			Name:  "global",
			Limit: 2,
		}},
		HubSecretNames: []string{"github_webhook"},
		IssueTrackers: []IssueTrackerRef{{
			Type:             "github-issues",
			Workspace:        "eng",
			TokenSet:         true,
			WebhookSecretSet: true,
		}},
		GitHubApps: []GitHubAppRef{{
			AppID:         123,
			URL:           "https://github.com/apps/eng",
			PrivateKeySet: true,
		}},
	}

	data, err := json.Marshal(snapshot)
	if err != nil {
		t.Fatalf("marshal snapshot: %v", err)
	}

	for _, secretValue := range []string{"sk-secret", "claw-secret", "-----BEGIN PRIVATE KEY-----"} {
		if strings.Contains(string(data), secretValue) {
			t.Fatalf("snapshot JSON contains secret value %q: %s", secretValue, data)
		}
	}
}
