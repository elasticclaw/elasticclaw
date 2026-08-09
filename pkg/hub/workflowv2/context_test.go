package workflowv2_test

import (
	"context"
	"errors"
	"reflect"
	"testing"

	workflowv2 "github.com/elasticclaw/elasticclaw/pkg/hub/workflowv2"
	typesv2 "github.com/elasticclaw/elasticclaw/pkg/types/v2"
)

const contextWorkspaceYAML = `
schema_version: 2
name: engineering
repositories:
  api:
    provider: github
    repository: org/api
  web:
    provider: github
    repository: org/web
knowledge:
  sources:
    principles:
      type: workspace_files
      scope: organization
      required: true
      paths: [ENGINEERING.md, PRODUCT.md]
    repository-guidance:
      type: repository_files
      scope: repository
      paths: [AGENTS.md, README.md]
`

const contextWorkflowYAML = `
schema_version: 2
name: context-first
enabled: true
initial_state: gathering
states:
  gathering:
    phase: context
  planning:
    phase: plan
transitions:
  context_ready:
    from: gathering
    on: context.bundle.ready
    when:
      context:
        status:
          equals: ready
    to: planning
`

func createContextRun(t *testing.T, store *workflowv2.Store, id string) {
	t.Helper()
	if _, err := store.CreateRun(context.Background(), workflowv2.CreateRunRequest{
		ID: id, TenantID: "tenant-1", WorkspaceYAML: []byte(contextWorkspaceYAML), WorkflowYAML: []byte(contextWorkflowYAML),
	}); err != nil {
		t.Fatal(err)
	}
}

func TestAssembleContextUsesPinnedWorkspaceAndRelevantRepositories(t *testing.T) {
	db := openRuntimeDB(t)
	store := workflowv2.NewStore(db)
	createContextRun(t, store, "run-context")
	seen := map[string]typesv2.KnowledgeSource{}
	resolver := workflowv2.KnowledgeResolverFunc(func(_ context.Context, _ workflowv2.Run, name string,
		source typesv2.KnowledgeSource) (typesv2.ContextBundleSource, error) {
		seen[name] = source
		return typesv2.ContextBundleSource{Status: "ready", SourceRevision: "git:abc", ContentDigest: "sha256:123",
			Documents: append([]string(nil), source.Paths...)}, nil
	})
	bundle, err := store.AssembleContext(context.Background(), "run-context", []string{"web"}, resolver)
	if err != nil {
		t.Fatal(err)
	}
	if len(bundle.Sources) != 2 || bundle.Revision == "" {
		t.Fatalf("bundle = %#v", bundle)
	}
	if got := bundle.Sources[1].Repositories; !reflect.DeepEqual(got, []string{"web"}) {
		t.Fatalf("bundle repository scope = %#v", got)
	}
	if got := seen["repository-guidance"].Repositories; !reflect.DeepEqual(got, []string{"web"}) {
		t.Fatalf("resolved repositories = %#v", got)
	}
	run, err := store.GetRun(context.Background(), "run-context")
	if err != nil {
		t.Fatal(err)
	}
	if run.ContextBundleID != bundle.ID || run.State != "planning" || run.StateVersion != 2 {
		t.Fatalf("run = %#v", run)
	}
	inspection, err := store.InspectRun(context.Background(), "run-context")
	if err != nil {
		t.Fatal(err)
	}
	contextFacts, ok := inspection.Facts["context"].(map[string]interface{})
	if !ok || contextFacts["status"] != "ready" || contextFacts["revision"] != bundle.Revision {
		t.Fatalf("context facts = %#v", inspection.Facts["context"])
	}
	repeated, err := store.AssembleContext(context.Background(), "run-context", []string{"web"}, resolver)
	if err != nil {
		t.Fatal(err)
	}
	if repeated.ID != bundle.ID || repeated.Revision != bundle.Revision {
		t.Fatalf("repeated bundle = %#v, want id/revision %s/%s", repeated, bundle.ID, bundle.Revision)
	}
	var bundleCount int
	if err := db.QueryRow(`SELECT COUNT(*) FROM workflow_v2_context_bundles WHERE run_id='run-context'`).Scan(&bundleCount); err != nil {
		t.Fatal(err)
	}
	if bundleCount != 1 {
		t.Fatalf("context bundle count = %d, want 1", bundleCount)
	}
}

func TestAssembleContextRejectsRepositoryOutsideWorkspace(t *testing.T) {
	db := openRuntimeDB(t)
	store := workflowv2.NewStore(db)
	createContextRun(t, store, "run-context-authority")
	_, err := store.AssembleContext(context.Background(), "run-context-authority", []string{"unknown"},
		workflowv2.KnowledgeResolverFunc(func(context.Context, workflowv2.Run, string, typesv2.KnowledgeSource) (typesv2.ContextBundleSource, error) {
			return typesv2.ContextBundleSource{}, nil
		}))
	if err == nil {
		t.Fatal("repository outside pinned workspace was accepted")
	}
	var bundles int
	if err := db.QueryRow(`SELECT COUNT(*) FROM workflow_v2_context_bundles WHERE run_id='run-context-authority'`).Scan(&bundles); err != nil {
		t.Fatal(err)
	}
	if bundles != 0 {
		t.Fatalf("invalid assembly persisted %d bundles", bundles)
	}
}

func TestRequiredKnowledgeFailureSuspendsWithoutExplicitRecoveryEdge(t *testing.T) {
	db := openRuntimeDB(t)
	store := workflowv2.NewStore(db)
	createContextRun(t, store, "run-context-failure")
	bundle, err := store.AssembleContext(context.Background(), "run-context-failure", []string{"api"},
		workflowv2.KnowledgeResolverFunc(func(_ context.Context, _ workflowv2.Run, name string,
			source typesv2.KnowledgeSource) (typesv2.ContextBundleSource, error) {
			if name == "principles" {
				return typesv2.ContextBundleSource{}, errors.New("knowledge service unavailable")
			}
			return typesv2.ContextBundleSource{Status: "ready", ContentDigest: "sha256:repository"}, nil
		}))
	if err != nil {
		t.Fatal(err)
	}
	if len(bundle.Sources) != 2 || bundle.Sources[0].Status != "failed" {
		t.Fatalf("bundle = %#v", bundle)
	}
	run, err := store.GetRun(context.Background(), "run-context-failure")
	if err != nil {
		t.Fatal(err)
	}
	if run.Status != workflowv2.RunSuspended || run.State != "gathering" || run.WaitingReason == "" {
		t.Fatalf("run = %#v", run)
	}
}

func TestRequiredKnowledgeFailedStatusSuspendsRun(t *testing.T) {
	db := openRuntimeDB(t)
	store := workflowv2.NewStore(db)
	createContextRun(t, store, "run-context-failed-status")
	_, err := store.AssembleContext(context.Background(), "run-context-failed-status", []string{"api"},
		workflowv2.KnowledgeResolverFunc(func(_ context.Context, _ workflowv2.Run, name string,
			_ typesv2.KnowledgeSource) (typesv2.ContextBundleSource, error) {
			if name == "principles" {
				return typesv2.ContextBundleSource{Status: "failed", Error: "source unavailable"}, nil
			}
			return typesv2.ContextBundleSource{Status: "ready", ContentDigest: "sha256:repository"}, nil
		}))
	if err != nil {
		t.Fatal(err)
	}
	run, err := store.GetRun(context.Background(), "run-context-failed-status")
	if err != nil {
		t.Fatal(err)
	}
	if run.Status != workflowv2.RunSuspended {
		t.Fatalf("run = %#v", run)
	}
}

func TestInvalidKnowledgeStatusCleansProvisionalBundle(t *testing.T) {
	db := openRuntimeDB(t)
	store := workflowv2.NewStore(db)
	createContextRun(t, store, "run-context-invalid-status")
	_, err := store.AssembleContext(context.Background(), "run-context-invalid-status", []string{"api"},
		workflowv2.KnowledgeResolverFunc(func(context.Context, workflowv2.Run, string,
			typesv2.KnowledgeSource) (typesv2.ContextBundleSource, error) {
			return typesv2.ContextBundleSource{Status: "partial"}, nil
		}))
	if err == nil {
		t.Fatal("unsupported resolver status was accepted")
	}
	var bundles int
	if err := db.QueryRow(`SELECT COUNT(*) FROM workflow_v2_context_bundles WHERE run_id='run-context-invalid-status'`).Scan(&bundles); err != nil {
		t.Fatal(err)
	}
	if bundles != 0 {
		t.Fatalf("provisional bundles = %d, want 0", bundles)
	}
}
