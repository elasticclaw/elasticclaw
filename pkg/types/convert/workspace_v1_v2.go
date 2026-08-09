package convert

import (
	"fmt"
	"regexp"
	"sort"
	"strings"

	v2 "github.com/elasticclaw/elasticclaw/pkg/types/v2"
	"gopkg.in/yaml.v3"
)

// v1WorkspaceRaw captures authored v1 workspace fields, including keys that
// are not all present on types.WorkspaceConfig (provider/nix/docker).
type v1WorkspaceRaw struct {
	SchemaVersion  interface{}           `yaml:"schema_version"`
	Name           string                `yaml:"name"`
	Provider       string                `yaml:"provider"`
	Nix            *bool                 `yaml:"nix"`
	Docker         *bool                 `yaml:"docker"`
	Repositories   []v1RepoEntry         `yaml:"repositories"`
	Env            map[string]v1EnvEntry `yaml:"env"`
	Secrets        []string              `yaml:"secrets"`
	WebhookSecrets []string              `yaml:"webhook_secrets"`
	SecretRefs     map[string]string     `yaml:"secret_refs"`
}

// v1RepoEntry accepts list form {repo, permissions} or will be filled from scalars.
type v1RepoEntry struct {
	Repo        string `yaml:"repo"`
	Permissions string `yaml:"permissions"`
	// When unmarshaling shorthand list items, Repo is set via custom logic below.
	raw string
}

type v1EnvEntry struct {
	Value  string `yaml:"value"`
	Secret string `yaml:"secret"`
	// Inline scalar form is handled by UnmarshalYAML.
	inline string
}

func (e *v1EnvEntry) UnmarshalYAML(value *yaml.Node) error {
	if value == nil {
		return nil
	}
	if value.Kind == yaml.ScalarNode {
		e.inline = value.Value
		e.Value = value.Value
		return nil
	}
	type plain v1EnvEntry
	var p plain
	if err := value.Decode(&p); err != nil {
		return err
	}
	*e = v1EnvEntry(p)
	return nil
}

func convertWorkspaceV1ToV2(data []byte, opts Options) (Result, error) {
	var warnings []string

	// Parse repositories with flexible list items (string or object).
	var probe map[string]interface{}
	if err := yaml.Unmarshal(data, &probe); err != nil {
		return Result{}, fmt.Errorf("parse workspace: %w", err)
	}

	var raw v1WorkspaceRaw
	if err := yaml.Unmarshal(data, &raw); err != nil {
		return Result{}, fmt.Errorf("parse workspace: %w", err)
	}
	// Re-parse repositories from probe for shorthand strings.
	repos, repoWarns := parseV1Repositories(probe["repositories"])
	warnings = append(warnings, repoWarns...)

	name := strings.TrimSpace(raw.Name)
	if name == "" {
		return Result{}, fmt.Errorf("workspace name is required")
	}

	out := v2.Workspace{
		SchemaVersion: 2,
		Name:          name,
		Repositories:  map[string]v2.Repository{},
		Credentials:   map[string]v2.Credential{},
	}

	// Source-control connection: one default github connection when any repo present.
	const defaultSC = "github-default"
	if len(repos) > 0 {
		out.SourceControl = &v2.SourceControlBlock{
			Connections: map[string]v2.Connection{
				defaultSC: {
					Provider: "github",
				},
			},
		}
		appendWarning(&warnings, "source_control.connections.%s: created as a placeholder; set credentials to a GitHub app/token secret ref", defaultSC)
	}

	// Repositories → named map entries.
	usedNames := map[string]int{}
	for _, r := range repos {
		repo := strings.TrimSpace(r.Repo)
		if repo == "" {
			continue
		}
		if strings.Contains(repo, "*") {
			appendWarning(&warnings, "repositories %q: glob patterns are not expressible as a v2 named repository; skipped — configure repositories explicitly", repo)
			continue
		}
		key := repoResourceName(repo, usedNames)
		out.Repositories[key] = v2.Repository{
			Provider:      "github",
			Repository:    repo,
			SourceControl: defaultSC,
		}
		if perms := strings.TrimSpace(r.Permissions); perms != "" && perms != "read" {
			appendWarning(&warnings, "repositories.%s: v1 permissions %q are not modeled on workspace v2 repositories (access is connection/credential scoped)", key, perms)
		}
	}

	// Execution from provider/nix/docker.
	if raw.Provider != "" || raw.Nix != nil || raw.Docker != nil {
		exec := &v2.Execution{Provider: strings.TrimSpace(raw.Provider)}
		if raw.Nix != nil {
			exec.Nix = *raw.Nix
		}
		if raw.Docker != nil {
			exec.Docker = *raw.Docker
		}
		out.Execution = exec
	}

	// Env secret refs → credentials; inline values cannot live in v2 workspace schema.
	for envName, entry := range raw.Env {
		secret := strings.TrimSpace(entry.Secret)
		if secret != "" {
			credName := credentialName(secret, envName)
			if _, exists := out.Credentials[credName]; !exists {
				out.Credentials[credName] = v2.Credential{Secret: secret}
			}
			appendWarning(&warnings, "env.%s: secret ref mapped to credentials.%s (secret: %s); runtime env wiring is not part of workspace v2 schema and must be configured separately if still required", envName, credName, secret)
			continue
		}
		if strings.TrimSpace(entry.Value) != "" || strings.TrimSpace(entry.inline) != "" {
			appendWarning(&warnings, "env.%s: inline value dropped — workspace v2 does not embed environment values; configure via execution/runtime outside the workspace document if needed", envName)
		}
	}

	// Top-level secrets / secret_refs / webhook_secrets.
	for _, s := range raw.Secrets {
		s = strings.TrimSpace(s)
		if s == "" {
			continue
		}
		credName := credentialName(s, s)
		if _, exists := out.Credentials[credName]; !exists {
			out.Credentials[credName] = v2.Credential{Secret: s}
		}
	}
	for envName, secretName := range raw.SecretRefs {
		secretName = strings.TrimSpace(secretName)
		if secretName == "" {
			continue
		}
		credName := credentialName(secretName, envName)
		if _, exists := out.Credentials[credName]; !exists {
			out.Credentials[credName] = v2.Credential{Secret: secretName}
		}
		appendWarning(&warnings, "secret_refs.%s: mapped to credentials.%s", envName, credName)
	}
	if len(raw.WebhookSecrets) > 0 {
		appendWarning(&warnings, "webhook_secrets: not represented in workspace v2 schema yet; configure webhook authentication on the hub separately (%d ref(s) dropped)", len(raw.WebhookSecrets))
	}

	// Empty CI / issue trackers / review blocks are omitted; warn that CI must be added manually.
	appendWarning(&warnings, "ci.connections/pipelines: not inferred from v1 — add named CI connections and pipelines manually (GitHub Actions, Depot, Jenkins, …)")
	appendWarning(&warnings, "issue_trackers/review_systems: not inferred from v1 — add connections when you need Linear/GitHub review evidence")

	// Attach credentials to default source_control if we have an obvious github cred.
	if out.SourceControl != nil {
		if cred := pickGitHubCredential(out.Credentials); cred != "" {
			c := out.SourceControl.Connections[defaultSC]
			c.Credentials = cred
			out.SourceControl.Connections[defaultSC] = c
		} else {
			appendWarning(&warnings, "source_control.connections.%s.credentials: unset — add a credentials.* entry and reference it", defaultSC)
		}
	}

	// Drop empty maps for cleaner YAML.
	if len(out.Credentials) == 0 {
		out.Credentials = nil
	}
	if len(out.Repositories) == 0 {
		out.Repositories = nil
		out.SourceControl = nil
	}

	if shouldValidate(opts) {
		// Re-marshal then parse+validate through the real v2 path.
		tmp, err := yaml.Marshal(out)
		if err != nil {
			return Result{}, err
		}
		if _, err := v2.ParseAndValidateWorkspace(tmp); err != nil {
			return Result{}, fmt.Errorf("converted workspace failed v2 validation: %w", err)
		}
	}

	encoded, err := yaml.Marshal(out)
	if err != nil {
		return Result{}, fmt.Errorf("marshal workspace v2: %w", err)
	}
	if len(encoded) == 0 || encoded[len(encoded)-1] != '\n' {
		encoded = append(encoded, '\n')
	}

	// Stable warning order for tests.
	sort.Strings(warnings)

	return Result{
		Output:   encoded,
		From:     "v1",
		To:       "2",
		Warnings: warnings,
	}, nil
}

func parseV1Repositories(raw interface{}) ([]v1RepoEntry, []string) {
	var warnings []string
	list, ok := raw.([]interface{})
	if !ok || len(list) == 0 {
		return nil, nil
	}
	var out []v1RepoEntry
	for i, item := range list {
		switch v := item.(type) {
		case string:
			out = append(out, v1RepoEntry{Repo: strings.TrimSpace(v), Permissions: "read"})
		case map[string]interface{}:
			repo, _ := v["repo"].(string)
			perms, _ := v["permissions"].(string)
			if perms == "" {
				perms = "read"
			}
			out = append(out, v1RepoEntry{Repo: strings.TrimSpace(repo), Permissions: perms})
		default:
			warnings = append(warnings, fmt.Sprintf("repositories[%d]: unrecognized entry, skipped", i))
		}
	}
	return out, warnings
}

var nonResourceChars = regexp.MustCompile(`[^a-zA-Z0-9_.-]+`)

func repoResourceName(repo string, used map[string]int) string {
	base := strings.ToLower(repo)
	base = strings.ReplaceAll(base, "/", "-")
	base = nonResourceChars.ReplaceAllString(base, "-")
	base = strings.Trim(base, "-.")
	if base == "" {
		base = "repo"
	}
	if !regexp.MustCompile(`^[A-Za-z0-9]`).MatchString(base) {
		base = "r-" + base
	}
	n := used[base]
	used[base] = n + 1
	if n == 0 {
		return base
	}
	return fmt.Sprintf("%s-%d", base, n+1)
}

func credentialName(secret, fallback string) string {
	s := strings.TrimSpace(secret)
	if s == "" {
		s = fallback
	}
	s = strings.ToLower(s)
	s = nonResourceChars.ReplaceAllString(s, "_")
	s = strings.Trim(s, "_")
	if s == "" {
		return "credential"
	}
	// resource names allow [A-Za-z0-9][A-Za-z0-9_.-]*
	if !regexp.MustCompile(`^[A-Za-z0-9]`).MatchString(s) {
		s = "c_" + s
	}
	return s
}

func pickGitHubCredential(creds map[string]v2.Credential) string {
	// Prefer names that look like GitHub app keys.
	preferred := []string{"github_app", "github_app_private_key", "github_token", "gh_token"}
	for _, p := range preferred {
		if _, ok := creds[p]; ok {
			return p
		}
	}
	for name, c := range creds {
		sec := strings.ToUpper(c.Secret)
		if strings.Contains(sec, "GITHUB") || strings.Contains(strings.ToUpper(name), "GITHUB") || strings.Contains(strings.ToUpper(name), "GH_") {
			return name
		}
	}
	return ""
}
