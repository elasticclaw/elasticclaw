package hub

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"sort"
	"strings"
	"time"

	"github.com/elasticclaw/elasticclaw/pkg/hub/workflowv2"
	"github.com/elasticclaw/elasticclaw/pkg/types"
	typesv2 "github.com/elasticclaw/elasticclaw/pkg/types/v2"
	"github.com/google/uuid"
)

// triggerWorkflowV2Config is an authenticated operator run-creation adapter.
// It deliberately does not translate the request or any conversation text
// into workflow transitions. Trigger inputs are only staged as claw context;
// the durable state machine still advances exclusively through typed events.
func (s *Server) triggerWorkflowV2Config(w http.ResponseWriter, r *http.Request,
	workspace *types.WorkspaceConfig, workflow *types.WorkflowConfig) {
	if workflow.Enabled == nil || !*workflow.Enabled {
		jsonError(w, http.StatusForbidden, "workflow is disabled")
		return
	}
	if !workflow.EnableManualTrigger {
		jsonError(w, http.StatusForbidden, "workflow does not support manual triggers")
		return
	}
	var req FactoryTriggerRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil && !errors.Is(err, io.EOF) {
		jsonError(w, http.StatusBadRequest, "invalid JSON: "+err.Error())
		return
	}
	inputs := make(map[string]string, len(req.Inputs))
	for name, value := range req.Inputs {
		name = strings.TrimSpace(name)
		if name == "" {
			jsonError(w, http.StatusBadRequest, "input names cannot be empty")
			return
		}
		raw, err := json.Marshal(value)
		if err != nil {
			jsonError(w, http.StatusBadRequest, "invalid input "+name+": "+err.Error())
			return
		}
		if text, ok := value.(string); ok {
			inputs[name] = text
		} else {
			inputs[name] = string(raw)
		}
	}

	workspaceYAML := []byte(workspace.Files["elasticclaw-config.yaml"])
	workflowYAML := []byte(workflow.RawConfig)
	runID := uuid.NewString()
	clawID, _, err := s.createClawFromWorkflowWithOptions(workspace, workflow, workflowCreateOptions{
		ctx: r.Context(), inputs: inputs, reason: "manual workflow v2 trigger",
		beforeProvision: func(ctx context.Context, clawID, tenantID string) error {
			store := workflowv2.NewStore(s.db)
			run, err := store.CreateRun(ctx, workflowv2.CreateRunRequest{
				ID: runID, TenantID: tenantID, InitialClawID: clawID,
				WorkspaceYAML: workspaceYAML, WorkflowYAML: workflowYAML, ActivationPending: true,
				TriggerType: "manual",
			})
			if err != nil {
				return err
			}
			_, err = store.AssembleOrganizationContext(ctx, run.ID, workspaceFileKnowledgeResolver(workspace.Files))
			if err == nil {
				err = store.CompleteActivation(ctx, run.ID)
			}
			if err == nil {
				return nil
			}
			cleanupCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			if cleanupErr := store.CancelActivation(cleanupCtx, run.ID, err.Error()); cleanupErr != nil {
				return errors.Join(err, fmt.Errorf("cancel failed workflow v2 activation: %w", cleanupErr))
			}
			return err
		},
	})
	if err != nil {
		jsonError(w, http.StatusInternalServerError, "failed to create workflow v2 run: "+err.Error())
		return
	}
	if startCommand := v2WorkflowStartCommand(workflow.RawConfig); startCommand != "" {
		store := workflowv2.NewStore(s.db)
		if _, err := store.ApplyCommand(r.Context(), runID, startCommand, workflowv2.CommandInput{
			ID:        uuid.NewString(),
			MessageID: runID + "/manual-start",
			Reason:    "manual workflow v2 trigger",
			Provenance: typesv2.EvidenceProvenance{
				Producer:   string(workflowv2.ProducerOperator),
				ObservedAt: time.Now().UTC(),
			},
		}); err != nil {
			log.Printf("[workflow-v2] manual trigger start command for run %s: %v", runID, err)
		}
	}
	jsonOK(w, map[string]string{"run_id": runID, "claw_id": clawID, "status": "created"})
}

func v2WorkflowStartCommand(rawConfig string) string {
	resolved, err := typesv2.ParseAndValidateWorkflow([]byte(rawConfig))
	if err != nil {
		return ""
	}
	wf := resolved.Workflow
	cmd, ok := wf.Commands["start"]
	if !ok {
		return ""
	}
	fromStates, err := typesv2.FromStates(cmd.From)
	if err != nil {
		return ""
	}
	for _, state := range fromStates {
		if state == wf.InitialState {
			return "start"
		}
	}
	return ""
}

func workspaceFileKnowledgeResolver(files map[string]string) workflowv2.KnowledgeResolver {
	return workflowv2.KnowledgeResolverFunc(func(_ context.Context, run workflowv2.Run, name string,
		source typesv2.KnowledgeSource) (typesv2.ContextBundleSource, error) {
		if source.Type != typesv2.KnowledgeTypeWorkspaceFiles {
			return typesv2.ContextBundleSource{}, fmt.Errorf(
				"organization knowledge source %q uses unsupported runtime type %q", name, source.Type)
		}
		paths := append([]string(nil), source.Paths...)
		sort.Strings(paths)
		documents := make([]string, 0, len(paths))
		hash := sha256.New()
		for _, path := range paths {
			path = strings.TrimSpace(path)
			content, found := files[path]
			if !found {
				return typesv2.ContextBundleSource{}, fmt.Errorf("workspace knowledge file %q is missing", path)
			}
			document := path + "\n" + content
			documents = append(documents, document)
			_, _ = hash.Write([]byte(document))
			_, _ = hash.Write([]byte{0})
		}
		return typesv2.ContextBundleSource{
			Status: "ready", SourceRevision: "workspace:" + run.WorkspaceRevision,
			ContentDigest: fmt.Sprintf("sha256:%x", hash.Sum(nil)), Documents: documents,
		}, nil
	})
}
