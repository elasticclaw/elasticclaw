package store

import (
	"context"
	"database/sql"
	"time"
)

// SettingsRepo is the repository for the settings aggregate (the legacy
// hub_templates table used before external template storage). SQL moved
// verbatim from pkg/hub/templates.go and pkg/hub/settings/doctor.go
// during the phase-2 store extraction.
type SettingsRepo struct {
	st *Store
}

// TemplateNames returns the names of the legacy DB templates.
func (r *SettingsRepo) TemplateNames(ctx context.Context) ([]string, error) {
	var names []string
	err := r.st.queryScan(ctx, `SELECT name FROM hub_templates`, nil, func(rows *sql.Rows) error {
		names = names[:0]
		for rows.Next() {
			var name string
			if err := rows.Scan(&name); err != nil {
				continue
			}
			names = append(names, name)
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return names, nil
}

// TemplateInfo is one legacy DB template row in the list projection.
type TemplateInfo struct {
	Name      string
	UpdatedAt string
}

// ListTemplates returns the legacy DB templates ordered by name. Rows
// that fail to scan are skipped (pre-extraction behavior).
func (r *SettingsRepo) ListTemplates(ctx context.Context) ([]TemplateInfo, error) {
	var out []TemplateInfo
	err := r.st.queryScan(ctx, `SELECT name, updated_at FROM hub_templates ORDER BY name ASC`, nil, func(rows *sql.Rows) error {
		out = out[:0]
		for rows.Next() {
			var t TemplateInfo
			if err := rows.Scan(&t.Name, &t.UpdatedAt); err != nil {
				continue
			}
			out = append(out, t)
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

// TemplateFiles returns the raw files JSON of a legacy DB template.
// Returns sql.ErrNoRows when the template does not exist.
func (r *SettingsRepo) TemplateFiles(ctx context.Context, name string) (string, error) {
	var filesJSON string
	err := r.st.queryRowScan(ctx, `SELECT files FROM hub_templates WHERE name = ?`, []any{name}, &filesJSON)
	return filesJSON, err
}

// UpsertTemplate creates or replaces a legacy DB template.
func (r *SettingsRepo) UpsertTemplate(ctx context.Context, name, filesJSON string, at time.Time) error {
	_, err := r.st.exec(ctx, `
		INSERT INTO hub_templates(name, files, created_at, updated_at)
		VALUES(?, ?, ?, ?)
		ON CONFLICT(name) DO UPDATE SET files=excluded.files, updated_at=excluded.updated_at`,
		name, filesJSON, at, at,
	)
	return err
}

// DeleteTemplate removes a legacy DB template row if present.
func (r *SettingsRepo) DeleteTemplate(ctx context.Context, name string) error {
	_, err := r.st.exec(ctx, `DELETE FROM hub_templates WHERE name = ?`, name)
	return err
}
