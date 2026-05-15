package hub

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"

	"github.com/elasticclaw/elasticclaw/pkg/types"
)

// handleFactoriesCRUD handles:
//
//	GET  /api/factories        → list all factories (secrets masked)
//	POST /api/factories        → upsert batch from factory push
//	DELETE /api/factories?name=<name> → remove a factory
func (s *Server) handleFactoriesCRUD(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		s.handleFactoriesList(w, r)
	case http.MethodPost:
		s.handleFactoriesPush(w, r)
	case http.MethodDelete:
		name := r.URL.Query().Get("name")
		if name == "" {
			http.Error(w, "name required", http.StatusBadRequest)
			return
		}
		s.handleFactoryDelete(w, r, name)
	default:
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	}
}

// FactoryPushView is the JSON-safe view of a factory returned by push/list API (secrets masked).
type FactoryPushView struct {
	Name                string              `json:"name"`
	Integration         string              `json:"integration"`
	Workspace           string              `json:"workspace"`
	TriggerStatus       string              `json:"trigger_status"`
	DoneStatus          string              `json:"done_status,omitempty"`
	Template            string              `json:"template"`
	Labels              []string            `json:"labels,omitempty"`
	AssignedTo          string              `json:"assigned_to,omitempty"`
	Enabled             *bool               `json:"enabled,omitempty"`
	HasWebhookSecret    bool                `json:"has_webhook_secret"`
	WebhookSecretRef    string              `json:"webhook_secret_ref,omitempty"`
	PipelineYAML        string              `json:"pipeline_yaml,omitempty"`
	EnableManualTrigger bool                `json:"enable_manual_trigger,omitempty"`
	SecretRefs          map[string]string    `json:"secret_refs,omitempty"`
	Inputs              []types.FactoryInput `json:"inputs,omitempty"`
}

func factoryToPushView(f *types.FactoryConfig) FactoryPushView {
	return FactoryPushView{
		Name:                f.Name,
		Integration:         f.Integration,
		Workspace:           f.Workspace,
		TriggerStatus:       f.TriggerStatus,
		DoneStatus:          f.DoneStatus,
		Template:            f.Template,
		Labels:              f.Labels,
		AssignedTo:          f.AssignedTo,
		Enabled:             f.Enabled,
		HasWebhookSecret:    f.WebhookSecret != "" || f.WebhookSecretRef != "",
		WebhookSecretRef:    f.WebhookSecretRef,
		PipelineYAML:        f.PipelineYAML,
		EnableManualTrigger: f.EnableManualTrigger,
		SecretRefs:          f.SecretRefs,
		Inputs:              f.Inputs,
	}
}

func (s *Server) handleFactoriesList(w http.ResponseWriter, r *http.Request) {
	nameFilter := strings.TrimSpace(r.URL.Query().Get("name"))

	factories, err := loadExternalFactories()
	if err != nil {
		http.Error(w, "fs error: "+err.Error(), http.StatusInternalServerError)
		return
	}

	views := make([]FactoryPushView, 0, len(factories))
	for _, f := range factories {
		if f == nil {
			continue
		}
		if nameFilter != "" && !strings.EqualFold(f.Name, nameFilter) {
			continue
		}
		views = append(views, factoryToPushView(f))
	}
	jsonOK(w, views)
}

// FactoryPushRequest is the payload for POST /api/factories.
type FactoryPushRequest struct {
	Factories []*types.FactoryConfig `json:"factories"`
}

func (s *Server) handleFactoriesPush(w http.ResponseWriter, r *http.Request) {
	var req FactoryPushRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "invalid JSON: "+err.Error(), http.StatusBadRequest)
		return
	}
	if len(req.Factories) == 0 {
		http.Error(w, "no factories provided", http.StatusBadRequest)
		return
	}

	// Validate all factories before saving
	for _, f := range req.Factories {
		if f == nil {
			http.Error(w, "factory cannot be nil", http.StatusBadRequest)
			return
		}
		if err := f.Validate(); err != nil {
			http.Error(w, "validation error: "+err.Error(), http.StatusBadRequest)
			return
		}
	}

	// Preserve webhook secrets from external storage before overwriting.
	for _, incoming := range req.Factories {
		if incoming == nil || incoming.Name == "" {
			continue
		}
		if disk, err := loadExternalFactory(incoming.Name); err == nil {
			if incoming.WebhookSecret == "" && incoming.WebhookSecretRef == "" && disk.WebhookSecret != "" {
				incoming.WebhookSecret = disk.WebhookSecret
			}
		}
	}

	// Write each factory to external storage — attempt all writes before
	// returning so that a failure mid-batch doesn't leave a partial update.
	var saveErrs []string
	for _, f := range req.Factories {
		if f == nil || f.Name == "" {
			saveErrs = append(saveErrs, "factory name required")
			continue
		}
		if err := saveExternalFactory(f); err != nil {
			saveErrs = append(saveErrs, fmt.Sprintf("save factory %q: %v", f.Name, err))
		}
	}
	if len(saveErrs) > 0 {
		http.Error(w, strings.Join(saveErrs, "; "), http.StatusInternalServerError)
		return
	}

	// Keep hubCfg.Factories empty — external storage is the only source of truth.
	// This prevents stale in-memory configs from shadowing disk state.

	views := make([]FactoryPushView, 0, len(req.Factories))
	for _, f := range req.Factories {
		views = append(views, factoryToPushView(f))
	}
	jsonOK(w, map[string]interface{}{
		"pushed":    len(req.Factories),
		"factories": views,
	})
}

func (s *Server) handleFactoryDelete(w http.ResponseWriter, _ *http.Request, name string) {
	if err := deleteExternalFactory(name); err != nil {
		http.Error(w, "delete error: "+err.Error(), http.StatusInternalServerError)
		return
	}
	jsonOK(w, map[string]string{"deleted": name})
}
