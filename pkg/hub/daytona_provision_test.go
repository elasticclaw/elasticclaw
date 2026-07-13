package hub

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"

	"github.com/elasticclaw/elasticclaw/pkg/types"
)

type recordingDaytonaSandboxProvisioner struct {
	calls         []string
	configuredEnv map[string]string
	configureErr  error
}

func (p *recordingDaytonaSandboxProvisioner) Create(_ context.Context, req types.CreateRequest) (*types.Instance, error) {
	p.calls = append(p.calls, "create")
	return &types.Instance{ID: "sandbox-123", Name: req.Name, Provider: "daytona"}, nil
}

func (p *recordingDaytonaSandboxProvisioner) ConfigureOpenClaw(_ context.Context, instanceID string, env map[string]string) error {
	if !reflect.DeepEqual(p.calls, []string{"create"}) {
		return errors.New("ConfigureOpenClaw was not called immediately after Create")
	}
	p.calls = append(p.calls, "configure-openclaw:"+instanceID)
	p.configuredEnv = env
	return p.configureErr
}

func TestCreateAndConfigureDaytonaSandboxMaterializesEnvBeforeBootstrap(t *testing.T) {
	p := &recordingDaytonaSandboxProvisioner{}
	env := map[string]string{
		"ELASTICCLAW_CLAW_ID":   "claw-123",
		"AWS_ACCESS_KEY_ID":     "resolved-workflow-access-key",
		"AWS_SECRET_ACCESS_KEY": "resolved-workflow-secret-key",
	}

	instance, err := createAndConfigureDaytonaSandbox(context.Background(), p, types.CreateRequest{Name: "ec-claw123", Env: env}, env)
	if err != nil {
		t.Fatalf("createAndConfigureDaytonaSandbox: %v", err)
	}
	p.calls = append(p.calls, "bootstrap")

	if instance.ID != "sandbox-123" {
		t.Fatalf("instance ID = %q, want sandbox-123", instance.ID)
	}
	if !reflect.DeepEqual(p.calls, []string{"create", "configure-openclaw:sandbox-123", "bootstrap"}) {
		t.Fatalf("call order = %v", p.calls)
	}
	if !reflect.DeepEqual(p.configuredEnv, env) {
		t.Fatal("ConfigureOpenClaw did not receive the resolved workflow environment")
	}
}

func TestCreateAndConfigureDaytonaSandboxStopsOnMaterializationFailureWithoutLeakingSecrets(t *testing.T) {
	const secret = "must-not-appear-in-error"
	p := &recordingDaytonaSandboxProvisioner{configureErr: errors.New("upload failed")}
	env := map[string]string{"AWS_SECRET_ACCESS_KEY": secret}

	instance, err := createAndConfigureDaytonaSandbox(context.Background(), p, types.CreateRequest{Env: env}, env)
	if err == nil {
		t.Fatal("expected configuration error")
	}
	if instance != nil {
		t.Fatalf("instance = %#v, want nil on configuration failure", instance)
	}
	if !strings.Contains(err.Error(), "daytona configure OpenClaw environment for sandbox sandbox-123: upload failed") {
		t.Fatalf("error lacks safe context: %v", err)
	}
	if strings.Contains(err.Error(), secret) {
		t.Fatal("error leaked secret value")
	}
	if !reflect.DeepEqual(p.calls, []string{"create", "configure-openclaw:sandbox-123"}) {
		t.Fatalf("provisioning continued after materialization failure: %v", p.calls)
	}
}
