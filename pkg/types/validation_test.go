package types

import (
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

func TestValidateRepositoryAccessPatterns(t *testing.T) {
	tests := []struct {
		selector string
		valid    bool
	}{
		{selector: "owner/repo", valid: true},
		{selector: "*", valid: true},
		{selector: "*-infra-*", valid: true},
		{selector: "owner/*", valid: true},
		{selector: "owner/repo-?", valid: true},
		{selector: "owner/[ab]pi", valid: true},
		{selector: "repo", valid: false},
		{selector: "owner/", valid: false},
		{selector: "owner/[", valid: false},
		{selector: "owner/repo/extra", valid: false},
		{selector: "owner name/*", valid: false},
	}
	for _, tt := range tests {
		t.Run(tt.selector, func(t *testing.T) {
			workspace := &WorkspaceConfig{
				Name: "test",
				Repositories: []GitHubRepoAccess{{
					Repo:        tt.selector,
					Permissions: "read",
				}},
			}
			err := workspace.Validate()
			if tt.valid && err != nil {
				t.Fatalf("Validate() error = %v", err)
			}
			if !tt.valid && err == nil {
				t.Fatal("Validate() succeeded for invalid repository selector")
			}
		})
	}
}

func TestFactoryConfigValidate(t *testing.T) {
	tests := []struct {
		name    string
		factory *FactoryConfig
		wantErr bool
		errMsg  string
	}{
		{
			name: "valid linear factory",
			factory: &FactoryConfig{
				Name:          "test-factory",
				Integration:   "linear",
				Workspace:     "test-workspace",
				TriggerStatus: "In Progress",
				Template:      "base",
			},
			wantErr: false,
		},
		{
			name: "valid github-issues factory",
			factory: &FactoryConfig{
				Name:        "test-factory",
				Integration: "github-issues",
				Workspace:   "test-workspace",
				Template:    "base",
			},
			wantErr: false,
		},
		{
			name: "missing name",
			factory: &FactoryConfig{
				Integration: "linear",
				Template:    "base",
			},
			wantErr: true,
			errMsg:  "factory name is required",
		},
		{
			name: "missing integration",
			factory: &FactoryConfig{
				Name:     "test-factory",
				Template: "base",
			},
			wantErr: true,
			errMsg:  "integration is required",
		},
		{
			name: "invalid integration",
			factory: &FactoryConfig{
				Name:        "test-factory",
				Integration: "invalid",
				Template:    "base",
			},
			wantErr: true,
			errMsg:  "invalid integration",
		},
		{
			name: "missing template",
			factory: &FactoryConfig{
				Name:        "test-factory",
				Integration: "linear",
			},
			wantErr: true,
			errMsg:  "template is required",
		},
		{
			name: "invalid color",
			factory: &FactoryConfig{
				Name:        "test-factory",
				Integration: "linear",
				Template:    "base",
				Color:       "invalid-color",
			},
			wantErr: true,
			errMsg:  "invalid color",
		},
		{
			name: "valid color",
			factory: &FactoryConfig{
				Name:        "test-factory",
				Integration: "linear",
				Template:    "base",
				Color:       "teal",
			},
			wantErr: false,
		},
		{
			name: "invalid run kind",
			factory: &FactoryConfig{
				Name:        "test-factory",
				Integration: "linear",
				Template:    "base",
				RunKind:     "typo",
			},
			wantErr: true,
			errMsg:  "invalid run_kind",
		},
		{
			name: "valid run kind",
			factory: &FactoryConfig{
				Name:        "test-factory",
				Integration: "linear",
				Template:    "base",
				RunKind:     "pr_task",
			},
			wantErr: false,
		},
		{
			name: "valid run kind code_task",
			factory: &FactoryConfig{
				Name:        "test-factory",
				Integration: "linear",
				Template:    "base",
				RunKind:     "code_task",
			},
			wantErr: false,
		},
		{
			name: "invalid provider",
			factory: &FactoryConfig{
				Name:        "test-factory",
				Integration: "linear",
				Template:    "base",
				Provider:    "invalid-provider",
			},
			wantErr: true,
			errMsg:  "invalid provider",
		},
		{
			name: "valid provider",
			factory: &FactoryConfig{
				Name:        "test-factory",
				Integration: "linear",
				Template:    "base",
				Provider:    "replicated",
			},
			wantErr: false,
		},
		{
			name: "invalid name pattern",
			factory: &FactoryConfig{
				Name:        "test-factory",
				Integration: "linear",
				Template:    "base",
				NamePattern: "{invalid pattern with spaces}",
			},
			wantErr: true,
			errMsg:  "invalid name_pattern",
		},
		{
			name: "valid name pattern",
			factory: &FactoryConfig{
				Name:        "test-factory",
				Integration: "linear",
				Template:    "base",
				NamePattern: "{issue_id}-test",
			},
			wantErr: false,
		},
		{
			name: "valid name pattern with multiple placeholders",
			factory: &FactoryConfig{
				Name:        "test-factory",
				Integration: "linear",
				Template:    "base",
				NamePattern: "{issue_id}-{title}",
			},
			wantErr: false,
		},
		{
			name: "valid name pattern with prefix and multiple placeholders",
			factory: &FactoryConfig{
				Name:        "test-factory",
				Integration: "linear",
				Template:    "base",
				NamePattern: "prefix-{project}-{issue_id}-fix",
			},
			wantErr: false,
		},
		{
			name:    "nil factory",
			factory: nil,
			wantErr: true,
			errMsg:  "factory config is nil",
		},
		{
			name: "valid github factory with trigger",
			factory: &FactoryConfig{
				Name:        "test-factory",
				Integration: "github",
				Template:    "base",
				Trigger: &GitHubTrigger{
					On:     "pull_request",
					Action: "opened",
				},
			},
			wantErr: false,
		},
		{
			name: "valid factory exclude labels",
			factory: &FactoryConfig{
				Name:          "test-factory",
				Integration:   "github",
				Template:      "base",
				ExcludeLabels: []string{"Bug"},
				Trigger: &GitHubTrigger{
					On:     "issue",
					Action: "opened",
				},
			},
			wantErr: false,
		},
		{
			name: "invalid factory blank exclude label",
			factory: &FactoryConfig{
				Name:          "test-factory",
				Integration:   "github",
				Template:      "base",
				ExcludeLabels: []string{" "},
				Trigger: &GitHubTrigger{
					On:     "issue",
					Action: "opened",
				},
			},
			wantErr: true,
			errMsg:  "exclude_labels[0] cannot be blank",
		},
		{
			name: "github factory missing trigger",
			factory: &FactoryConfig{
				Name:        "test-factory",
				Integration: "github",
				Template:    "base",
			},
			wantErr: true,
			errMsg:  "trigger is required",
		},
		{
			name: "github factory with enable_manual_trigger and no trigger",
			factory: &FactoryConfig{
				Name:                "test-factory",
				Integration:         "github",
				Template:            "base",
				EnableManualTrigger: true,
			},
			wantErr: false,
		},
		{
			name: "github factory invalid trigger on",
			factory: &FactoryConfig{
				Name:        "test-factory",
				Integration: "github",
				Template:    "base",
				Trigger: &GitHubTrigger{
					On:     "invalid",
					Action: "opened",
				},
			},
			wantErr: true,
			errMsg:  "invalid trigger.on",
		},
		{
			name: "github factory invalid trigger action",
			factory: &FactoryConfig{
				Name:        "test-factory",
				Integration: "github",
				Template:    "base",
				Trigger: &GitHubTrigger{
					On:     "pull_request",
					Action: "invalid",
				},
			},
			wantErr: true,
			errMsg:  "invalid trigger.action",
		},
		{
			name: "external factory missing external_trigger",
			factory: &FactoryConfig{
				Name:        "test-factory",
				Integration: "external",
				Template:    "base",
			},
			wantErr: true,
			errMsg:  "external_trigger is required",
		},
		{
			name: "external factory with enable_manual_trigger and no external_trigger",
			factory: &FactoryConfig{
				Name:                "test-factory",
				Integration:         "external",
				Template:            "base",
				EnableManualTrigger: true,
			},
			wantErr: false,
		},
		{
			name: "github factory with enable_manual_trigger and invalid trigger",
			factory: &FactoryConfig{
				Name:                "test-factory",
				Integration:         "github",
				Template:            "base",
				EnableManualTrigger: true,
				Trigger: &GitHubTrigger{
					On:     "invalid",
					Action: "opened",
				},
			},
			wantErr: true,
			errMsg:  "invalid trigger.on",
		},
		{
			name: "external factory with enable_manual_trigger and invalid external_trigger",
			factory: &FactoryConfig{
				Name:                "test-factory",
				Integration:         "external",
				Template:            "base",
				EnableManualTrigger: true,
				ExternalTrigger: &ExternalTrigger{
					Source: "invalid-source",
				},
			},
			wantErr: true,
			errMsg:  "invalid external_trigger.source",
		},
		{
			name: "valid repos with wildcard",
			factory: &FactoryConfig{
				Name:        "test-factory",
				Integration: "github-issues",
				Template:    "base",
				Repos:       []string{"owner/*"},
			},
			wantErr: false,
		},
		{
			name: "valid repos with owner/repo",
			factory: &FactoryConfig{
				Name:        "test-factory",
				Integration: "github-issues",
				Template:    "base",
				Repos:       []string{"owner/repo"},
			},
			wantErr: false,
		},
		{
			name: "invalid repos format",
			factory: &FactoryConfig{
				Name:        "test-factory",
				Integration: "github-issues",
				Template:    "base",
				Repos:       []string{"invalid-repo-format"},
			},
			wantErr: true,
			errMsg:  "invalid format",
		},
		{
			name: "empty repo in list",
			factory: &FactoryConfig{
				Name:        "test-factory",
				Integration: "github-issues",
				Template:    "base",
				Repos:       []string{""},
			},
			wantErr: true,
			errMsg:  "cannot be empty",
		},
		{
			name: "invalid wildcard repo missing owner",
			factory: &FactoryConfig{
				Name:        "test-factory",
				Integration: "github-issues",
				Template:    "base",
				Repos:       []string{"/*"},
			},
			wantErr: true,
			errMsg:  "invalid format",
		},
		{
			name: "invalid wildcard repo with extra path",
			factory: &FactoryConfig{
				Name:        "test-factory",
				Integration: "github-issues",
				Template:    "base",
				Repos:       []string{"owner/repo/*"},
			},
			wantErr: true,
			errMsg:  "invalid format",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := tt.factory.Validate()
			if (err != nil) != tt.wantErr {
				t.Errorf("Validate() error = %v, wantErr %v", err, tt.wantErr)
				return
			}
			if err != nil && tt.errMsg != "" {
				if !contains(err.Error(), tt.errMsg) {
					t.Errorf("Validate() error message = %v, should contain %v", err.Error(), tt.errMsg)
				}
			}
		})
	}
}

func TestGitHubFactoryConfigLoadsExcludeLabels(t *testing.T) {
	data := []byte(`
name: github-pr
integration: github
template: base
repos:
  - elastic/claw
labels:
  - agent-ready
exclude_labels:
  - Bug
trigger:
  on: pull_request
  action: opened
`)
	var factory FactoryConfig
	if err := yaml.Unmarshal(data, &factory); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if err := factory.Validate(); err != nil {
		t.Fatalf("validate: %v", err)
	}
	if got := strings.Join(factory.ExcludeLabels, ","); got != "Bug" {
		t.Fatalf("ExcludeLabels = %q, want Bug", got)
	}
}

func TestFactoryInputValidate(t *testing.T) {
	tests := []struct {
		name        string
		factoryName string
		index       int
		input       FactoryInput
		wantErr     bool
		errMsg      string
	}{
		{
			name:        "valid string input",
			factoryName: "test-factory",
			index:       0,
			input: FactoryInput{
				Name: "test-input",
				Type: "string",
			},
			wantErr: false,
		},
		{
			name:        "valid enum input",
			factoryName: "test-factory",
			index:       0,
			input: FactoryInput{
				Name:    "test-input",
				Type:    "enum",
				Options: []string{"option1", "option2"},
			},
			wantErr: false,
		},
		{
			name:        "missing name",
			factoryName: "test-factory",
			index:       0,
			input: FactoryInput{
				Type: "string",
			},
			wantErr: true,
			errMsg:  "name is required",
		},
		{
			name:        "missing type",
			factoryName: "test-factory",
			index:       0,
			input: FactoryInput{
				Name: "test-input",
			},
			wantErr: true,
			errMsg:  "type is required",
		},
		{
			name:        "invalid type",
			factoryName: "test-factory",
			index:       0,
			input: FactoryInput{
				Name: "test-input",
				Type: "invalid",
			},
			wantErr: true,
			errMsg:  "invalid type",
		},
		{
			name:        "enum without options",
			factoryName: "test-factory",
			index:       0,
			input: FactoryInput{
				Name: "test-input",
				Type: "enum",
			},
			wantErr: true,
			errMsg:  "enum type requires options",
		},
		{
			name:        "invalid regex pattern",
			factoryName: "test-factory",
			index:       0,
			input: FactoryInput{
				Name:       "test-input",
				Type:       "string",
				Validation: "[invalid(regex",
			},
			wantErr: true,
			errMsg:  "invalid validation regex",
		},
		{
			name:        "valid regex pattern",
			factoryName: "test-factory",
			index:       0,
			input: FactoryInput{
				Name:       "test-input",
				Type:       "string",
				Validation: "^[a-z]+$",
			},
			wantErr: false,
		},
		{
			name:        "number with min > max",
			factoryName: "test-factory",
			index:       0,
			input: FactoryInput{
				Name: "test-input",
				Type: "number",
				Min:  floatPtr(100),
				Max:  floatPtr(10),
			},
			wantErr: true,
			errMsg:  "min cannot be greater than max",
		},
		{
			name:        "number with valid min/max",
			factoryName: "test-factory",
			index:       0,
			input: FactoryInput{
				Name: "test-input",
				Type: "number",
				Min:  floatPtr(0),
				Max:  floatPtr(100),
			},
			wantErr: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := validateFactoryInput(tt.factoryName, tt.index, tt.input)
			if (err != nil) != tt.wantErr {
				t.Errorf("validateFactoryInput() error = %v, wantErr %v", err, tt.wantErr)
				return
			}
			if err != nil && tt.errMsg != "" {
				if !contains(err.Error(), tt.errMsg) {
					t.Errorf("validateFactoryInput() error message = %v, should contain %v", err.Error(), tt.errMsg)
				}
			}
		})
	}
}

func TestTemplateConfigValidate(t *testing.T) {
	tests := []struct {
		name     string
		template *TemplateConfig
		wantErr  bool
		errMsg   string
	}{
		{
			name: "valid replicated template",
			template: &TemplateConfig{
				Provider: "replicated",
			},
			wantErr: false,
		},
		{
			name: "valid daytona template",
			template: &TemplateConfig{
				Provider: "daytona",
			},
			wantErr: false,
		},
		{
			name:     "nil template",
			template: nil,
			wantErr:  true,
			errMsg:   "template config is nil",
		},
		{
			name: "missing provider",
			template: &TemplateConfig{
				Provider: "",
			},
			wantErr: true,
			errMsg:  "provider is required",
		},
		{
			name: "invalid provider",
			template: &TemplateConfig{
				Provider: "invalid",
			},
			wantErr: true,
			errMsg:  "invalid provider",
		},
		{
			name: "invalid color",
			template: &TemplateConfig{
				Provider: "replicated",
				Color:    "invalid-color",
			},
			wantErr: true,
			errMsg:  "invalid color",
		},
		{
			name: "valid color",
			template: &TemplateConfig{
				Provider: "replicated",
				Color:    "blue",
			},
			wantErr: false,
		},
		{
			name: "valid github repos",
			template: &TemplateConfig{
				Provider: "replicated",
				GitHub: &GitHubTemplateConfig{
					Repos: []GitHubRepoAccess{
						{Repo: "owner/repo", Permissions: "read"},
						{Repo: "owner/repo2", Permissions: "write"},
					},
				},
			},
			wantErr: false,
		},
		{
			name: "github repo missing repo field",
			template: &TemplateConfig{
				Provider: "replicated",
				GitHub: &GitHubTemplateConfig{
					Repos: []GitHubRepoAccess{
						{Repo: "", Permissions: "read"},
					},
				},
			},
			wantErr: true,
			errMsg:  "repo is required",
		},
		{
			name: "github repo invalid format",
			template: &TemplateConfig{
				Provider: "replicated",
				GitHub: &GitHubTemplateConfig{
					Repos: []GitHubRepoAccess{
						{Repo: "invalid-format", Permissions: "read"},
					},
				},
			},
			wantErr: true,
			errMsg:  "invalid repo format",
		},
		{
			name: "github repo invalid permissions",
			template: &TemplateConfig{
				Provider: "replicated",
				GitHub: &GitHubTemplateConfig{
					Repos: []GitHubRepoAccess{
						{Repo: "owner/repo", Permissions: "admin"},
					},
				},
			},
			wantErr: true,
			errMsg:  "invalid permissions",
		},
		{
			name: "valid MCPs",
			template: &TemplateConfig{
				Provider: "replicated",
				MCPs: []MCPRef{
					{Name: "github"},
					{Name: "postgres"},
				},
			},
			wantErr: false,
		},
		{
			name: "MCP missing name",
			template: &TemplateConfig{
				Provider: "replicated",
				MCPs: []MCPRef{
					{Name: ""},
				},
			},
			wantErr: true,
			errMsg:  "name is required",
		},
		{
			name: "valid secret refs",
			template: &TemplateConfig{
				Provider: "replicated",
				Secrets: SecretRefList{
					{Type: "linear"},
					{Type: "custom", Name: "my-secret"},
				},
			},
			wantErr: false,
		},
		{
			name: "secret ref missing type",
			template: &TemplateConfig{
				Provider: "replicated",
				Secrets: SecretRefList{
					{Type: ""},
				},
			},
			wantErr: true,
			errMsg:  "type is required",
		},
		{
			name: "secret ref invalid type",
			template: &TemplateConfig{
				Provider: "replicated",
				Secrets: SecretRefList{
					{Type: "invalid"},
				},
			},
			wantErr: true,
			errMsg:  "invalid type",
		},
		{
			name: "custom secret ref missing name",
			template: &TemplateConfig{
				Provider: "replicated",
				Secrets: SecretRefList{
					{Type: "custom"},
				},
			},
			wantErr: true,
			errMsg:  "name is required for custom type",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := tt.template.Validate()
			if (err != nil) != tt.wantErr {
				t.Errorf("Validate() error = %v, wantErr %v", err, tt.wantErr)
				return
			}
			if err != nil && tt.errMsg != "" {
				if !contains(err.Error(), tt.errMsg) {
					t.Errorf("Validate() error message = %v, should contain %v", err.Error(), tt.errMsg)
				}
			}
		})
	}
}

func TestValidateMCPServerConfig(t *testing.T) {
	tests := []struct {
		name    string
		mcp     *MCPServerHubConfig
		wantErr bool
		errMsg  string
	}{
		{
			name: "valid npx MCP",
			mcp: &MCPServerHubConfig{
				Name:    "github",
				Source:  MCPSourceNpx,
				Package: "@modelcontextprotocol/server-github",
			},
			wantErr: false,
		},
		{
			name: "valid docker MCP",
			mcp: &MCPServerHubConfig{
				Name:   "postgres",
				Source: MCPSourceDocker,
				Image:  "mcp/postgres",
			},
			wantErr: false,
		},
		{
			name: "valid sse MCP",
			mcp: &MCPServerHubConfig{
				Name:   "remote",
				Source: MCPSourceSSE,
				URL:    "http://localhost:3000/sse",
			},
			wantErr: false,
		},
		{
			name:    "nil MCP",
			mcp:     nil,
			wantErr: true,
			errMsg:  "MCP server config is nil",
		},
		{
			name: "missing name",
			mcp: &MCPServerHubConfig{
				Source:  MCPSourceNpx,
				Package: "@modelcontextprotocol/server-github",
			},
			wantErr: true,
			errMsg:  "name is required",
		},
		{
			name: "missing source",
			mcp: &MCPServerHubConfig{
				Name:    "github",
				Package: "@modelcontextprotocol/server-github",
			},
			wantErr: true,
			errMsg:  "source is required",
		},
		{
			name: "invalid source",
			mcp: &MCPServerHubConfig{
				Name:   "github",
				Source: "invalid",
			},
			wantErr: true,
			errMsg:  "invalid source",
		},
		{
			name: "npx missing package",
			mcp: &MCPServerHubConfig{
				Name:   "github",
				Source: MCPSourceNpx,
			},
			wantErr: true,
			errMsg:  "package is required",
		},
		{
			name: "docker missing image",
			mcp: &MCPServerHubConfig{
				Name:   "postgres",
				Source: MCPSourceDocker,
			},
			wantErr: true,
			errMsg:  "image is required",
		},
		{
			name: "sse missing url",
			mcp: &MCPServerHubConfig{
				Name:   "remote",
				Source: MCPSourceSSE,
			},
			wantErr: true,
			errMsg:  "url is required",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := ValidateMCPServerConfig(tt.mcp)
			if (err != nil) != tt.wantErr {
				t.Errorf("ValidateMCPServerConfig() error = %v, wantErr %v", err, tt.wantErr)
				return
			}
			if err != nil && tt.errMsg != "" {
				if !contains(err.Error(), tt.errMsg) {
					t.Errorf("ValidateMCPServerConfig() error message = %v, should contain %v", err.Error(), tt.errMsg)
				}
			}
		})
	}
}

func TestValidateProviderConfig(t *testing.T) {
	tests := []struct {
		name    string
		cfgName string
		cfg     *ProviderConfig
		wantErr bool
		errMsg  string
	}{
		{
			name:    "valid replicated",
			cfgName: "replicated",
			cfg: &ProviderConfig{
				Type: "replicated",
			},
			wantErr: false,
		},
		{
			name:    "valid daytona",
			cfgName: "daytona",
			cfg: &ProviderConfig{
				Type: "daytona",
			},
			wantErr: false,
		},
		{
			name:    "nil config",
			cfgName: "test",
			cfg:     nil,
			wantErr: true,
			errMsg:  "config is nil",
		},
		{
			name:    "missing type with empty name",
			cfgName: "",
			cfg: &ProviderConfig{
				Type: "",
			},
			wantErr: true,
			errMsg:  "provider type is required",
		},
		{
			name:    "invalid type",
			cfgName: "test",
			cfg: &ProviderConfig{
				Type: "invalid",
			},
			wantErr: true,
			errMsg:  "invalid provider type",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := ValidateProviderConfig(tt.cfgName, tt.cfg)
			if (err != nil) != tt.wantErr {
				t.Errorf("ValidateProviderConfig() error = %v, wantErr %v", err, tt.wantErr)
				return
			}
			if err != nil && tt.errMsg != "" {
				if !contains(err.Error(), tt.errMsg) {
					t.Errorf("ValidateProviderConfig() error message = %v, should contain %v", err.Error(), tt.errMsg)
				}
			}
		})
	}
}

func TestHubConfigValidate(t *testing.T) {
	tests := []struct {
		name    string
		hub     *HubConfig
		wantErr bool
		errMsg  string
	}{
		{
			name: "valid empty hub config",
			hub: &HubConfig{
				URL:   "http://localhost:8080",
				Token: "test-token",
			},
			wantErr: false,
		},
		{
			name:    "nil hub config",
			hub:     nil,
			wantErr: true,
			errMsg:  "hub config is nil",
		},
		{
			name: "valid hub with factories",
			hub: &HubConfig{
				URL:   "http://localhost:8080",
				Token: "test-token",
				Factories: []*FactoryConfig{
					{
						Name:        "factory1",
						Integration: "linear",
						Template:    "base",
					},
				},
			},
			wantErr: false,
		},
		{
			name: "hub with invalid factory",
			hub: &HubConfig{
				URL:   "http://localhost:8080",
				Token: "test-token",
				Factories: []*FactoryConfig{
					{
						Name:        "",
						Integration: "linear",
						Template:    "base",
					},
				},
			},
			wantErr: true,
			errMsg:  "factory name is required",
		},
		{
			name: "hub with valid MCP servers",
			hub: &HubConfig{
				URL:   "http://localhost:8080",
				Token: "test-token",
				MCPServers: []*MCPServerHubConfig{
					{
						Name:    "github",
						Source:  MCPSourceNpx,
						Package: "@modelcontextprotocol/server-github",
					},
				},
			},
			wantErr: false,
		},
		{
			name: "hub with invalid MCP server",
			hub: &HubConfig{
				URL:   "http://localhost:8080",
				Token: "test-token",
				MCPServers: []*MCPServerHubConfig{
					{
						Name:   "github",
						Source: "",
					},
				},
			},
			wantErr: true,
			errMsg:  "source is required",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := tt.hub.Validate()
			if (err != nil) != tt.wantErr {
				t.Errorf("Validate() error = %v, wantErr %v", err, tt.wantErr)
				return
			}
			if err != nil && tt.errMsg != "" {
				if !contains(err.Error(), tt.errMsg) {
					t.Errorf("Validate() error message = %v, should contain %v", err.Error(), tt.errMsg)
				}
			}
		})
	}
}

func TestValidateCronWorkflowTrigger(t *testing.T) {
	tests := []struct {
		name    string
		trigger *CronWorkflowTrigger
		wantErr bool
		errMsg  string
	}{
		{
			name:    "valid schedule",
			trigger: &CronWorkflowTrigger{Schedule: "0 9 * * 1"},
			wantErr: false,
		},
		{
			name:    "valid schedule with timezone",
			trigger: &CronWorkflowTrigger{Schedule: "0 9 * * 1", Timezone: "America/Chicago"},
			wantErr: false,
		},
		{
			name:    "missing schedule",
			trigger: &CronWorkflowTrigger{},
			wantErr: true,
			errMsg:  "requires schedule",
		},
		{
			name:    "invalid schedule expression",
			trigger: &CronWorkflowTrigger{Schedule: "not-a-cron"},
			wantErr: true,
			errMsg:  `invalid cron schedule "not-a-cron"`,
		},
		{
			name:    "invalid timezone",
			trigger: &CronWorkflowTrigger{Schedule: "0 9 * * 1", Timezone: "Mars/Phobos"},
			wantErr: true,
			errMsg:  `invalid cron timezone "Mars/Phobos"`,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := validateCronWorkflowTrigger("wf", tt.trigger)
			if (err != nil) != tt.wantErr {
				t.Fatalf("validateCronWorkflowTrigger() error = %v, wantErr %v", err, tt.wantErr)
			}
			if err != nil && tt.errMsg != "" && !contains(err.Error(), tt.errMsg) {
				t.Errorf("validateCronWorkflowTrigger() error = %q, should contain %q", err.Error(), tt.errMsg)
			}
		})
	}
}

// TestWorkflowConfigValidateRejectsBadCron exercises the save-time path
// (WorkflowConfig.Validate) that the hub API uses before persisting configs.
func TestWorkflowConfigValidateRejectsBadCron(t *testing.T) {
	wf := &WorkflowConfig{
		Name:    "nightly",
		Trigger: &WorkflowTrigger{Cron: &CronWorkflowTrigger{Schedule: "bogus"}},
	}
	err := wf.Validate()
	if err == nil {
		t.Fatal("expected Validate() to reject invalid cron schedule")
	}
	if !contains(err.Error(), `invalid cron schedule "bogus"`) {
		t.Errorf("Validate() error = %q, should mention the offending schedule", err.Error())
	}

	wf.Trigger.Cron.Schedule = "0 9 * * 1"
	if err := wf.Validate(); err != nil {
		t.Fatalf("expected valid cron workflow to pass, got %v", err)
	}
}

// Helper functions
func floatPtr(f float64) *float64 {
	return &f
}

func contains(s, substr string) bool {
	return strings.Contains(s, substr)
}
