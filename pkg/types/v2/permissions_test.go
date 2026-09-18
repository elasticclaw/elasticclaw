package v2_test

import (
	"strings"
	"testing"

	v2 "github.com/elasticclaw/elasticclaw/pkg/types/v2"
)

func parseRepositoryPermissions(t *testing.T, doc string) v2.RepositoryPermissions {
	t.Helper()
	resolved, err := v2.ParseAndValidateWorkspace([]byte(doc))
	if err != nil {
		t.Fatalf("ParseAndValidateWorkspace: %v", err)
	}
	repo, ok := resolved.Workspace.Repositories["primary"]
	if !ok {
		t.Fatal("missing repository primary")
	}
	return repo.Permissions
}

func TestRepositoryPermissionsScalarForm(t *testing.T) {
	perms := parseRepositoryPermissions(t, `
schema_version: 2
name: x
repositories:
  primary:
    provider: github
    repository: org/repo
    permissions: write
`)
	if perms.Level() != "write" {
		t.Fatalf("level = %q, want write", perms.Level())
	}
	if perms.Granular() != nil {
		t.Fatalf("granular = %v, want nil for scalar form", perms.Granular())
	}
	if perms.IsZero() {
		t.Fatal("scalar write must not be zero")
	}
}

func TestRepositoryPermissionsDefaultsToRead(t *testing.T) {
	perms := parseRepositoryPermissions(t, `
schema_version: 2
name: x
repositories:
  primary:
    provider: github
    repository: org/repo
`)
	if perms.Level() != "read" {
		t.Fatalf("level = %q, want read", perms.Level())
	}
	if !perms.IsZero() {
		t.Fatal("omitted permissions must be zero")
	}
}

func TestRepositoryPermissionsGranularForm(t *testing.T) {
	perms := parseRepositoryPermissions(t, `
schema_version: 2
name: x
repositories:
  primary:
    provider: github
    repository: org/repo
    permissions:
      contents: write
      dependabot_alerts: read
      security_events: read
`)
	if perms.Level() != "write" {
		t.Fatalf("level = %q, want write derived from contents", perms.Level())
	}
	granular := perms.Granular()
	if granular == nil {
		t.Fatal("expected granular map")
	}
	// dependabot_alerts is an alias for the canonical vulnerability_alerts.
	if _, ok := granular["dependabot_alerts"]; ok {
		t.Fatalf("granular = %v, want alias canonicalized", granular)
	}
	if granular["vulnerability_alerts"] != "read" {
		t.Fatalf("vulnerability_alerts = %q, want read", granular["vulnerability_alerts"])
	}
	if granular["security_events"] != "read" {
		t.Fatalf("security_events = %q, want read", granular["security_events"])
	}
	if granular["contents"] != "write" {
		t.Fatalf("contents = %q, want write", granular["contents"])
	}
}

func TestRepositoryPermissionsGranularDefaultsToReadLevel(t *testing.T) {
	// Granular form without a contents entry keeps the historical default
	// base level (read) — extras only add.
	perms := parseRepositoryPermissions(t, `
schema_version: 2
name: x
repositories:
  primary:
    provider: github
    repository: org/repo
    permissions:
      vulnerability_alerts: read
`)
	if perms.Level() != "read" {
		t.Fatalf("level = %q, want read", perms.Level())
	}
}

func TestWorkspaceRejectsUnknownGranularPermission(t *testing.T) {
	_, err := v2.ParseAndValidateWorkspace([]byte(`
schema_version: 2
name: x
repositories:
  primary:
    provider: github
    repository: org/repo
    permissions:
      nuclear_launch_codes: read
`))
	if err == nil || !strings.Contains(err.Error(), "unknown GitHub App permission") {
		t.Fatalf("error = %v, want unknown GitHub App permission", err)
	}
}

func TestWorkspaceRejectsInvalidGranularLevel(t *testing.T) {
	_, err := v2.ParseAndValidateWorkspace([]byte(`
schema_version: 2
name: x
repositories:
  primary:
    provider: github
    repository: org/repo
    permissions:
      vulnerability_alerts: admin
`))
	if err == nil || !strings.Contains(err.Error(), "invalid level") {
		t.Fatalf("error = %v, want invalid level", err)
	}
}

func TestWorkspaceRejectsEmptyGranularPermissions(t *testing.T) {
	_, err := v2.ParseAndValidateWorkspace([]byte(`
schema_version: 2
name: x
repositories:
  primary:
    provider: github
    repository: org/repo
    permissions: {}
`))
	if err == nil || !strings.Contains(err.Error(), "at least one permission") {
		t.Fatalf("error = %v, want at least one permission", err)
	}
}

func TestWorkspaceScalarPermissionsStayLenient(t *testing.T) {
	// Historical behavior: scalar values other than read/write are normalized
	// to read at projection and must not start failing validation (issue #697
	// rollout must not break existing configs).
	perms := parseRepositoryPermissions(t, `
schema_version: 2
name: x
repositories:
  primary:
    provider: github
    repository: org/repo
    permissions: banana
`)
	if perms.Level() != "read" {
		t.Fatalf("level = %q, want read for unrecognized scalar", perms.Level())
	}
}
