package hub

import (
	"encoding/json"
	"net/http"
	"strings"

	"github.com/elasticclaw/elasticclaw/pkg/config"
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
	Name             string   `json:"name"`
	Integration      string   `json:"integration"`
	Workspace        string   `json:"workspace"`
	TriggerStatus    string   `json:"trigger_status"`
	DoneStatus       string   `json:"done_status,omitempty"`
	Template         string   `json:"template"`
	Labels           []string `json:"labels,omitempty"`
	AssignedTo       string   `json:"assigned_to,omitempty"`
	Enabled          *bool    `json:"enabled,omitempty"`
	HasWebhookSecret bool     `json:"has_webhook_secret"`
	WebhookSecretRef string   `json:"webhook_secret_ref,omitempty"`
	PipelineYAML     string   `json:"pipeline_yaml,omitempty"`
}

func factoryToPushView(f *types.FactoryConfig) FactoryPushView {
	return FactoryPushView{
		Name:             f.Name,
		Integration:      f.Integration,
		Workspace:        f.Workspace,
		TriggerStatus:    f.TriggerStatus,
		DoneStatus:       f.DoneStatus,
		Template:         f.Template,
		Labels:           f.Labels,
		AssignedTo:       f.AssignedTo,
		Enabled:          f.Enabled,
		HasWebhookSecret: f.WebhookSecret != "",
		WebhookSecretRef: f.WebhookSecretRef,
		PipelineYAML:     f.PipelineYAML,
	}
}

func (s *Server) handleFactoriesList(w http.ResponseWriter, r *http.Request) {
	nameFilter := strings.TrimSpace(r.URL.Query().Get("name"))

	s.mu.RLock()
	factories := s.hubCfg.Factories
	s.mu.RUnlock()

	views := make([]FactoryPushView, 0, len(factories))
	for _, f := range factories {
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

	s.mu.Lock()
	// Upsert by name — preserve existing webhook secrets for repo-pushed factories
	existing := make(map[string]*types.FactoryConfig)
	for _, f := range s.hubCfg.Factories {
		existing[f.Name] = f
	}
	for _, incoming := range req.Factories {
		if prev, ok := existing[incoming.Name]; ok {
			// Preserve inline webhook secret if not provided in push
			if incoming.WebhookSecret == "" && prev.WebhookSecret != "" {
				incoming.WebhookSecret = prev.WebhookSecret
			}
		}
		existing[incoming.Name] = incoming
	}
	updated := make([]*types.FactoryConfig, 0, len(existing))
	for _, f := range existing {
		updated = append(updated, f)
	}
	s.hubCfg.Factories = updated
	cfgCopy := *s.hubCfg
	s.mu.Unlock()

	if err := config.SaveHubConfig(&cfgCopy); err != nil {
		http.Error(w, "failed to save config: "+err.Error(), http.StatusInternalServerError)
		return
	}

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
	s.mu.Lock()
	factories := make([]*types.FactoryConfig, 0, len(s.hubCfg.Factories))
	found := false
	for _, f := range s.hubCfg.Factories {
		if strings.EqualFold(f.Name, name) {
			found = true
			continue
		}
		factories = append(factories, f)
	}
	s.hubCfg.Factories = factories
	cfgCopy := *s.hubCfg
	s.mu.Unlock()

	if !found {
		http.Error(w, "factory not found", http.StatusNotFound)
		return
	}

	if err := config.SaveHubConfig(&cfgCopy); err != nil {
		http.Error(w, "failed to save config: "+err.Error(), http.StatusInternalServerError)
		return
	}

	jsonOK(w, map[string]string{"deleted": name})
}
