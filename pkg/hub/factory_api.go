package hub

import (
	"encoding/json"
	"fmt"
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
		HasWebhookSecret: f.WebhookSecret != "" || f.WebhookSecretRef != "",
		WebhookSecretRef: f.WebhookSecretRef,
		PipelineYAML:     f.PipelineYAML,
	}
}

func (s *Server) handleFactoriesList(w http.ResponseWriter, r *http.Request) {
	nameFilter := strings.TrimSpace(r.URL.Query().Get("name"))

	// Load from external storage first
	factories, err := loadExternalFactories()
	if err != nil {
		http.Error(w, "fs error: "+err.Error(), http.StatusInternalServerError)
		return
	}

	// Merge with in-memory factories (external takes precedence)
	s.mu.RLock()
	memFactories := s.hubCfg.Factories
	s.mu.RUnlock()

	merged := make(map[string]*types.FactoryConfig, len(factories)+len(memFactories))
	for _, f := range memFactories {
		if f == nil {
			continue
		}
		merged[f.Name] = f
	}
	for _, f := range factories {
		if f == nil {
			continue
		}
		merged[f.Name] = f
	}

	views := make([]FactoryPushView, 0, len(merged))
	for _, f := range merged {
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

	// Preserve inline webhook secrets before writing to external storage.
	// Check in-memory config first, then fall back to external storage
	// so that factories with secrets only on disk are also protected.
	s.mu.RLock()
	existing := make(map[string]*types.FactoryConfig)
	for _, f := range s.hubCfg.Factories {
		if f == nil {
			continue
		}
		existing[f.Name] = f
	}
	s.mu.RUnlock()
	for _, incoming := range req.Factories {
		if incoming == nil || incoming.Name == "" {
			continue
		}
		prev, ok := existing[incoming.Name]
		if !ok {
			// Factory not in memory — try external storage
			if disk, err := loadExternalFactory(incoming.Name); err == nil {
				prev = disk
				ok = true
			}
		}
		if ok {
			if incoming.WebhookSecret == "" && incoming.WebhookSecretRef == "" && prev.WebhookSecret != "" {
				incoming.WebhookSecret = prev.WebhookSecret
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

	// Also update in-memory config for backward compat during migration
	s.mu.Lock()
	existing = make(map[string]*types.FactoryConfig)
	for _, f := range s.hubCfg.Factories {
		if f == nil {
			continue
		}
		existing[f.Name] = f
	}
	incomingByName := make(map[string]*types.FactoryConfig, len(req.Factories))
	incomingOrder := make([]string, 0, len(req.Factories))
	seenIncoming := make(map[string]bool, len(req.Factories))
	for _, incoming := range req.Factories {
		existing[incoming.Name] = incoming
		incomingByName[incoming.Name] = incoming
		if !seenIncoming[incoming.Name] {
			seenIncoming[incoming.Name] = true
			incomingOrder = append(incomingOrder, incoming.Name)
		}
	}
	updated := make([]*types.FactoryConfig, 0, len(existing))
	for _, f := range s.hubCfg.Factories {
		if incoming, ok := incomingByName[f.Name]; ok {
			updated = append(updated, incoming)
			delete(incomingByName, f.Name)
			continue
		}
		updated = append(updated, f)
	}
	for _, name := range incomingOrder {
		if incoming, ok := incomingByName[name]; ok {
			updated = append(updated, incoming)
		}
	}
	cfgCopy := *s.hubCfg
	cfgCopy.Factories = updated
	s.hubCfg = &cfgCopy
	s.mu.Unlock()

	// Save to hub.yaml for backward compat during migration
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
	// Delete from external storage first
	if err := deleteExternalFactory(name); err != nil {
		http.Error(w, "delete error: "+err.Error(), http.StatusInternalServerError)
		return
	}

	s.mu.Lock()
	factories := make([]*types.FactoryConfig, 0, len(s.hubCfg.Factories))
	found := false
	for _, f := range s.hubCfg.Factories {
		if f == nil {
			continue
		}
		if strings.EqualFold(f.Name, name) {
			found = true
			continue
		}
		factories = append(factories, f)
	}
	if !found {
		s.mu.Unlock()
		// If not in memory but deleted from disk, still report success
		jsonOK(w, map[string]string{"deleted": name})
		return
	}
	cfgCopy := *s.hubCfg
	cfgCopy.Factories = factories
	s.hubCfg = &cfgCopy
	s.mu.Unlock()

	if err := config.SaveHubConfig(&cfgCopy); err != nil {
		http.Error(w, "failed to save config: "+err.Error(), http.StatusInternalServerError)
		return
	}

	jsonOK(w, map[string]string{"deleted": name})
}
