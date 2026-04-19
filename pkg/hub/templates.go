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

// handleTemplateDetail handles DELETE /api/templates/{name}.
func (s *Server) handleTemplateDetail(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	if name == "" {
		http.Error(w, "missing template name", http.StatusBadRequest)
		return
	}
	if r.Method != http.MethodDelete {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if _, err := s.db.Exec(`DELETE FROM hub_templates WHERE name = ?`, name); err != nil {
		http.Error(w, "db error", http.StatusInternalServerError)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) listTemplates(w http.ResponseWriter, r *http.Request) {
	rows, err := s.db.Query(`SELECT name, updated_at FROM hub_templates ORDER BY name ASC`)
	if err != nil {
		http.Error(w, "db error", http.StatusInternalServerError)
		return
	}
	defer rows.Close()

	type entry struct {
		Name      string `json:"name"`
		UpdatedAt string `json:"updatedAt"`
	}
	var out []entry
	for rows.Next() {
		var e entry
		if err := rows.Scan(&e.Name, &e.UpdatedAt); err != nil {
			continue
		}
		out = append(out, e)
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

	filesJSON, _ := json.Marshal(body.Files)
	now := time.Now().UTC()

	_, err := s.db.Exec(`
		INSERT INTO hub_templates(name, files, created_at, updated_at)
		VALUES(?, ?, ?, ?)
		ON CONFLICT(name) DO UPDATE SET files=excluded.files, updated_at=excluded.updated_at`,
		body.Name, string(filesJSON), now, now,
	)
	if err != nil {
		http.Error(w, "db error: "+err.Error(), http.StatusInternalServerError)
		return
	}
	jsonOK(w, map[string]string{"name": body.Name})
}

// loadHubTemplate fetches a template's files from the hub DB.
// Used by the factory engine when creating claws.
func (s *Server) loadHubTemplate(name string) (map[string]string, error) {
	var filesJSON string
	err := s.db.QueryRow(`SELECT files FROM hub_templates WHERE name = ?`, name).Scan(&filesJSON)
	if err != nil {
		return nil, err
	}
	var files map[string]string
	if err := json.Unmarshal([]byte(filesJSON), &files); err != nil {
		return nil, err
	}
	return files, nil
}

// resolveTemplateFiles tries hub DB first, then local filesystem via config.ResolveTemplate.
func (s *Server) resolveTemplateFiles(name string) (map[string]string, error) {
	// Hub DB first (pushed via `elasticclaw template push`)
	files, err := s.loadHubTemplate(name)
	if err == nil {
		return files, nil
	}
	// Fall back to filesystem resolution
	templateDir, err := config.ResolveTemplate(name)
	if err != nil {
		return nil, err
	}
	fsFiles, err := config.ReadTemplateFiles(templateDir)
	if err != nil {
		return nil, err
	}
	// Convert []byte values to string
	result := make(map[string]string, len(fsFiles))
	for k, v := range fsFiles {
		result[k] = string(v)
	}
	return result, nil
}
