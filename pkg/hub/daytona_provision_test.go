package hub

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/elasticclaw/elasticclaw/pkg/types"
)

type recordingDaytonaSandboxProvisioner struct {
	calls             []string
	configuredEnv     map[string]string
	configureErrors   []error
	destroyErr        error
	destroyContextErr error
}

func (p *recordingDaytonaSandboxProvisioner) Create(_ context.Context, req types.CreateRequest) (*types.Instance, error) {
	p.calls = append(p.calls, "create")
	return &types.Instance{ID: "sandbox-123", Name: req.Name, Provider: "daytona"}, nil
}

func (p *recordingDaytonaSandboxProvisioner) ConfigureOpenClaw(_ context.Context, instanceID string, env map[string]string) error {
	if len(p.calls) == 0 || p.calls[0] != "create" {
		return errors.New("ConfigureOpenClaw was called before Create")
	}
	p.calls = append(p.calls, "configure-openclaw:"+instanceID)
	p.configuredEnv = env
	if len(p.configureErrors) == 0 {
		return nil
	}
	err := p.configureErrors[0]
	p.configureErrors = p.configureErrors[1:]
	return err
}

func (p *recordingDaytonaSandboxProvisioner) Destroy(ctx context.Context, instanceID string, _ bool) error {
	p.calls = append(p.calls, "destroy:"+instanceID)
	p.destroyContextErr = ctx.Err()
	return p.destroyErr
}

func TestCreateAndConfigureDaytonaSandboxMaterializesEnvBeforeBootstrap(t *testing.T) {
	p := &recordingDaytonaSandboxProvisioner{}
	env := map[string]string{
		"ELASTICCLAW_CLAW_ID":   "claw-123",
		"AWS_ACCESS_KEY_ID":     "resolved-workflow-access-key",
		"AWS_SECRET_ACCESS_KEY": "resolved-workflow-secret-key",
	}

	instance, err := createAndConfigureDaytonaSandboxWithRetry(context.Background(), p, types.CreateRequest{Name: "ec-claw123", Env: env}, env, func(instance *types.Instance) error {
		p.calls = append(p.calls, "record:"+instance.ID)
		return nil
	}, nil)
	if err != nil {
		t.Fatalf("createAndConfigureDaytonaSandbox: %v", err)
	}
	p.calls = append(p.calls, "bootstrap")

	if instance.ID != "sandbox-123" {
		t.Fatalf("instance ID = %q, want sandbox-123", instance.ID)
	}
	if !reflect.DeepEqual(p.calls, []string{"create", "record:sandbox-123", "configure-openclaw:sandbox-123", "bootstrap"}) {
		t.Fatalf("call order = %v", p.calls)
	}
	if !reflect.DeepEqual(p.configuredEnv, env) {
		t.Fatal("ConfigureOpenClaw did not receive the resolved workflow environment")
	}
}

func TestCreateAndConfigureDaytonaSandboxStopsOnMaterializationFailureWithoutLeakingSecrets(t *testing.T) {
	const secret = "must-not-appear-in-error"
	p := &recordingDaytonaSandboxProvisioner{configureErrors: []error{errors.New("upload failed")}}
	env := map[string]string{"AWS_SECRET_ACCESS_KEY": secret}

	instance, err := createAndConfigureDaytonaSandboxWithRetry(context.Background(), p, types.CreateRequest{Env: env}, env, nil, nil)
	if err == nil {
		t.Fatal("expected configuration error")
	}
	if instance != nil {
		t.Fatalf("instance = %#v, want nil on configuration failure", instance)
	}
	if !strings.Contains(err.Error(), "daytona configure OpenClaw environment for sandbox sandbox-123 after 1 attempts: upload failed") {
		t.Fatalf("error lacks safe context: %v", err)
	}
	if strings.Contains(err.Error(), secret) {
		t.Fatal("error leaked secret value")
	}
	if !reflect.DeepEqual(p.calls, []string{"create", "configure-openclaw:sandbox-123", "destroy:sandbox-123"}) {
		t.Fatalf("provisioning continued after materialization failure: %v", p.calls)
	}
}

func TestCreateAndConfigureDaytonaSandboxRetriesUntilSandboxIsReady(t *testing.T) {
	p := &recordingDaytonaSandboxProvisioner{configureErrors: []error{errors.New("sandbox not ready"), nil}}

	instance, err := createAndConfigureDaytonaSandboxWithRetry(context.Background(), p, types.CreateRequest{}, nil, nil, []time.Duration{0})
	if err != nil {
		t.Fatalf("createAndConfigureDaytonaSandboxWithRetry: %v", err)
	}
	if instance == nil || instance.ID != "sandbox-123" {
		t.Fatalf("instance = %#v, want sandbox-123", instance)
	}
	if !reflect.DeepEqual(p.calls, []string{"create", "configure-openclaw:sandbox-123", "configure-openclaw:sandbox-123"}) {
		t.Fatalf("call order = %v", p.calls)
	}
}

func TestDaytonaConfigureUsesLongBootstrapRetryPolicy(t *testing.T) {
	want := []time.Duration{5 * time.Second, 10 * time.Second, 20 * time.Second, 40 * time.Second, 60 * time.Second}
	if !reflect.DeepEqual(daytonaLongRetryDelays, want) {
		t.Fatalf("Daytona retry delays = %v, want %v", daytonaLongRetryDelays, want)
	}
}

func TestCreateAndConfigureDaytonaSandboxCleansUpWithIndependentContext(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	p := &recordingDaytonaSandboxProvisioner{configureErrors: []error{errors.New("sandbox not ready")}}

	_, err := createAndConfigureDaytonaSandboxWithRetry(ctx, p, types.CreateRequest{}, nil, nil, nil)
	if err == nil {
		t.Fatal("expected configuration error")
	}
	if p.destroyContextErr != nil {
		t.Fatalf("cleanup inherited canceled context: %v", p.destroyContextErr)
	}
	if !reflect.DeepEqual(p.calls, []string{"create", "configure-openclaw:sandbox-123", "destroy:sandbox-123"}) {
		t.Fatalf("call order = %v", p.calls)
	}
}

func TestCreateAndConfigureDaytonaSandboxRecordsIDBeforeConfigurationAndCleanup(t *testing.T) {
	p := &recordingDaytonaSandboxProvisioner{
		configureErrors: []error{errors.New("upload failed")},
		destroyErr:      errors.New("destroy timed out"),
	}
	recordedID := ""

	_, err := createAndConfigureDaytonaSandboxWithRetry(context.Background(), p, types.CreateRequest{}, nil, func(instance *types.Instance) error {
		recordedID = instance.ID
		p.calls = append(p.calls, "record:"+instance.ID)
		return nil
	}, nil)
	if err == nil {
		t.Fatal("expected configuration and cleanup error")
	}
	if recordedID != "sandbox-123" {
		t.Fatalf("recorded ID = %q, want sandbox-123", recordedID)
	}
	if !strings.Contains(err.Error(), "sandbox cleanup failed: destroy timed out") {
		t.Fatalf("error lacks cleanup context: %v", err)
	}
	if !reflect.DeepEqual(p.calls, []string{"create", "record:sandbox-123", "configure-openclaw:sandbox-123", "destroy:sandbox-123"}) {
		t.Fatalf("call order = %v", p.calls)
	}
}
