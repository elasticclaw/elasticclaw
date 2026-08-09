package workflowv2

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"time"

	typesv2 "github.com/elasticclaw/elasticclaw/pkg/types/v2"
	"github.com/google/uuid"
)

type KnowledgeResolver interface {
	ResolveKnowledge(context.Context, Run, string, typesv2.KnowledgeSource) (typesv2.ContextBundleSource, error)
}

type KnowledgeResolverFunc func(context.Context, Run, string, typesv2.KnowledgeSource) (typesv2.ContextBundleSource, error)

func (f KnowledgeResolverFunc) ResolveKnowledge(ctx context.Context, run Run, name string,
	source typesv2.KnowledgeSource) (typesv2.ContextBundleSource, error) {
	return f(ctx, run, name, source)
}

// AssembleContext resolves workspace-owned knowledge sources into a versioned
// bundle. Repository authority remains bounded by the pinned workspace; the
// caller may only narrow repository_files sources to named workspace repos.
func (s *Store) AssembleContext(ctx context.Context, runID string, relevantRepositories []string,
	resolver KnowledgeResolver) (typesv2.ContextBundle, error) {
	return s.assembleContext(ctx, runID, relevantRepositories, "", resolver)
}

// AssembleOrganizationContext builds the mandatory pre-plan layer without
// prematurely resolving repository-scoped sources. Repository context is
// expanded later, after planning has selected names from the pinned workspace.
func (s *Store) AssembleOrganizationContext(ctx context.Context, runID string,
	resolver KnowledgeResolver) (typesv2.ContextBundle, error) {
	return s.assembleContext(ctx, runID, nil, typesv2.KnowledgeScopeOrganization, resolver)
}

func (s *Store) assembleContext(ctx context.Context, runID string, relevantRepositories []string,
	scope string, resolver KnowledgeResolver) (typesv2.ContextBundle, error) {
	if s == nil || s.db == nil {
		return typesv2.ContextBundle{}, fmt.Errorf("workflow v2 store is not configured")
	}
	if resolver == nil {
		return typesv2.ContextBundle{}, fmt.Errorf("knowledge resolver is required")
	}
	run, err := s.GetRun(ctx, runID)
	if err != nil {
		return typesv2.ContextBundle{}, err
	}
	var workspaceYAML string
	if err := s.db.QueryRowContext(ctx, `SELECT workspace_yaml FROM workflow_v2_runs WHERE id=?`, runID).Scan(&workspaceYAML); err != nil {
		return typesv2.ContextBundle{}, err
	}
	workspace, err := typesv2.ParseAndValidateWorkspace([]byte(workspaceYAML))
	if err != nil {
		return typesv2.ContextBundle{}, fmt.Errorf("load pinned workspace: %w", err)
	}
	relevant := make([]string, 0, len(relevantRepositories))
	seen := map[string]bool{}
	for _, name := range relevantRepositories {
		name = strings.TrimSpace(name)
		if name == "" || seen[name] {
			continue
		}
		if !workspace.Workspace.HasRepository(name) {
			return typesv2.ContextBundle{}, fmt.Errorf("relevant repository %q is not in pinned workspace", name)
		}
		seen[name] = true
		relevant = append(relevant, name)
	}
	sort.Strings(relevant)

	bundle := typesv2.ContextBundle{ID: uuid.NewString(), RunID: runID, Sources: []typesv2.ContextBundleSource{}, CreatedAt: s.now().UTC()}
	nowMillis := bundle.CreatedAt.UnixMilli()
	if _, err := s.db.ExecContext(ctx, `INSERT INTO workflow_v2_context_bundles(id,run_id,revision,status,sources_json,created_at,updated_at)
		VALUES(?,?,?,'assembling','[]',?,?)`, bundle.ID, runID, "assembling:"+bundle.ID, nowMillis, nowMillis); err != nil {
		return typesv2.ContextBundle{}, err
	}
	defer func() {
		_, _ = s.db.ExecContext(context.Background(), `DELETE FROM workflow_v2_context_bundles WHERE id=? AND run_id=? AND status='assembling'`,
			bundle.ID, runID)
	}()

	status := "ready"
	if workspace.Workspace.Knowledge != nil {
		names := make([]string, 0, len(workspace.Workspace.Knowledge.Sources))
		for name := range workspace.Workspace.Knowledge.Sources {
			names = append(names, name)
		}
		sort.Strings(names)
		for _, name := range names {
			source := workspace.Workspace.Knowledge.Sources[name]
			if scope != "" && source.Scope != scope {
				continue
			}
			if source.Type == typesv2.KnowledgeTypeRepositoryFiles && len(source.Repositories) == 0 {
				source.Repositories = append([]string(nil), relevant...)
			}
			resolved, resolveErr := resolver.ResolveKnowledge(ctx, run, name, source)
			resolved.Name = name
			resolved.Type = source.Type
			resolved.Scope = source.Scope
			resolved.Required = source.Required
			resolved.Repositories = append([]string(nil), source.Repositories...)
			if resolveErr != nil {
				resolved.Status = "failed"
				resolved.Error = resolveErr.Error()
			} else if strings.TrimSpace(resolved.Status) == "" {
				resolved.Status = "ready"
			}
			if resolved.Status != "ready" && resolved.Status != "failed" {
				return typesv2.ContextBundle{}, fmt.Errorf("knowledge resolver returned unsupported status %q for source %q",
					resolved.Status, name)
			}
			if resolved.Status == "ready" && strings.TrimSpace(resolved.SourceRevision) == "" &&
				strings.TrimSpace(resolved.ContentDigest) == "" {
				return typesv2.ContextBundle{}, fmt.Errorf("knowledge source %q is ready without a revision or content digest", name)
			}
			if resolved.Status == "failed" && strings.TrimSpace(resolved.Error) == "" {
				return typesv2.ContextBundle{}, fmt.Errorf("knowledge source %q failed without an error", name)
			}
			if source.Required && resolved.Status != "ready" {
				status = "failed"
			}
			bundle.Sources = append(bundle.Sources, resolved)
		}
	}
	bundle.Revision, err = contextRevision(bundle.Sources)
	if err != nil {
		return typesv2.ContextBundle{}, err
	}
	sourcesJSON, err := json.Marshal(bundle.Sources)
	if err != nil {
		return typesv2.ContextBundle{}, err
	}
	var existingID, existingStatus, existingSources string
	var existingCreated int64
	existingErr := s.db.QueryRowContext(ctx, `SELECT id,status,sources_json,created_at FROM workflow_v2_context_bundles
		WHERE run_id=? AND revision=? AND id!=?`, runID, bundle.Revision, bundle.ID).Scan(
		&existingID, &existingStatus, &existingSources, &existingCreated)
	if existingErr != nil && existingErr != sql.ErrNoRows {
		return typesv2.ContextBundle{}, existingErr
	}
	if existingID != "" {
		if _, err := s.db.ExecContext(ctx, `DELETE FROM workflow_v2_context_bundles WHERE id=? AND run_id=? AND status='assembling'`,
			bundle.ID, runID); err != nil {
			return typesv2.ContextBundle{}, err
		}
		bundle.ID = existingID
		bundle.CreatedAt = time.UnixMilli(existingCreated).UTC()
		if err := json.Unmarshal([]byte(existingSources), &bundle.Sources); err != nil {
			return typesv2.ContextBundle{}, err
		}
		status = existingStatus
	} else {
		result, err := s.db.ExecContext(ctx, `UPDATE workflow_v2_context_bundles SET revision=?,status=?,sources_json=?,updated_at=?
			WHERE id=? AND run_id=? AND status='assembling'`, bundle.Revision, status, string(sourcesJSON),
			s.now().UTC().UnixMilli(), bundle.ID, runID)
		if err != nil {
			return typesv2.ContextBundle{}, err
		}
		if changed, err := result.RowsAffected(); err != nil || changed != 1 {
			return typesv2.ContextBundle{}, fmt.Errorf("context bundle changed while assembling")
		}
	}
	if _, err := s.db.ExecContext(ctx, `UPDATE workflow_v2_runs SET context_bundle_id=?,updated_at=? WHERE id=?`,
		bundle.ID, s.now().UTC().UnixMilli(), runID); err != nil {
		return typesv2.ContextBundle{}, err
	}

	eventKind := "context.bundle.ready"
	if status == "failed" {
		eventKind = "context.bundle.failed"
	}
	eventResult, err := s.ApplyEvent(ctx, runID, EventInput{
		ID: uuid.NewString(), Kind: eventKind, Producer: ProducerContext,
		Payload: map[string]interface{}{"context": map[string]interface{}{
			"status": status, "bundle_id": bundle.ID, "revision": bundle.Revision,
		}},
		Facts: map[string]interface{}{
			"context.status": status, "context.bundle_id": bundle.ID, "context.revision": bundle.Revision,
		},
		Provenance: typesv2.EvidenceProvenance{Producer: string(ProducerContext), ObservedAt: s.now().UTC()},
	})
	if err != nil {
		return typesv2.ContextBundle{}, err
	}
	if eventResult.Disposition != typesv2.DispositionAccepted {
		return typesv2.ContextBundle{}, fmt.Errorf("context event was %s: %s", eventResult.Disposition, eventResult.Reason)
	}
	if status == "failed" && eventResult.Transition == nil &&
		(eventResult.Run.Status == RunActive || eventResult.Run.Status == RunSuspended) {
		reason := "one or more required knowledge sources failed"
		if _, err := s.db.ExecContext(ctx, `UPDATE workflow_v2_runs SET status='suspended',waiting_reason=?,updated_at=?
			WHERE id=? AND state_version=? AND status IN ('active','suspended')`, reason, s.now().UTC().UnixMilli(), runID, eventResult.Run.StateVersion); err != nil {
			return typesv2.ContextBundle{}, err
		}
	}
	return bundle, nil
}

func contextRevision(sources []typesv2.ContextBundleSource) (string, error) {
	raw, err := json.Marshal(sources)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(raw)
	return "sha256:" + hex.EncodeToString(sum[:]), nil
}
