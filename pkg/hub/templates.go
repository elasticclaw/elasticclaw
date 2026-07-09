package hub

import (
	"encoding/json"
	"net/http"
	"strings"
	"time"

	"github.com/elasticclaw/elasticclaw/pkg/config"
)

// handleTemplates handles GET /api/templates and POST /api/templates.
func (s *Server) handleTemplates(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		s.listTemplates(w, r)
	case http.MethodPost:
		s.pushTemplate(w, r)
	default:
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	}
}

// handleTemplateDetail handles GET and DELETE /api/templates/{name}.
func (s *Server) handleTemplateDetail(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	if name == "" {
		http.Error(w, "missing template name", http.StatusBadRequest)
		return
	}
	switch r.Method {
	case http.MethodGet:
		files, err := s.loadHubTemplate(name)
		if err != nil {
			http.Error(w, "template not found", http.StatusNotFound)
			return
		}
		jsonOK(w, map[string]interface{}{
			"name":  name,
			"files": files,
		})
	case http.MethodDelete:
		if err := deleteExternalTemplate(name); err != nil {
			http.Error(w, "delete error: "+err.Error(), http.StatusInternalServerError)
			return
		}
		// Also clean up legacy DB row if present
		_ = s.st().Settings().DeleteTemplate(r.Context(), name)
		w.WriteHeader(http.StatusNoContent)
	default:
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	}
}

func (s *Server) listTemplates(w http.ResponseWriter, r *http.Request) {
	// List from external storage first
	names, err := listExternalTemplates()
	if err != nil {
		http.Error(w, "fs error: "+err.Error(), http.StatusInternalServerError)
		return
	}

	// Collect legacy DB templates and their updated_at timestamps in one pass
	dbUpdatedAt := make(map[string]string)
	if dbTemplates, err := s.st().Settings().ListTemplates(r.Context()); err == nil {
		for _, tmpl := range dbTemplates {
			dbUpdatedAt[tmpl.Name] = tmpl.UpdatedAt
			// Deduplicate: external takes precedence
			found := false
			for _, n := range names {
				if n == tmpl.Name {
					found = true
					break
				}
			}
			if !found {
				names = append(names, tmpl.Name)
			}
		}
	}

	type entry struct {
		Name      string `json:"name"`
		UpdatedAt string `json:"updatedAt"`
	}
	now := time.Now().UTC().Format(time.RFC3339)
	out := make([]entry, len(names))
	for i, name := range names {
		ts := now
		if v, ok := dbUpdatedAt[name]; ok && v != "" {
			ts = v
		}
		out[i] = entry{Name: name, UpdatedAt: ts}
	}
	if out == nil {
		out = []entry{}
	}
	jsonOK(w, out)
}

func (s *Server) pushTemplate(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Name  string            `json:"name"`
		Files map[string]string `json:"files"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil || body.Name == "" {
		http.Error(w, "invalid request: name and files required", http.StatusBadRequest)
		return
	}
	// Validate name
	if strings.ContainsAny(body.Name, "/\\") || strings.Contains(body.Name, "..") {
		http.Error(w, "invalid template name", http.StatusBadRequest)
		return
	}

	// Validate template configuration (required)
	configData, ok := body.Files["elasticclaw-config.yaml"]
	if !ok {
		http.Error(w, "elasticclaw-config.yaml is required", http.StatusBadRequest)
		return
	}
	cfg, err := config.ParseTemplateConfig([]byte(configData))
	if err != nil {
		http.Error(w, "invalid elasticclaw-config.yaml: "+err.Error(), http.StatusBadRequest)
		return
	}
	if err := cfg.Validate(); err != nil {
		http.Error(w, "validation error: "+err.Error(), http.StatusBadRequest)
		return
	}

	// Write to external storage
	if err := saveExternalTemplate(body.Name, body.Files); err != nil {
		http.Error(w, "save error: "+err.Error(), http.StatusInternalServerError)
		return
	}

	// Also write to legacy DB for backward compat during migration
	filesJSON, _ := json.Marshal(body.Files)
	_ = s.st().Settings().UpsertTemplate(r.Context(), body.Name, string(filesJSON), time.Now().UTC())

	jsonOK(w, map[string]string{"name": body.Name})
}

// loadHubTemplate fetches a template's files.
// Tries external storage first, then legacy DB.
func (s *Server) loadHubTemplate(name string) (map[string]string, error) {
	// External storage first
	files, err := loadExternalTemplate(name)
	if err == nil {
		return files, nil
	}
	// Legacy DB fallback
	filesJSON, err := s.st().Settings().TemplateFiles(s.base(), name)
	if err != nil {
		return nil, err
	}
	if err := json.Unmarshal([]byte(filesJSON), &files); err != nil {
		return nil, err
	}
	return files, nil
}

// resolveTemplateFiles tries external storage first, then legacy DB, then local filesystem.
func (s *Server) resolveTemplateFiles(name string) (map[string]string, error) {
	// External storage first
	files, err := loadExternalTemplate(name)
	if err == nil {
		return files, nil
	}
	// Legacy DB second
	files, err = s.loadHubTemplate(name)
	if err == nil {
		return files, nil
	}
	// Fall back to filesystem resolution
	templateDir, err := config.ResolveTemplate(name)
	if err != nil {
		return nil, err
	}
	return config.ReadTemplateFiles(templateDir)
}
