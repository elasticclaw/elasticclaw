package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestLoad(t *testing.T) {
	for _, tc := range []struct {
		name, yaml         string
		missing, wantError bool
		valid, invalid     int
	}{
		{name: "missing", missing: true, wantError: true},
		{name: "empty", wantError: true},
		{name: "syntax", yaml: "about: [", wantError: true},
		{name: "scalar", yaml: "hello", wantError: true},
		{name: "null", yaml: "null", wantError: true},
		{name: "multiple documents", yaml: "about: one\n---\nabout: two", wantError: true},
		{name: "wrong about type", yaml: "about: {}", wantError: true},
		{name: "duplicate key", yaml: "about: one\nabout: two", wantError: true},
		{name: "unknown mode", yaml: "modes: {custom: {prompt: hello}}", wantError: true},
		{name: "invalid mode", yaml: "modes: {explore_idea: []}", wantError: true},
		{name: "zero retention", yaml: "retention: {chats_days: 0}", wantError: true},
		{name: "negative retention", yaml: "retention: {tool_results_days: -1}", wantError: true},
		{name: "bad retention type", yaml: "retention: {artifact_versions_days: nope}", wantError: true},
		{name: "defaults", yaml: "about: Product assistant"},
		{name: "reserved fields", yaml: "conventions: {}\ntemplate: []\nskill: anything"},
		{name: "llm entry name", yaml: "llm: Product assistant"},
		{name: "blank llm entry name", yaml: "llm: '  '", wantError: true},
		{name: "KB", yaml: "knowledge_base: {provider: github, repo: org/docs, entry: docs/index.md, write: {}, save: []}", valid: 1},
		{name: "wrong KB provider", yaml: "knowledge_base: {provider: notion}", invalid: 1},
		{name: "KB traversal", yaml: "knowledge_base: {provider: github, repo: org/docs, entry: ../secret}", invalid: 1},
		{name: "KB absolute", yaml: "knowledge_base: {provider: github, repo: org/docs, entry: /README.md}", invalid: 1},
		{name: "KB missing entry", yaml: "knowledge_base: {provider: github, repo: org/docs}", invalid: 1},
		{name: "malformed source", yaml: "knowledge_base: []", invalid: 1},
		{name: "repositories", yaml: "repositories: [org/repo, invalid, ../private, {repo: org/repo2}]", valid: 1, invalid: 3},
		{name: "invalid repositories list", yaml: "repositories: org/repo", invalid: 1},
		{name: "PostHog", yaml: "posthog: {host: https://us.posthog.com, project_id: 123, api_key: product/posthog}", valid: 1},
		{name: "PostHog credentials in URL", yaml: "posthog: {host: 'https://user:password@example.com', project_id: 123, api_key: KEY}", invalid: 1},
		{name: "PostHog missing secret ref", yaml: "posthog: {host: https://us.posthog.com, project_id: 123}", invalid: 1},
		{name: "PostHog non-http URL", yaml: "posthog: {host: 'file:///tmp/file', project_id: 123, api_key: KEY}", invalid: 1},
		{name: "Datadog", yaml: "datadog: {site: us5.datadoghq.com, env: prod, api_key: 123-api, app_key: app}", valid: 1},
		{name: "Datadog invalid site", yaml: "datadog: {site: bad..host, env: prod, api_key: api, app_key: app}", invalid: 1},
		{name: "Datadog missing scope", yaml: "datadog: {site: datadoghq.com, api_key: api, app_key: app}", invalid: 1},
		{name: "Linear", yaml: "issue_tracker: {provider: linear, default_fields: {team: PRODUCT, labels: [product]}}", valid: 1},
		{name: "wrong tracker", yaml: "issue_tracker: {provider: jira, default_fields: {team: PRODUCT}}", invalid: 1},
		{name: "tracker missing team", yaml: "issue_tracker: {provider: linear}", invalid: 1},
		{name: "valid source survives malformed sibling", yaml: "repositories: [org/repo]\nposthog: [invalid]", valid: 1, invalid: 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			if !tc.missing {
				if err := os.WriteFile(filepath.Join(dir, "ai_chat.yaml"), []byte(tc.yaml), 0600); err != nil {
					t.Fatal(err)
				}
			}
			cfg, err := Load(dir)
			if (err != nil) != tc.wantError {
				t.Fatalf("Load error = %v, want error %v", err, tc.wantError)
			}
			if tc.wantError {
				return
			}
			valid, invalid := 0, 0
			for _, source := range cfg.Sources {
				switch source.Status {
				case "unchecked":
					valid++
				case "invalid":
					invalid++
					if source.Error == "" {
						t.Error("invalid source has no error")
					}
				default:
					t.Errorf("unexpected status %q", source.Status)
				}
			}
			if valid != tc.valid || invalid != tc.invalid {
				t.Fatalf("sources = %#v", cfg.Sources)
			}
			retained := len(cfg.Repositories)
			if cfg.KnowledgeBase != nil {
				retained++
			}
			if cfg.PostHog != nil {
				retained++
			}
			if cfg.Datadog != nil {
				retained++
			}
			if cfg.IssueTracker != nil {
				retained++
			}
			if retained != valid {
				t.Fatalf("invalid source retained: %#v", cfg)
			}
			if cfg.Retention != (Retention{90, 30, 30}) {
				t.Fatalf("defaults = %#v", cfg.Retention)
			}
		})
	}
}

func TestLoadSampleAndReload(t *testing.T) {
	cfg, err := Load("testdata")
	if err != nil {
		t.Fatal(err)
	}
	if len(cfg.Sources) != 5 {
		t.Fatalf("sources = %#v", cfg.Sources)
	}
	for _, source := range cfg.Sources {
		if source.Status != "unchecked" {
			t.Fatalf("sample source = %#v", source)
		}
	}
	if cfg.Modes["ask_the_data"].Prompt == "" || cfg.LLM != "product-assistant" || cfg.PostHog.APIKey != "POSTHOG_READ_KEY" {
		t.Fatalf("sample not loaded: %#v", cfg)
	}
	dir := t.TempDir()
	for _, about := range []string{"first", "second"} {
		if err := os.WriteFile(filepath.Join(dir, "ai_chat.yaml"), []byte("about: "+about+"\nretention: {chats_days: 7}"), 0600); err != nil {
			t.Fatal(err)
		}
		got, err := Load(dir)
		if err != nil || got.About != about || got.Retention != (Retention{7, 30, 30}) {
			t.Fatalf("reload = %#v, %v", got, err)
		}
	}
}

func TestSourceErrorsDoNotExposeValues(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "ai_chat.yaml"), []byte("posthog: {api_key: {value: super-secret}}"), 0600); err != nil {
		t.Fatal(err)
	}
	cfg, err := Load(dir)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.PostHog != nil || len(cfg.Sources) != 1 || strings.Contains(cfg.Sources[0].Error, "super-secret") {
		t.Fatalf("unsafe source: %#v", cfg)
	}
}
