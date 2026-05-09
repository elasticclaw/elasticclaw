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
		_, _ = s.db.Exec(`DELETE FROM hub_templates WHERE name = ?`, name)
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

	// Also include legacy DB templates (for migration period)
	rows, err := s.db.Query(`SELECT name, updated_at FROM hub_templates ORDER BY name ASC`)
	if err == nil {
		defer rows.Close()
		for rows.Next() {
			var name, updatedAt string
			if err := rows.Scan(&name, &updatedAt); err != nil {
				continue
			}
			// Deduplicate: external takes precedence
			found := false
			for _, n := range names {
				if n == name {
					found = true
					break
				}
			}
			if !found {
				names = append(names, name)
			}
		}
	}

	// Collect real updated_at timestamps from legacy DB rows
	dbUpdatedAt := make(map[string]string)
	rows2, err2 := s.db.Query(`SELECT name, updated_at FROM hub_templates ORDER BY name ASC`)
	if err2 == nil {
		defer rows2.Close()
		for rows2.Next() {
			var n, ts string
			if err := rows2.Scan(&n, &ts); err == nil {
				dbUpdatedAt[n] = ts
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

	// Write to external storage
	if err := saveExternalTemplate(body.Name, body.Files); err != nil {
		http.Error(w, "save error: "+err.Error(), http.StatusInternalServerError)
		return
	}

	// Also write to legacy DB for backward compat during migration
	filesJSON, _ := json.Marshal(body.Files)
	now := time.Now().UTC()
	_, _ = s.db.Exec(`
		INSERT INTO hub_templates(name, files, created_at, updated_at)
		VALUES(?, ?, ?, ?)
		ON CONFLICT(name) DO UPDATE SET files=excluded.files, updated_at=excluded.updated_at`,
		body.Name, string(filesJSON), now, now,
	)

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
	var filesJSON string
	err = s.db.QueryRow(`SELECT files FROM hub_templates WHERE name = ?`, name).Scan(&filesJSON)
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
