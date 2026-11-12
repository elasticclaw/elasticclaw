package hub

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"

	dockerpkg "github.com/elasticclaw/elasticclaw/pkg/provider/docker"
	"github.com/elasticclaw/elasticclaw/pkg/types"
)

func TestProvisionWithProviderRejectsUnknownProvider(t *testing.T) {
	s := &Server{}
	err := s.provisionWithProvider(context.Background(), "unknown", "claw", types.CreateClawRequest{}, types.ProviderConfig{}, nil, nil)
	if err == nil || err.Error() != "unsupported provider: unknown" {
		t.Fatalf("error = %v, want unsupported provider: unknown", err)
	}
}

func TestProvisionWithProviderNoopRequiresOptIn(t *testing.T) {
	t.Setenv("ELASTICCLAW_NOOP_PROVIDER", "")
	s := &Server{}
	err := s.provisionWithProvider(context.Background(), "noop", "claw", types.CreateClawRequest{}, types.ProviderConfig{}, nil, nil)
	if err == nil || !strings.Contains(err.Error(), "noop provider requires ELASTICCLAW_NOOP_PROVIDER=1") {
		t.Fatalf("error = %v, want noop opt-in guard", err)
	}
}

func TestProvisionStoredClawDispatchesDocker(t *testing.T) {
	s, db, _ := newClawRetryTestServer(t, "provisioning")
	s.hubCfg.Providers = map[string]types.ProviderConfig{"docker": {}}
	if _, err := db.Exec(`UPDATE claws SET provider='docker', template_files='{}' WHERE id='retry-claw'`); err != nil {
		t.Fatal(err)
	}

	original := createDockerContainer
	t.Cleanup(func() { createDockerContainer = original })
	var got *types.CreateRequest
	createDockerContainer = func(_ *dockerpkg.Provider, _ context.Context, req types.CreateRequest) (*types.Instance, error) {
		got = &req
		return nil, errors.New("test container creation stopped")
	}

	s.provisionStoredClaw("retry-claw")
	if got == nil {
		t.Fatal("stored docker claw did not reach Docker container creation")
	}
	if !strings.HasPrefix(got.Name, "ec-retry-cl-r") || got.Env["ELASTICCLAW_CLAW_ID"] != "retry-claw" {
		t.Fatalf("unexpected Docker restore request: name=%q clawID=%q", got.Name, got.Env["ELASTICCLAW_CLAW_ID"])
	}
}

func TestProvisionDockerEnvPrecedence(t *testing.T) {
	s, _, _ := newClawRetryTestServer(t, "provisioning")
	s.hubCfg.Token = "hub-token"
	s.hubCfg.ClawToken = "claw-token"
	s.hubCfg.URL = "http://localhost:8080"
	s.hubCfg.LLMKeys = []*types.LLMKeyConfig{{Name: "model", Provider: "anthropic", APIKey: "managed-api-key", AuthProfile: "profile", Default: true}}
	s.hubCfg.ModelAuthProfiles = []*types.ModelAuthProfileConfig{{Name: "profile", Provider: "anthropic", AuthState: "managed-auth-state"}}
	requestEnv := map[string]string{"REQUEST_ONLY": "request", "SHARED": "request"}
	workflowEnv := map[string]string{
		"WORKFLOW_ONLY": "workflow", "SHARED": "workflow",
		"ELASTICCLAW_HUB_URL": "untrusted", "ELASTICCLAW_CLAW_ID": "untrusted",
		"ELASTICCLAW_CLAW_TOKEN": "untrusted", "ELASTICCLAW_MODEL_AUTH_TOKEN": "untrusted",
		"ELASTICCLAW_BOOTSTRAP": "0", "ANTHROPIC_API_KEY": "untrusted",
		"ELASTICCLAW_MODEL_AUTH_PROVIDER": "untrusted", "ELASTICCLAW_MODEL_AUTH_STATE": "untrusted",
	}
	originalWorkflowEnv := make(map[string]string, len(workflowEnv))
	for k, v := range workflowEnv {
		originalWorkflowEnv[k] = v
	}
	original := createDockerContainer
	t.Cleanup(func() { createDockerContainer = original })
	var got map[string]string
	stopped := errors.New("test container creation stopped")
	createDockerContainer = func(_ *dockerpkg.Provider, _ context.Context, req types.CreateRequest) (*types.Instance, error) {
		got = req.Env
		return nil, stopped
	}
	err := s.provisionWithProvider(context.Background(), "docker", "retry-claw", types.CreateClawRequest{Env: requestEnv}, types.ProviderConfig{}, nil, workflowEnv)
	if !errors.Is(err, stopped) {
		t.Fatalf("error = %v, want container creation seam", err)
	}
	for key, want := range map[string]string{
		"REQUEST_ONLY": "request", "WORKFLOW_ONLY": "workflow", "SHARED": "workflow",
		"ELASTICCLAW_HUB_URL": "http://host.docker.internal:8080", "ELASTICCLAW_CLAW_ID": "retry-claw",
		"ELASTICCLAW_CLAW_TOKEN": "claw-token", "ELASTICCLAW_MODEL_AUTH_TOKEN": s.modelAuthTokenForClaw("retry-claw"),
		"ELASTICCLAW_BOOTSTRAP": "1", "ANTHROPIC_API_KEY": "managed-api-key",
		"ELASTICCLAW_MODEL_AUTH_PROVIDER": "anthropic", "ELASTICCLAW_MODEL_AUTH_STATE": "managed-auth-state",
	} {
		if got[key] != want {
			t.Errorf("%s = %q, want %q", key, got[key], want)
		}
	}
	if !reflect.DeepEqual(workflowEnv, originalWorkflowEnv) || !reflect.DeepEqual(requestEnv, map[string]string{"REQUEST_ONLY": "request", "SHARED": "request"}) {
		t.Fatal("container environment merge mutated an input map")
	}
}
