package hub

import (
	"strings"
	"testing"

	"github.com/elasticclaw/elasticclaw/pkg/types"
)

func TestCheckLLMKeysRecognizesOllama(t *testing.T) {
	s := &Server{}
	checks := s.checkLLMKeys(&types.HubConfig{
		LLMKeys: types.LLMKeysList{
			{Name: "local-ollama", Provider: "ollama", APIKey: "ollama-local", Default: true},
		},
	})

	for _, check := range checks {
		if strings.Contains(check.Title, "Unknown LLM provider") {
			t.Fatalf("ollama was reported as unknown: %#v", check)
		}
	}
	if len(checks) != 1 || !checks[0].OK {
		t.Fatalf("expected one passing LLM key check, got %#v", checks)
	}
}

func TestCheckLLMKeysAllowsBlankOllamaAPIKey(t *testing.T) {
	s := &Server{}
	checks := s.checkLLMKeys(&types.HubConfig{
		LLMKeys: types.LLMKeysList{
			{Name: "local-ollama", Provider: "ollama", Default: true},
		},
	})

	for _, check := range checks {
		if strings.Contains(check.Title, "has no API key") {
			t.Fatalf("blank ollama key was reported as invalid: %#v", check)
		}
	}
	if len(checks) != 1 || !checks[0].OK {
		t.Fatalf("expected one passing LLM key check, got %#v", checks)
	}
}

func TestCheckNotifiersMissingNotifier(t *testing.T) {
	checks := runNotifierDoctorCheck(t, nil, nil)

	assertDoctorCheck(t, checks, "critical", `Workflow "release" stage "announce" references missing notifier "slack-releases"`, false)
	if checks[0].FixAction == nil || checks[0].FixAction.Type != "navigate" || checks[0].FixAction.Target != "/settings/workspaces" {
		t.Fatalf("unexpected fix action: %#v", checks[0].FixAction)
	}
}

func TestCheckNotifiersUnsupportedType(t *testing.T) {
	checks := runNotifierDoctorCheck(t, map[string]types.NotifierConfig{
		"slack-releases": {Type: "carrier-pigeon"},
	}, nil)

	assertDoctorCheck(t, checks, "critical", `Notifier "slack-releases" has unsupported type "carrier-pigeon"`, false)
}

func TestCheckNotifiersMissingTokenSecret(t *testing.T) {
	checks := runNotifierDoctorCheck(t, map[string]types.NotifierConfig{
		"slack-releases": {Type: "slack"},
	}, nil)

	assertDoctorCheck(t, checks, "warning", `Notifier "slack-releases" has no token_secret`, false)
}

func TestCheckNotifiersMissingReferencedSecret(t *testing.T) {
	checks := runNotifierDoctorCheck(t, map[string]types.NotifierConfig{
		"slack-releases": {
			Type:     "slack",
			Settings: map[string]any{"token_secret": "slack_bot_token"},
		},
	}, nil)

	assertDoctorCheck(t, checks, "warning", `Notifier "slack-releases" token_secret references missing secret "slack_bot_token"`, false)
	if checks[0].FixAction == nil || checks[0].FixAction.Type != "navigate" || checks[0].FixAction.Target != "/settings/secrets" || checks[0].FixAction.Label != "Create Secret" {
		t.Fatalf("unexpected fix action: %#v", checks[0].FixAction)
	}
}

func TestCheckNotifiersConfigured(t *testing.T) {
	checks := runNotifierDoctorCheck(t, map[string]types.NotifierConfig{
		"slack-releases": {
			Type:     "slack",
			Settings: map[string]any{"token_secret": "slack_bot_token"},
		},
	}, map[string]string{"slack_bot_token": "secret"})

	assertDoctorCheck(t, checks, "info", "Workflow notifiers configured", true)
}

func TestCheckNotifiersWorkspaceScopedSecret(t *testing.T) {
	t.Setenv("ELASTICCLAW_HUB_CONFIG", t.TempDir()+"/hub.yaml")
	saveNotifierDoctorWorkspace(t, map[string]types.NotifierConfig{
		"slack-releases": {
			Type:     "slack",
			Settings: map[string]any{"token_secret": "slack_bot_token"},
		},
	}, map[string]interface{}{"via": "slack-releases"})
	if err := saveWorkspaceSecret("engineering", "slack_bot_token", "xoxb"); err != nil {
		t.Fatalf("save workspace secret: %v", err)
	}

	s := &Server{}
	checks := s.checkNotifiers(&types.HubConfig{}, &types.HubConfig{})
	assertDoctorCheck(t, checks, "info", "Workflow notifiers configured", true)
}

func TestCheckNotifiersMissingVia(t *testing.T) {
	t.Setenv("ELASTICCLAW_HUB_CONFIG", t.TempDir()+"/hub.yaml")
	saveNotifierDoctorWorkspace(t, nil, map[string]interface{}{"text": "hello"})

	s := &Server{}
	checks := s.checkNotifiers(&types.HubConfig{}, &types.HubConfig{})
	assertDoctorCheck(t, checks, "critical", `Workflow "release" stage "announce" notify action has no valid "via"`, false)
}

func runNotifierDoctorCheck(t *testing.T, notifiers map[string]types.NotifierConfig, secrets map[string]string) []DoctorCheck {
	t.Helper()
	t.Setenv("ELASTICCLAW_HUB_CONFIG", t.TempDir()+"/hub.yaml")
	saveNotifierDoctorWorkspace(t, notifiers, map[string]interface{}{"via": "slack-releases"})

	s := &Server{}
	return s.checkNotifiers(&types.HubConfig{}, &types.HubConfig{Secrets: secrets})
}

func saveNotifierDoctorWorkspace(t *testing.T, notifiers map[string]types.NotifierConfig, notifyBlock map[string]interface{}) {
	t.Helper()
	SaveWorkspaceForTest(t, &types.WorkspaceConfig{
		Name:      "engineering",
		Notifiers: notifiers,
	}, []*types.WorkflowConfig{{
		Name: "release",
		Stages: []types.WorkflowStage{{
			ID: "announce",
			OnEnter: map[string]interface{}{
				"notify": notifyBlock,
			},
		}},
	}})
}

func assertDoctorCheck(t *testing.T, checks []DoctorCheck, severity, title string, ok bool) {
	t.Helper()
	if len(checks) != 1 {
		t.Fatalf("expected one doctor check, got %#v", checks)
	}
	check := checks[0]
	if check.Category != "workflows" || check.Severity != severity || check.Title != title || check.OK != ok {
		t.Fatalf("unexpected doctor check: %#v", check)
	}
}
