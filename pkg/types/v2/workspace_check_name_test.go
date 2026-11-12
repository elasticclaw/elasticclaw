package v2

import (
	"strings"
	"testing"
)

func TestWorkspaceCheckNamePipeline(t *testing.T) {
	for _, tc := range []struct {
		name, fields string
		valid        bool
	}{
		{"glob", "check_name: depot-*", true},
		{"app", "check_name: depot-*\n      app_slug: depot", true},
		{"workflow", "workflow: ci.yml", true},
		{"both", "check_name: depot-*\n      workflow: ci.yml", false},
		{"invalid glob", "check_name: '[broken'", false},
		{"blank", "check_name: ' '", false},
		{"app without check", "app_slug: depot", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := ParseAndValidateWorkspace([]byte(strings.ReplaceAll(`schema_version: 2
name: checks
repositories:
  api: {provider: github, repository: org/api}
ci:
  connections:
    github: {provider: github}
  pipelines:
    depot:
      connection: github
      repository: api
      FIELDS
`, "FIELDS", tc.fields)))
			if (err == nil) != tc.valid {
				t.Fatalf("valid=%v, error=%v", tc.valid, err)
			}
		})
	}
}

func TestMatchCheckNameSpansSlash(t *testing.T) {
	for _, tc := range []struct {
		pattern, name string
		want          bool
	}{
		{"*", "CI / unit", true},
		{"*unit*", "CI / unit", true},
		{"CI*", "CI / unit", true},
		{"CI / ?nit", "CI / unit", true},
		{"build / test (*)", "build / test (ubuntu)", true},
		{"depot-*", "CI / unit", false},
	} {
		got, err := MatchCheckName(tc.pattern, tc.name)
		if err != nil || got != tc.want {
			t.Fatalf("MatchCheckName(%q, %q) = %v, %v; want %v", tc.pattern, tc.name, got, err, tc.want)
		}
	}
}
