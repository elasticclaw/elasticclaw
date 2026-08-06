package hub

import (
	"encoding/json"
	"net/http"

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
	Name                string               `json:"name"`
	Integration         string               `json:"integration"`
	Workspace           string               `json:"workspace"`
	TriggerStatus       string               `json:"trigger_status"`
	DoneStatus          string               `json:"done_status,omitempty"`
	Template            string               `json:"template"`
	Labels              []string             `json:"labels,omitempty"`
	ExcludeLabels       []string             `json:"exclude_labels,omitempty"`
	AssignedTo          string               `json:"assigned_to,omitempty"`
	Enabled             *bool                `json:"enabled,omitempty"`
	HasWebhookSecret    bool                 `json:"has_webhook_secret"`
	WebhookSecretRef    string               `json:"webhook_secret_ref,omitempty"`
	PipelineYAML        string               `json:"pipeline_yaml,omitempty"`
	EnableManualTrigger bool                 `json:"enable_manual_trigger,omitempty"`
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
		ExcludeLabels:       f.ExcludeLabels,
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
	// On-disk factories/ is retired. Always return an empty list so the UI
	// does not surface ghost automations that no longer run.
	_ = r
	jsonOK(w, []FactoryPushView{})
}

// FactoryPushRequest is the payload for POST /api/factories.
type FactoryPushRequest struct {
	Factories []*types.FactoryConfig `json:"factories"`
}

func (s *Server) handleFactoriesPush(w http.ResponseWriter, r *http.Request) {
	// Drain body so clients don't hang, then reject.
	var req FactoryPushRequest
	_ = json.NewDecoder(r.Body).Decode(&req)
	http.Error(w, errFactoriesRetired.Error(), http.StatusGone)
}

func (s *Server) handleFactoryDelete(w http.ResponseWriter, _ *http.Request, name string) {
	// Best-effort remove leftover factories/<name> on disk.
	if err := deleteExternalFactory(name); err != nil {
		http.Error(w, "delete error: "+err.Error(), http.StatusInternalServerError)
		return
	}
	jsonOK(w, map[string]string{"deleted": name})
}
