package v2_test

import (
	"strings"
	"testing"

	v2 "github.com/elasticclaw/elasticclaw/pkg/types/v2"
)

const validWorkspaceYAML = `
schema_version: 2
name: elasticclaw

repositories:
  primary:
    provider: github
    repository: elasticclaw/elasticclaw
    source_control: github-production
    checkout:
      ref: default
      depth: full

execution:
  provider: daytona-production
  nix: true
  docker: true
  tools:
    - git
    - gh
    - depot

credentials:
  github_app:
    secret: GITHUB_APP_PRIVATE_KEY
  depot_token:
    secret: DEPOT_TOKEN
  jenkins_token:
    secret: JENKINS_TOKEN
  linear_api_key:
    secret: LINEAR_API_KEY

source_control:
  connections:
    github-production:
      provider: github
      credentials: github_app

ci:
  connections:
    github-production:
      provider: github_actions
      source_control: github-production
      credentials: github_app
      capability_restrictions:
        trigger_run: false
        cancel_run: false

    depot-production:
      provider: depot
      credentials: depot_token

    corporate-jenkins:
      provider: jenkins
      base_url: https://jenkins.example.com
      credentials: jenkins_token

  pipelines:
    github-pr:
      connection: github-production
      repository: primary
      workflow: ci.yml

    depot-container:
      connection: depot-production
      repository: primary
      project: elasticclaw
      pipeline: container-build

    jenkins-release:
      connection: corporate-jenkins
      repository: primary
      job: release-validation

issue_trackers:
  connections:
    product-linear:
      provider: linear
      credentials: linear_api_key

review_systems:
  connections:
    github-reviews:
      provider: github
      source_control: github-production
    greptile:
      provider: greptile
      source_control: github-production
`

func TestParseAndValidateWorkspaceValidRFCShape(t *testing.T) {
	resolved, err := v2.ParseAndValidateWorkspace([]byte(validWorkspaceYAML))
	if err != nil {
		t.Fatalf("ParseAndValidateWorkspace: %v", err)
	}
	if resolved.Workspace.Name != "elasticclaw" {
		t.Fatalf("name = %q", resolved.Workspace.Name)
	}
	if resolved.Revision == "" {
		t.Fatal("expected non-empty revision digest")
	}
	// capability restrictions narrow trigger_run
	caps := resolved.ResolvedCICaps["github-production"]
	if caps[v2.CapTriggerRun] {
		t.Fatal("expected trigger_run to be restricted false")
	}
	if !caps[v2.CapObserveRuns] {
		t.Fatal("expected observe_runs to remain true")
	}
	if !resolved.Workspace.HasCIPipeline("github-pr") {
		t.Fatal("expected github-pr pipeline")
	}
}

func TestWorkspaceRevisionStableAcrossCalls(t *testing.T) {
	a, err := v2.ParseAndValidateWorkspace([]byte(validWorkspaceYAML))
	if err != nil {
		t.Fatal(err)
	}
	b, err := v2.ParseAndValidateWorkspace([]byte(validWorkspaceYAML))
	if err != nil {
		t.Fatal(err)
	}
	if a.Revision == "" || a.Revision != b.Revision {
		t.Fatalf("revisions not stable: %q vs %q", a.Revision, b.Revision)
	}
}

func TestWorkspaceRejectsUnknownField(t *testing.T) {
	yaml := `
schema_version: 2
name: x
unknown_top_level: true
`
	_, err := v2.ParseAndValidateWorkspace([]byte(yaml))
	if err == nil || !strings.Contains(err.Error(), "unknown field") {
		t.Fatalf("error = %v, want unknown field", err)
	}
}

func TestWorkspaceRejectsUnknownPipelineConnection(t *testing.T) {
	yaml := `
schema_version: 2
name: x
repositories:
  primary:
    provider: github
    repository: org/repo
credentials:
  tok:
    secret: TOKEN
ci:
  connections:
    c1:
      provider: github_actions
      credentials: tok
  pipelines:
    p1:
      connection: missing-conn
      repository: primary
`
	_, err := v2.ParseAndValidateWorkspace([]byte(yaml))
	if err == nil || !strings.Contains(err.Error(), "unknown ci connection") {
		t.Fatalf("error = %v, want unknown ci connection", err)
	}
}

func TestWorkspaceRejectsMissingCredential(t *testing.T) {
	yaml := `
schema_version: 2
name: x
ci:
  connections:
    c1:
      provider: github_actions
      credentials: missing_cred
`
	_, err := v2.ParseAndValidateWorkspace([]byte(yaml))
	if err == nil || !strings.Contains(err.Error(), "unknown credential") {
		t.Fatalf("error = %v, want unknown credential", err)
	}
}

func TestWorkspaceRejectsCapabilityGrantBeyondProvider(t *testing.T) {
	// greptile provider only has observe_runs + reconcile; granting trigger_run invents a capability.
	yaml := `
schema_version: 2
name: x
source_control:
  connections:
    sc:
      provider: github
review_systems:
  connections:
    greptile:
      provider: greptile
      source_control: sc
      capability_restrictions:
        trigger_run: true
`
	_, err := v2.ParseAndValidateWorkspace([]byte(yaml))
	if err == nil || !strings.Contains(err.Error(), "cannot grant capability") {
		t.Fatalf("error = %v, want cannot grant capability", err)
	}
}

func TestWorkspaceRejectsNonV2(t *testing.T) {
	yaml := `
schema_version: v1
name: x
`
	_, err := v2.ParseAndValidateWorkspace([]byte(yaml))
	if err == nil || !strings.Contains(err.Error(), "not v2") {
		t.Fatalf("error = %v, want not v2", err)
	}
}

func TestIsV2AcceptsIntegerAndV2Forms(t *testing.T) {
	if !v2.IsV2("2") || !v2.IsV2("v2") || !v2.IsV2("V2") {
		t.Fatal("expected 2/v2 accepted")
	}
	if v2.IsV2("v1") || v2.IsV2("") || v2.IsV2("1") {
		t.Fatal("expected v1/empty/1 rejected as v2")
	}
	ver, err := v2.DetectSchemaVersion([]byte("schema_version: 2\nname: x\n"))
	if err != nil || ver != "2" {
		t.Fatalf("DetectSchemaVersion = %q, %v", ver, err)
	}
}
