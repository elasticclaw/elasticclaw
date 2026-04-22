package hub

import (
	"encoding/json"
	"net/http"

	"github.com/elasticclaw/elasticclaw/pkg/config"
)

// handleSecretsCRUD handles:
//
//	GET    /api/secrets         → list secret names only (never values)
//	PUT    /api/secrets         → upsert a secret {"name": "...", "value": "..."}
//	DELETE /api/secrets?name=x  → delete a secret by name
func (s *Server) handleSecretsCRUD(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		s.handleSecretsList(w, r)
	case http.MethodPut:
		s.handleSecretUpsert(w, r)
	case http.MethodDelete:
		name := r.URL.Query().Get("name")
		if name == "" {
			http.Error(w, "name required", http.StatusBadRequest)
			return
		}
		s.handleSecretDelete(w, r, name)
	default:
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	}
}

func (s *Server) handleSecretsList(w http.ResponseWriter, _ *http.Request) {
	s.mu.RLock()
	secrets := s.hubCfg.Secrets
	s.mu.RUnlock()

	names := make([]string, 0, len(secrets))
	for k := range secrets {
		names = append(names, k)
	}
	jsonOK(w, map[string][]string{"secrets": names})
}

type secretUpsertRequest struct {
	Name  string `json:"name"`
	Value string `json:"value"`
}

func (s *Server) handleSecretUpsert(w http.ResponseWriter, r *http.Request) {
	var req secretUpsertRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "invalid JSON: "+err.Error(), http.StatusBadRequest)
		return
	}
	if req.Name == "" {
		http.Error(w, "name required", http.StatusBadRequest)
		return
	}

	s.mu.Lock()
	cfgCopy := *s.hubCfg
	newSecrets := make(map[string]string, len(cfgCopy.Secrets)+1)
	for k, v := range cfgCopy.Secrets {
		newSecrets[k] = v
	}
	newSecrets[req.Name] = req.Value
	cfgCopy.Secrets = newSecrets
	s.hubCfg = &cfgCopy
	s.mu.Unlock()

	if err := config.SaveHubConfig(&cfgCopy); err != nil {
		http.Error(w, "failed to save config: "+err.Error(), http.StatusInternalServerError)
		return
	}

	jsonOK(w, map[string]string{"upserted": req.Name})
}

func (s *Server) handleSecretDelete(w http.ResponseWriter, _ *http.Request, name string) {
	s.mu.Lock()
	if _, ok := s.hubCfg.Secrets[name]; !ok {
		s.mu.Unlock()
		http.Error(w, "secret not found", http.StatusNotFound)
		return
	}
	cfgCopy := *s.hubCfg
	newSecrets := make(map[string]string, len(cfgCopy.Secrets))
	for k, v := range cfgCopy.Secrets {
		if k != name {
			newSecrets[k] = v
		}
	}
	cfgCopy.Secrets = newSecrets
	s.hubCfg = &cfgCopy
	s.mu.Unlock()

	if err := config.SaveHubConfig(&cfgCopy); err != nil {
		http.Error(w, "failed to save config: "+err.Error(), http.StatusInternalServerError)
		return
	}

	jsonOK(w, map[string]string{"deleted": name})
}
