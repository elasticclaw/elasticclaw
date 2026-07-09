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
			writeErr(w, http.StatusBadRequest, "bad_request", "name required")
			return
		}
		s.handleSecretDelete(w, r, name)
	default:
		writeErr(w, http.StatusMethodNotAllowed, "method_not_allowed", "method not allowed")
	}
}

func (s *Server) handleSecretsList(w http.ResponseWriter, _ *http.Request) {
	// Read from disk so manually-edited hub.yaml entries are visible immediately
	cfg, err := config.LoadHubConfig()
	if err != nil || cfg == nil {
		jsonOK(w, map[string][]string{"secrets": {}})
		return
	}
	names := make([]string, 0, len(cfg.Secrets))
	for k := range cfg.Secrets {
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
		writeErr(w, http.StatusBadRequest, "bad_request", "invalid JSON: "+err.Error())
		return
	}
	if req.Name == "" {
		writeErr(w, http.StatusBadRequest, "bad_request", "name required")
		return
	}

	s.cfgMu.Lock()
	cfgCopy := *s.hubCfg
	newSecrets := make(map[string]string, len(cfgCopy.Secrets)+1)
	for k, v := range cfgCopy.Secrets {
		newSecrets[k] = v
	}
	newSecrets[req.Name] = req.Value
	cfgCopy.Secrets = newSecrets
	s.hubCfg = &cfgCopy
	s.cfgMu.Unlock()

	if err := config.SaveHubConfig(&cfgCopy); err != nil {
		writeErr(w, http.StatusInternalServerError, "internal", "failed to save config: "+err.Error())
		return
	}

	jsonOK(w, map[string]string{"upserted": req.Name})
}

func (s *Server) handleSecretDelete(w http.ResponseWriter, _ *http.Request, name string) {
	diskCfg, err := config.LoadHubConfig()
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "internal", "failed to load config: "+err.Error())
		return
	}

	s.cfgMu.Lock()
	_, inMemory := s.hubCfg.Secrets[name]
	onDisk := false
	if diskCfg != nil {
		_, onDisk = diskCfg.Secrets[name]
	}
	if !inMemory && !onDisk {
		s.cfgMu.Unlock()
		writeErr(w, http.StatusNotFound, "not_found", "secret not found")
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
	s.cfgMu.Unlock()

	cfgToSave := cfgCopy
	if diskCfg != nil {
		// Preserve disk-only secrets except the one being explicitly deleted.
		mergedSecrets := make(map[string]string, len(cfgCopy.Secrets)+len(diskCfg.Secrets))
		for k, v := range cfgCopy.Secrets {
			if k != name {
				mergedSecrets[k] = v
			}
		}
		for k, v := range diskCfg.Secrets {
			if k == name {
				continue
			}
			if _, already := mergedSecrets[k]; !already {
				mergedSecrets[k] = v
			}
		}
		cfgToSave.Secrets = mergedSecrets
	}
	if err := config.SaveHubConfigNoSecretMerge(&cfgToSave); err != nil {
		writeErr(w, http.StatusInternalServerError, "internal", "failed to save config: "+err.Error())
		return
	}

	jsonOK(w, map[string]string{"deleted": name})
}
