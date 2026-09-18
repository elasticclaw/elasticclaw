package v2_test

import (
	"encoding/json"
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

func TestWorkspaceRejectsCaseVariantDuplicatePermissions(t *testing.T) {
	// Case-only variants (Contents vs contents) normalize to the same key at
	// decode time and must be rejected instead of silently overwriting.
	_, err := v2.ParseAndValidateWorkspace([]byte(`
schema_version: 2
name: x
repositories:
  primary:
    provider: github
    repository: org/repo
    permissions:
      Contents: write
      contents: read
`))
	if err == nil || !strings.Contains(err.Error(), "duplicate declaration") {
		t.Fatalf("error = %v, want duplicate declaration", err)
	}
}

func TestWorkspaceRejectsAliasDuplicatePermissions(t *testing.T) {
	// dependabot_alerts and vulnerability_alerts are distinct authored keys
	// that canonicalize to the same permission; validation must reject them.
	_, err := v2.ParseAndValidateWorkspace([]byte(`
schema_version: 2
name: x
repositories:
  primary:
    provider: github
    repository: org/repo
    permissions:
      dependabot_alerts: read
      vulnerability_alerts: read
`))
	if err == nil || !strings.Contains(err.Error(), "duplicate declaration of vulnerability_alerts") {
		t.Fatalf("error = %v, want duplicate declaration of vulnerability_alerts", err)
	}
}

func TestWorkspaceRejectsMetadataWrite(t *testing.T) {
	// metadata is always read and cannot be widened; reject instead of
	// silently dropping the requested level at mint time.
	_, err := v2.ParseAndValidateWorkspace([]byte(`
schema_version: 2
name: x
repositories:
  primary:
    provider: github
    repository: org/repo
    permissions:
      metadata: write
`))
	if err == nil || !strings.Contains(err.Error(), "metadata is always read") {
		t.Fatalf("error = %v, want metadata is always read", err)
	}

	// Case variants normalize to the same level and must be rejected too.
	_, err = v2.ParseAndValidateWorkspace([]byte(`
schema_version: 2
name: x
repositories:
  primary:
    provider: github
    repository: org/repo
    permissions:
      metadata: WRITE
`))
	if err == nil || !strings.Contains(err.Error(), "metadata is always read") {
		t.Fatalf("uppercase variant: error = %v, want metadata is always read", err)
	}
}

func TestRepositoryPermissionsJSONRejectsDuplicateKeys(t *testing.T) {
	// Exact duplicates are collapsed by a plain map unmarshal; the decoder
	// must walk authored key order and reject them instead.
	cases := []struct {
		name string
		data string
	}{
		{"exact duplicate", `{"contents":"read","contents":"write"}`},
		{"case variant", `{"Contents":"read","contents":"write"}`},
		{"alias pair", `{"dependabot_alerts":"read","vulnerability_alerts":"read"}`},
	}
	for _, tc := range cases {
		var perms v2.RepositoryPermissions
		if err := json.Unmarshal([]byte(tc.data), &perms); err == nil || !strings.Contains(err.Error(), "duplicate declaration") {
			t.Fatalf("%s: error = %v, want duplicate declaration", tc.name, err)
		}
	}
}

func TestRepositoryPermissionsJSONRejectsTrailingData(t *testing.T) {
	// encoding/json's scanner rejects trailing data at the top level before
	// the custom unmarshaler runs ("invalid character ... after top-level
	// value"); the unmarshaler additionally guards its own token stream.
	var perms v2.RepositoryPermissions
	err := json.Unmarshal([]byte(`{"contents":"read"} trailing`), &perms)
	if err == nil {
		t.Fatal("expected trailing data to be rejected")
	}
}

func TestRepositoryPermissionsJSONRoundTripOmitsZero(t *testing.T) {
	// A repository without permissions must not serialize an explicit
	// permissions field (no "permissions": null).
	type doc struct {
		Repo v2.Repository `json:"repo"`
	}
	out, err := json.Marshal(doc{Repo: v2.Repository{Provider: "github", Repository: "org/repo"}})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(out), "permissions") {
		t.Fatalf("zero permissions must be omitted, got %s", out)
	}

	out, err = json.Marshal(doc{Repo: v2.Repository{Provider: "github", Repository: "org/repo", Permissions: v2.PermissionsFromLevel("write")}})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(out), `"permissions":"write"`) {
		t.Fatalf("scalar form must serialize as a string, got %s", out)
	}

	out, err = json.Marshal(doc{Repo: v2.Repository{Provider: "github", Repository: "org/repo", Permissions: v2.PermissionsFromMap(map[string]string{"vulnerability_alerts": "read"})}})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(out), `"vulnerability_alerts":"read"`) {
		t.Fatalf("granular form must serialize as a map, got %s", out)
	}
}

func TestRepositoryPermissionsConstructors(t *testing.T) {
	scalar := v2.PermissionsFromLevel(" Write ")
	if scalar.Level() != "write" || scalar.Granular() != nil {
		t.Fatalf("PermissionsFromLevel = level %q granular %v", scalar.Level(), scalar.Granular())
	}
	granular := v2.PermissionsFromMap(map[string]string{
		"Dependabot_Alerts": "read",
		"security_events":   " Read ",
	})
	got := granular.Granular()
	if got["vulnerability_alerts"] != "read" || got["security_events"] != "read" {
		t.Fatalf("PermissionsFromMap canonicalization = %v", got)
	}
	if _, ok := got["dependabot_alerts"]; ok {
		t.Fatalf("PermissionsFromMap must canonicalize keys, got %v", got)
	}
	if v2.PermissionsFromMap(nil).IsZero() != true {
		t.Fatal("PermissionsFromMap(nil) must be zero")
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
