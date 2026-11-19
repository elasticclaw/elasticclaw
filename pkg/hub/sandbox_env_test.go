package hub

import (
	"bytes"
	"context"
	"errors"
	"log"
	"reflect"
	"strings"
	"testing"

	"github.com/elasticclaw/elasticclaw/pkg/cliversion"
	dockerpkg "github.com/elasticclaw/elasticclaw/pkg/provider/docker"
	"github.com/elasticclaw/elasticclaw/pkg/types"
)

func TestIsReservedSandboxEnvKey(t *testing.T) {
	for _, name := range []string{
		"NPM_CONFIG_REGISTRY", "npm_config_registry", "NpM_cOnFiG_registry",
		"YARN_REGISTRY", "PNPM_HOME", "NODE_OPTIONS", "NODE_PATH", "PATH",
		"LD_PRELOAD", "LD_LIBRARY_PATH", "NIX_CONFIG", "NIX_PATH",
		"ELASTICCLAW_CODEX_PLUGIN_VERSION", "ELASTICCLAW_CODEX_CLI_VERSION",
		"ELASTICCLAW_GROK_CLI_VERSION", "ELASTICCLAW_FUTURE_VERSION",
	} {
		t.Run(name, func(t *testing.T) {
			if !isReservedSandboxEnvKey(name) {
				t.Errorf("%s should be reserved", name)
			}
		})
	}
	for name := range sandboxManagedEnvKeys {
		t.Run(name, func(t *testing.T) {
			if !isReservedSandboxEnvKey(name) {
				t.Errorf("managed key %s should be reserved", name)
			}
		})
	}
	for _, name := range []string{"", "APP_TOKEN", "NPM_TOKEN", "MY_PATH", "PATH_EXTRA", "ELASTICCLAW_VERSION_EXTRA", "CODEX_CLI_VERSION", "node_options", "yarn_registry"} {
		t.Run(name, func(t *testing.T) {
			if isReservedSandboxEnvKey(name) {
				t.Errorf("%s should be allowed", name)
			}
		})
	}
}

func TestSandboxEnvFilterLogsNamesOnce(t *testing.T) {
	var output bytes.Buffer
	original := log.Writer()
	log.SetOutput(&output)
	t.Cleanup(func() { log.SetOutput(original) })
	filter := sandboxEnvFilter{}
	for range 2 {
		if filter.allow("NPM_CONFIG_REGISTRY") {
			t.Fatal("reserved key allowed")
		}
	}
	if !filter.allow("APP_TOKEN") {
		t.Fatal("normal key rejected")
	}
	filter.log("claw-123")
	if strings.Count(output.String(), "NPM_CONFIG_REGISTRY") != 1 || !strings.Contains(output.String(), "claw-123") {
		t.Fatalf("unexpected log: %s", output.String())
	}
}

func TestResolvedSandboxEnvForDaytonaAndDocker(t *testing.T) {
	for _, provider := range []string{"daytona", "docker"} {
		t.Run(provider, func(t *testing.T) {
			s, _, _ := newClawRetryTestServer(t, "provisioning")
			s.hubCfg.URL = "http://localhost:8080"
			s.hubCfg.ClawToken = "managed-token"
			s.hubCfg.Secrets = map[string]string{"untrusted": "must-not-reach-provider", "normal": "app-secret"}
			tmpl := &types.TemplateConfig{
				Env: types.WorkspaceEnv{
					"NPM_CONFIG_REGISTRY":           {Value: "must-not-reach-provider"},
					"ELASTICCLAW_CODEX_CLI_VERSION": {Value: "must-not-reach-provider"},
					"ELASTICCLAW_CLAW_TOKEN":        {Value: "must-not-reach-provider"},
					"APP_MODE":                      {Value: "normal"},
				},
				SecretRefs: map[string]string{"NODE_OPTIONS": "untrusted"},
			}
			workflow := &types.WorkflowConfig{SecretRefs: map[string]string{
				"NPM_CONFIG_REGISTRY": "untrusted", "ELASTICCLAW_CODEX_CLI_VERSION": "untrusted",
				"ELASTICCLAW_CLAW_ID": "untrusted", "APP_TOKEN": "normal",
			}}
			env, secrets, err := s.resolveClawEnv("sandbox-env-test", workflow, tmpl, "retry-claw")
			if err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(secrets, map[string]string{"APP_TOKEN": "workflow secret_ref"}) {
				t.Fatalf("resolved secrets contain rejected keys: %v", secrets)
			}
			var got map[string]string
			if provider == "docker" {
				original := createDockerContainer
				t.Cleanup(func() { createDockerContainer = original })
				stopped := errors.New("stop before container creation")
				createDockerContainer = func(_ *dockerpkg.Provider, _ context.Context, req types.CreateRequest) (*types.Instance, error) {
					got = req.Env
					return nil, stopped
				}
				err = s.provisionWithProvider(context.Background(), provider, "retry-claw", types.CreateClawRequest{Env: env}, types.ProviderConfig{}, nil, env)
				if !errors.Is(err, stopped) {
					t.Fatalf("provision: %v", err)
				}
			} else {
				original := createDaytonaSandbox
				t.Cleanup(func() { createDaytonaSandbox = original })
				stopped := errors.New("stop before sandbox creation")
				createDaytonaSandbox = func(_ context.Context, _ daytonaSandboxProvisioner, req types.CreateRequest, configuredEnv map[string]string, _ func(*types.Instance) error) (*types.Instance, error) {
					got = req.Env
					if !reflect.DeepEqual(got, configuredEnv) {
						t.Error("create and configure received different environments")
					}
					return nil, stopped
				}
				err = s.provisionWithProvider(context.Background(), provider, "retry-claw", types.CreateClawRequest{Env: env}, types.ProviderConfig{APIKey: "test-key"}, nil, env)
				if !errors.Is(err, stopped) {
					t.Fatalf("provision: %v", err)
				}
			}
			for _, name := range []string{"NPM_CONFIG_REGISTRY", "ELASTICCLAW_CODEX_CLI_VERSION", "NODE_OPTIONS"} {
				if _, ok := got[name]; ok {
					t.Errorf("reserved user key %s reached %s", name, provider)
				}
			}
			for name, want := range map[string]string{
				"APP_MODE": "normal", "APP_TOKEN": "app-secret",
				"ELASTICCLAW_CLAW_ID": "retry-claw", "ELASTICCLAW_CLAW_TOKEN": "managed-token",
			} {
				if got[name] != want {
					t.Errorf("%s = %q, want %q", name, got[name], want)
				}
			}
		})
	}
}

func TestDaytonaInstallVersionsIgnoreEnv(t *testing.T) {
	t.Setenv("ELASTICCLAW_CODEX_CLI_VERSION", "untrusted-version")
	t.Setenv("ELASTICCLAW_GROK_CLI_VERSION", "untrusted-version")
	t.Setenv("ELASTICCLAW_CODEX_PLUGIN_VERSION", "untrusted-version")
	for _, tc := range []struct{ name, command, spec string }{
		{"codex", daytonaInstallCodingModelCLICommand("codex/model"), "@openai/codex@" + cliversion.CodexCLIVersion},
		{"grok", daytonaInstallCodingModelCLICommand("grok/model"), "@xai-official/grok@" + cliversion.GrokCLIVersion},
		{"plugin", daytonaInstallModelPluginCommand("codex"), "npm:@openclaw/codex@" + cliversion.CodexPluginVersion},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if !strings.Contains(tc.command, tc.spec) || strings.Contains(tc.command, "untrusted-version") {
				t.Fatalf("install command does not use binary version: %s", tc.command)
			}
		})
	}
}
