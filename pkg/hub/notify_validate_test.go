package hub

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/elasticclaw/elasticclaw/pkg/types"
)

// Author-time validation of notify "via" references: a workflow whose notify
// action names a blank or undefined notifier must be rejected when it is
// SAVED, where the author gets blocking feedback — while the runtime stays
// lenient (see TestNotifyActionUnknownViaIsNonBlocking,
// TestNotifyActionBlankViaWarnsAndContinues and
// TestParseNotifyActionMissingViaIsNotFatal for the runtime half of the
// contract).

func newNotifyValidateTestServer(t *testing.T, notifications *types.NotificationsConfig) *Server {
	t.Helper()
	t.Setenv("ELASTICCLAW_HUB_CONFIG", t.TempDir()+"/hub.yaml")
	s, _ := NewTestServerWithConfig(t, &types.HubConfig{
		Token:         "test-token",
		Notifications: notifications,
	}, "", "", "")
	SaveWorkspaceForTest(t, &types.WorkspaceConfig{Name: "engineering"}, nil)
	return s
}

func notifyValidateTestNotifiers() *types.NotificationsConfig {
	return &types.NotificationsConfig{
		Notifiers: map[string]types.NotifierConfig{
			"eng-agents": {Type: "slack", Settings: map[string]any{"token_secret": "slack_tok", "channel": "C0123ABCD"}},
			"ops":        {Type: "slack", Settings: map[string]any{"token_secret": "slack_tok", "channel": "C0456EFGH"}},
		},
	}
}

func pushWorkflowsForTest(t *testing.T, s *Server, workflows []*types.WorkflowConfig) *httptest.ResponseRecorder {
	t.Helper()
	body, err := json.Marshal(WorkflowPushRequest{Workflows: workflows})
	if err != nil {
		t.Fatalf("marshal push request: %v", err)
	}
	req := httptest.NewRequest(http.MethodPost, "/api/workspaces/engineering/workflows", bytes.NewReader(body))
	req.Header.Set("Authorization", "Bearer test-token")
	req.Header.Set("Content-Type", "application/json")
	rr := httptest.NewRecorder()
	s.Handler().ServeHTTP(rr, req)
	return rr
}

func notifyWorkflowStage(id string, notify map[string]interface{}) types.WorkflowStage {
	return types.WorkflowStage{ID: id, OnEnter: map[string]interface{}{"notify": notify}}
}

func TestWorkflowPushRejectsBlankNotifyVia(t *testing.T) {
	s := newNotifyValidateTestServer(t, notifyValidateTestNotifiers())
	rr := pushWorkflowsForTest(t, s, []*types.WorkflowConfig{{
		Name:                "release",
		Integration:         "github",
		EnableManualTrigger: true,
		Stages: []types.WorkflowStage{
			notifyWorkflowStage("announce", map[string]interface{}{"text": "hi"}),
		},
	}})
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400; body = %s", rr.Code, rr.Body.String())
	}
	body := rr.Body.String()
	if !strings.Contains(body, `stage "announce"`) || !strings.Contains(body, `without a "via"`) {
		t.Fatalf("error does not name the stage and the blank via: %s", body)
	}
}

func TestWorkflowPushRejectsUnknownNotifyViaNamingStageAndNotifiers(t *testing.T) {
	s := newNotifyValidateTestServer(t, notifyValidateTestNotifiers())
	rr := pushWorkflowsForTest(t, s, []*types.WorkflowConfig{{
		Name:                "release",
		Integration:         "github",
		EnableManualTrigger: true,
		Stages: []types.WorkflowStage{
			notifyWorkflowStage("announce", map[string]interface{}{"via": "eng-agentz", "text": "hi"}),
		},
	}})
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400; body = %s", rr.Code, rr.Body.String())
	}
	body := rr.Body.String()
	if !strings.Contains(body, `stage "announce"`) || !strings.Contains(body, `"eng-agentz"`) {
		t.Fatalf("error does not name the stage and the dangling via: %s", body)
	}
	if !strings.Contains(body, "defined notifiers: eng-agents, ops") {
		t.Fatalf("error does not list the defined notifiers: %s", body)
	}
}

// A via that resolves to a notifier of an unregistered provider type must be
// rejected at save time too: ValidateNotificationsConfig only requires a
// non-blank type, so without the notify.Supported check here,
// `notifiers.eng: {type: teams}` plus `via: eng` saved with a 200 and the
// failure only surfaced at runtime as a per-send warning — defeating the
// author-time rejection this branch exists for.
func TestWorkflowPushRejectsNotifyViaWithUnsupportedType(t *testing.T) {
	notifications := notifyValidateTestNotifiers()
	notifications.Notifiers["eng"] = types.NotifierConfig{Type: "teams", Settings: map[string]any{}}
	s := newNotifyValidateTestServer(t, notifications)
	rr := pushWorkflowsForTest(t, s, []*types.WorkflowConfig{{
		Name:                "release",
		Integration:         "github",
		EnableManualTrigger: true,
		Stages: []types.WorkflowStage{
			notifyWorkflowStage("announce", map[string]interface{}{"via": "eng", "text": "hi"}),
		},
	}})
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400; body = %s", rr.Code, rr.Body.String())
	}
	body := rr.Body.String()
	if !strings.Contains(body, `stage "announce"`) || !strings.Contains(body, `"teams"`) {
		t.Fatalf("error does not name the stage and the unsupported type: %s", body)
	}
}

// Every offending notify action must be reported in the one rejection, not
// just the first, so the author fixes them all in a single round trip.
func TestWorkflowPushReportsAllBadNotifyActions(t *testing.T) {
	s := newNotifyValidateTestServer(t, notifyValidateTestNotifiers())
	rr := pushWorkflowsForTest(t, s, []*types.WorkflowConfig{{
		Name:                "release",
		Integration:         "github",
		EnableManualTrigger: true,
		Stages: []types.WorkflowStage{
			notifyWorkflowStage("no-via", map[string]interface{}{"text": "hi"}),
			notifyWorkflowStage("fine", map[string]interface{}{"via": "eng-agents", "text": "hi"}),
			notifyWorkflowStage("ghost-via", map[string]interface{}{"via": "ghost", "text": "hi"}),
		},
	}})
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400; body = %s", rr.Code, rr.Body.String())
	}
	body := rr.Body.String()
	if !strings.Contains(body, `stage "no-via"`) || !strings.Contains(body, `stage "ghost-via"`) {
		t.Fatalf("error does not report every offending action: %s", body)
	}
	if strings.Contains(body, `stage "fine"`) {
		t.Fatalf("error flags the valid action too: %s", body)
	}
}

func TestWorkflowPushAcceptsValidNotifyVia(t *testing.T) {
	s := newNotifyValidateTestServer(t, notifyValidateTestNotifiers())
	rr := pushWorkflowsForTest(t, s, []*types.WorkflowConfig{{
		Name:                "release",
		Integration:         "github",
		EnableManualTrigger: true,
		Stages: []types.WorkflowStage{
			notifyWorkflowStage("announce", map[string]interface{}{"via": "eng-agents", "text": "hi"}),
		},
	}})
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body = %s", rr.Code, rr.Body.String())
	}
	req := httptest.NewRequest(http.MethodGet, "/api/workspaces/engineering/workflows/release", nil)
	req.Header.Set("Authorization", "Bearer test-token")
	get := httptest.NewRecorder()
	s.Handler().ServeHTTP(get, req)
	if get.Code != http.StatusOK {
		t.Fatalf("saved workflow not readable: status = %d, body = %s", get.Code, get.Body.String())
	}
}

// A severity typo is an author-time error too: pipeline.Parse stays lenient
// on notify blocks (a bad severity must not kill every stage action of a
// running pipeline), so this save path is the only place the author can be
// told about it while nothing is running.
func TestWorkflowPushRejectsInvalidNotifySeverity(t *testing.T) {
	s := newNotifyValidateTestServer(t, notifyValidateTestNotifiers())
	rr := pushWorkflowsForTest(t, s, []*types.WorkflowConfig{{
		Name:                "release",
		Integration:         "github",
		EnableManualTrigger: true,
		Stages: []types.WorkflowStage{
			notifyWorkflowStage("announce", map[string]interface{}{"via": "eng-agents", "text": "hi", "severity": "urgent"}),
		},
	}})
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400; body = %s", rr.Code, rr.Body.String())
	}
	if body := rr.Body.String(); !strings.Contains(body, `stage "announce"`) || !strings.Contains(body, `"urgent"`) {
		t.Fatalf("error does not name the stage and the bad severity: %s", body)
	}
}

func TestWorkflowPushAcceptsKnownNotifySeverity(t *testing.T) {
	s := newNotifyValidateTestServer(t, notifyValidateTestNotifiers())
	rr := pushWorkflowsForTest(t, s, []*types.WorkflowConfig{{
		Name:                "release",
		Integration:         "github",
		EnableManualTrigger: true,
		Stages: []types.WorkflowStage{
			notifyWorkflowStage("announce", map[string]interface{}{"via": "eng-agents", "text": "hi", "severity": "warning"}),
		},
	}})
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body = %s", rr.Code, rr.Body.String())
	}
}

// The check must cover both authoring shapes: a directly authored
// pipeline_yaml: string is judged exactly like stages:.
func TestWorkflowPushValidatesAuthoredPipelineYAML(t *testing.T) {
	s := newNotifyValidateTestServer(t, notifyValidateTestNotifiers())
	const pipelineYAML = "stages:\n  - id: announce\n    on_enter:\n      notify:\n        via: %s\n        text: hi\n"

	rr := pushWorkflowsForTest(t, s, []*types.WorkflowConfig{{
		Name:                "rawflow",
		Integration:         "github",
		EnableManualTrigger: true,
		PipelineYAML:        strings.Replace(pipelineYAML, "%s", "ghost", 1),
	}})
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("dangling via in pipeline_yaml: status = %d, want 400; body = %s", rr.Code, rr.Body.String())
	}
	if body := rr.Body.String(); !strings.Contains(body, `stage "announce"`) || !strings.Contains(body, `"ghost"`) {
		t.Fatalf("error does not name the stage and via: %s", body)
	}

	rr = pushWorkflowsForTest(t, s, []*types.WorkflowConfig{{
		Name:                "rawflow",
		Integration:         "github",
		EnableManualTrigger: true,
		PipelineYAML:        strings.Replace(pipelineYAML, "%s", "ops", 1),
	}})
	if rr.Code != http.StatusOK {
		t.Fatalf("valid via in pipeline_yaml: status = %d, want 200; body = %s", rr.Code, rr.Body.String())
	}
}

// A workflow with no notify action is unaffected, even on a hub with no
// notifications config at all; one WITH a notify action on such a hub is
// rejected and says that no notifiers are defined.
func TestWorkflowPushWithoutNotifyActionsIsUnaffected(t *testing.T) {
	s := newNotifyValidateTestServer(t, nil)
	rr := pushWorkflowsForTest(t, s, []*types.WorkflowConfig{{
		Name:                "plain",
		Integration:         "github",
		EnableManualTrigger: true,
		Stages: []types.WorkflowStage{
			{ID: "work", OnEnter: map[string]interface{}{"inject": "go"}},
		},
	}})
	if rr.Code != http.StatusOK {
		t.Fatalf("workflow without notify: status = %d, want 200; body = %s", rr.Code, rr.Body.String())
	}

	rr = pushWorkflowsForTest(t, s, []*types.WorkflowConfig{{
		Name:                "noisy",
		Integration:         "github",
		EnableManualTrigger: true,
		Stages: []types.WorkflowStage{
			notifyWorkflowStage("announce", map[string]interface{}{"via": "eng-agents", "text": "hi"}),
		},
	}})
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("notify with no notifiers configured: status = %d, want 400; body = %s", rr.Code, rr.Body.String())
	}
	if body := rr.Body.String(); !strings.Contains(body, "no notifiers are defined") {
		t.Fatalf("error does not say no notifiers are defined: %s", body)
	}
}

const factoryPipelineYAMLTemplate = "stages:\n  - id: announce\n    on_enter:\n      notify:\n        via: %s\n        text: hi\n"

func notifyTestFactory(name, via string) *types.FactoryConfig {
	return &types.FactoryConfig{
		Name:                name,
		Integration:         "github",
		Template:            "default",
		EnableManualTrigger: true,
		PipelineYAML:        strings.Replace(factoryPipelineYAMLTemplate, "%s", via, 1),
	}
}

func savedFactoryCount(t *testing.T, s *Server, name string) int {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, "/api/factories?name="+name, nil)
	req.Header.Set("Authorization", "Bearer test-token")
	rr := httptest.NewRecorder()
	s.Handler().ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("list factories: status = %d, body = %s", rr.Code, rr.Body.String())
	}
	var views []FactoryPushView
	if err := json.NewDecoder(rr.Body).Decode(&views); err != nil {
		t.Fatalf("decode factory list: %v", err)
	}
	return len(views)
}

// POST /api/factories is retired with on-disk factories/. Push is rejected
// with 410 Gone and must not write anything.
func TestFactoriesPushIsRetired(t *testing.T) {
	s := newNotifyValidateTestServer(t, notifyValidateTestNotifiers())
	body, err := json.Marshal(FactoryPushRequest{Factories: []*types.FactoryConfig{
		notifyTestFactory("probe", "eng-agents"),
	}})
	if err != nil {
		t.Fatalf("marshal factories push: %v", err)
	}
	req := httptest.NewRequest(http.MethodPost, "/api/factories", bytes.NewReader(body))
	req.Header.Set("Authorization", "Bearer test-token")
	req.Header.Set("Content-Type", "application/json")
	rr := httptest.NewRecorder()
	s.Handler().ServeHTTP(rr, req)
	if rr.Code != http.StatusGone {
		t.Fatalf("status = %d, want 410; body = %s", rr.Code, rr.Body.String())
	}
	if !strings.Contains(rr.Body.String(), "factories are retired") {
		t.Fatalf("body = %q, want retired message", rr.Body.String())
	}
	if n := savedFactoryCount(t, s, "probe"); n != 0 {
		t.Fatalf("retired factory push still saved %d factories", n)
	}
}

// Settings factory patches are rejected the same way.
func TestSettingsPatchRejectsFactories(t *testing.T) {
	s := newNotifyValidateTestServer(t, notifyValidateTestNotifiers())
	body, err := json.Marshal(SettingsPatch{Factories: []FactoryPatch{{
		Name:                "probe",
		Integration:         "github",
		Template:            "default",
		EnableManualTrigger: true,
		PipelineYAML:        strings.Replace(factoryPipelineYAMLTemplate, "%s", "ops", 1),
	}}})
	if err != nil {
		t.Fatalf("marshal settings patch: %v", err)
	}
	req := httptest.NewRequest(http.MethodPatch, "/api/settings", bytes.NewReader(body))
	req.Header.Set("Authorization", "Bearer test-token")
	req.Header.Set("Content-Type", "application/json")
	rr := httptest.NewRecorder()
	s.Handler().ServeHTTP(rr, req)
	if rr.Code != http.StatusGone {
		t.Fatalf("status = %d, want 410; body = %s", rr.Code, rr.Body.String())
	}
	if n := savedFactoryCount(t, s, "probe"); n != 0 {
		t.Fatalf("settings factory patch still saved %d factories", n)
	}
}

// The workspaces push endpoint carries nested workflows; a bad notify via in
// one must fail the push as a 400 before anything is written.
func TestWorkspacesPushRejectsNestedWorkflowBadNotifyVia(t *testing.T) {
	s := newNotifyValidateTestServer(t, notifyValidateTestNotifiers())
	body, err := json.Marshal(WorkspacePushRequest{Workspaces: []*types.WorkspaceConfig{{
		Name: "platform",
		Workflows: []*types.WorkflowConfig{{
			Name:                "release",
			Integration:         "github",
			EnableManualTrigger: true,
			Stages: []types.WorkflowStage{
				notifyWorkflowStage("announce", map[string]interface{}{"via": "ghost", "text": "hi"}),
			},
		}},
	}}})
	if err != nil {
		t.Fatalf("marshal workspace push: %v", err)
	}
	req := httptest.NewRequest(http.MethodPost, "/api/workspaces", bytes.NewReader(body))
	req.Header.Set("Authorization", "Bearer test-token")
	req.Header.Set("Content-Type", "application/json")
	rr := httptest.NewRecorder()
	s.Handler().ServeHTTP(rr, req)
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400; body = %s", rr.Code, rr.Body.String())
	}
	respBody := rr.Body.String()
	if !strings.Contains(respBody, `workspace "platform"`) || !strings.Contains(respBody, `stage "announce"`) || !strings.Contains(respBody, `"ghost"`) {
		t.Fatalf("error does not locate the offending action: %s", respBody)
	}
}
