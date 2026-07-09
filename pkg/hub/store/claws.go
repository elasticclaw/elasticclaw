package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"time"

	"github.com/elasticclaw/elasticclaw/pkg/types"
)

// ClawsRepo is the repository for the claw aggregate (the claws table
// plus its dependent conversation rows). SQL moved verbatim from the
// pkg/hub handlers during the phase-2 store extraction.
type ClawsRepo struct {
	st *Store
}

// clawColumns is the shared projection for full claw reads.
const clawColumns = `id, name, template, COALESCE(provider,''), COALESCE(provider_id,''), status, last_seen, created_at, ssh_host, ssh_port, ssh_user, COALESCE(tags,'[]'), COALESCE(color,''), COALESCE(bootstrap_status,''), COALESCE(bootstrap_diagnostic,''), COALESCE(github_issue_id,'')`

func scanClaw(scan func(dest ...any) error, c *types.Claw) error {
	var lastSeen sql.NullTime
	var tagsJSON string
	if err := scan(&c.ID, &c.Name, &c.Template, &c.Provider, &c.ProviderID, &c.Status, &lastSeen, &c.CreatedAt, &c.SSHHost, &c.SSHPort, &c.SSHUser, &tagsJSON, &c.Color, &c.BootstrapStatus, &c.BootstrapDiagnostic, &c.GitHubIssueID); err != nil {
		return err
	}
	_ = json.Unmarshal([]byte(tagsJSON), &c.Tags)
	if lastSeen.Valid {
		c.LastSeen = lastSeen.Time
	}
	return nil
}

// List returns the tenant's non-deleted claws, newest first. Rows that
// fail to scan are skipped (pre-extraction behavior).
func (r *ClawsRepo) List(ctx context.Context, tenantID string) ([]types.Claw, error) {
	var out []types.Claw
	err := r.st.queryScan(ctx,
		`SELECT `+clawColumns+` FROM claws WHERE tenant_id = ? AND status != 'deleted' ORDER BY created_at DESC`,
		[]any{tenantID},
		func(rows *sql.Rows) error {
			out = out[:0]
			for rows.Next() {
				var c types.Claw
				if err := scanClaw(rows.Scan, &c); err != nil {
					continue
				}
				out = append(out, c)
			}
			return nil
		},
	)
	if err != nil {
		return nil, err
	}
	return out, nil
}

// Get returns a single non-deleted claw. Returns sql.ErrNoRows when the
// claw does not exist for the tenant.
func (r *ClawsRepo) Get(ctx context.Context, clawID, tenantID string) (types.Claw, error) {
	var c types.Claw
	err := retryBusy(ctx, func() error {
		row := r.st.db.QueryRowContext(ctx,
			`SELECT `+clawColumns+` FROM claws WHERE id = ? AND tenant_id = ? AND status != 'deleted'`,
			clawID, tenantID,
		)
		return scanClaw(row.Scan, &c)
	})
	return c, err
}

// CreateParams carries the pre-provisioning claw row inserted by the
// create-claw handler.
type CreateParams struct {
	ID              string
	TenantID        string
	Name            string
	Template        string
	Provider        string
	DefaultModel    string
	TemplateFiles   string // JSON map filename -> content
	GitHubRepos     string // JSON array
	LinearWorkspace string
	Nix             bool
	Docker          bool
	Tags            []string
	Color           string
	LLMKey          string
	Status          string
	CreatedAt       time.Time
}

// Create inserts the pre-registered claw row.
func (r *ClawsRepo) Create(ctx context.Context, p CreateParams) error {
	nix, docker := 0, 0
	if p.Nix {
		nix = 1
	}
	if p.Docker {
		docker = 1
	}
	tagsJSON, _ := json.Marshal(p.Tags)
	_, err := r.st.exec(ctx,
		`INSERT INTO claws(id, tenant_id, name, template, provider, default_model, template_files, github_repos, linear_workspace, nix, docker, tags, color, llm_key, status, created_at) VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
		p.ID, p.TenantID, p.Name, p.Template, p.Provider, p.DefaultModel, p.TemplateFiles,
		p.GitHubRepos, p.LinearWorkspace, nix, docker, string(tagsJSON), p.Color, p.LLMKey, p.Status, p.CreatedAt,
	)
	return err
}

// Tags returns the claw's tags. Returns sql.ErrNoRows when the claw does
// not exist for the tenant.
func (r *ClawsRepo) Tags(ctx context.Context, clawID, tenantID string) ([]string, error) {
	var tagsJSON string
	if err := r.st.queryRowScan(ctx,
		`SELECT COALESCE(tags,'[]') FROM claws WHERE id = ? AND tenant_id = ?`,
		[]any{clawID, tenantID}, &tagsJSON,
	); err != nil {
		return nil, err
	}
	var tags []string
	_ = json.Unmarshal([]byte(tagsJSON), &tags)
	return tags, nil
}

// SetName renames the claw.
func (r *ClawsRepo) SetName(ctx context.Context, clawID, tenantID, name string) error {
	_, err := r.st.exec(ctx, `UPDATE claws SET name = ? WHERE id = ? AND tenant_id = ?`, name, clawID, tenantID)
	return err
}

// SetTags replaces the claw's tags (statement unchanged from the
// pre-extraction handler, including the updated_at touch).
func (r *ClawsRepo) SetTags(ctx context.Context, clawID, tenantID string, tags []string) error {
	tagsJSON, _ := json.Marshal(tags)
	_, err := r.st.exec(ctx, `UPDATE claws SET tags = ?, updated_at = datetime('now') WHERE id = ? AND tenant_id = ?`, string(tagsJSON), clawID, tenantID)
	return err
}

// SetColor sets the claw's dashboard color.
func (r *ClawsRepo) SetColor(ctx context.Context, clawID, tenantID, color string) error {
	_, err := r.st.exec(ctx, `UPDATE claws SET color = ? WHERE id = ? AND tenant_id = ?`, color, clawID, tenantID)
	return err
}

// ResolveID resolves a full claw ID or unique short prefix to the full
// ID. Returns "" when nothing matches.
func (r *ClawsRepo) ResolveID(ctx context.Context, tenantID, idOrPrefix string) string {
	var fullID string
	_ = r.st.queryRowScan(ctx,
		`SELECT id FROM claws WHERE tenant_id = ? AND (id = ? OR id LIKE ?)`,
		[]any{tenantID, idOrPrefix, idOrPrefix + "%"}, &fullID,
	)
	return fullID
}

// ProviderInfo returns the claw's provider, provider-side ID and status.
// Missing rows yield empty strings (pre-extraction behavior: the error
// was ignored).
func (r *ClawsRepo) ProviderInfo(ctx context.Context, clawID, tenantID string) (provider, providerID, status string) {
	_ = r.st.queryRowScan(ctx,
		`SELECT COALESCE(provider,''), COALESCE(provider_id,''), COALESCE(status,'') FROM claws WHERE id = ? AND tenant_id = ?`,
		[]any{clawID, tenantID}, &provider, &providerID, &status,
	)
	return provider, providerID, status
}

// SoftDelete marks the claw deleted and clears its bootstrap status,
// returning the number of rows affected (0 when already deleted/absent).
func (r *ClawsRepo) SoftDelete(ctx context.Context, clawID, tenantID string) (int64, error) {
	res, err := r.st.exec(ctx,
		`UPDATE claws SET status='deleted', bootstrap_status='' WHERE id = ? AND tenant_id = ? AND status != 'deleted'`,
		clawID, tenantID,
	)
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}

// PurgeConversation deletes the claw's messages and PR links in one
// transaction (previously two independent statements in the kill path).
func (r *ClawsRepo) PurgeConversation(ctx context.Context, clawID string) error {
	return r.st.WithTx(ctx, func(tx *sql.Tx) error {
		if _, err := tx.ExecContext(ctx, `DELETE FROM messages WHERE claw_id = ?`, clawID); err != nil {
			return err
		}
		_, err := tx.ExecContext(ctx, `DELETE FROM claw_prs WHERE claw_id = ?`, clawID)
		return err
	})
}

// BootstrapInfo returns the claw's bootstrap_ok flag and provider.
// Missing rows yield zero values (pre-extraction behavior).
func (r *ClawsRepo) BootstrapInfo(ctx context.Context, clawID, tenantID string) (bootstrapOK int, provider string) {
	_ = r.st.queryRowScan(ctx,
		`SELECT COALESCE(bootstrap_ok,0), COALESCE(provider,'') FROM claws WHERE id = ? AND tenant_id = ?`,
		[]any{clawID, tenantID}, &bootstrapOK, &provider,
	)
	return bootstrapOK, provider
}

// RegisterUpsert upserts the claw row on bridge registration, keeping
// terminal/watching states sticky across reconnects, and returns the
// resulting status.
func (r *ClawsRepo) RegisterUpsert(ctx context.Context, clawID, tenantID, name, template, desiredStatus string, at time.Time) (string, error) {
	status := desiredStatus
	err := r.st.queryRowScan(ctx,
		`INSERT INTO claws(id,tenant_id,name,template,status,last_seen,created_at) VALUES(?,?,?,?,?,?,?)
		 ON CONFLICT(id) DO UPDATE SET name=excluded.name, template=excluded.template,
		 status=CASE WHEN claws.status IN ('idle','deleted') THEN claws.status ELSE excluded.status END,
		 last_seen=excluded.last_seen
		 RETURNING status`,
		[]any{clawID, tenantID, name, template, desiredStatus, at, at}, &status,
	)
	return status, err
}

// Status returns the claw's stored status ("" when absent).
func (r *ClawsRepo) Status(ctx context.Context, clawID string) string {
	var status string
	_ = r.st.queryRowScan(ctx, `SELECT status FROM claws WHERE id=?`, []any{clawID}, &status)
	return status
}

// StatusForTenant returns the claw's stored status scoped to a tenant
// ("" when absent).
func (r *ClawsRepo) StatusForTenant(ctx context.Context, clawID, tenantID string) string {
	var status string
	_ = r.st.queryRowScan(ctx, `SELECT status FROM claws WHERE id = ? AND tenant_id = ?`, []any{clawID, tenantID}, &status)
	return status
}

// MarkOffline sets the claw offline and stamps last_seen.
func (r *ClawsRepo) MarkOffline(ctx context.Context, clawID string, at time.Time) error {
	_, err := r.st.exec(ctx, `UPDATE claws SET status='offline', last_seen=? WHERE id=?`, at, clawID)
	return err
}

// TouchLastSeen stamps the claw's last_seen heartbeat.
func (r *ClawsRepo) TouchLastSeen(ctx context.Context, clawID string, at time.Time) error {
	_, err := r.st.exec(ctx, `UPDATE claws SET last_seen=? WHERE id=?`, at, clawID)
	return err
}

// PromoteStartingToConnected flips a bootstrapped starting claw to
// connected, returning the number of rows updated (0 when the claw was
// not in that state).
func (r *ClawsRepo) PromoteStartingToConnected(ctx context.Context, clawID string) (int64, error) {
	res, err := r.st.exec(ctx, `UPDATE claws SET status='connected', bootstrap_status='' WHERE id=? AND status='starting' AND bootstrap_ok=1`, clawID)
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}

// SSHEndpoint returns the claw's SSH connection details. Returns
// sql.ErrNoRows when the claw does not exist for the tenant.
func (r *ClawsRepo) SSHEndpoint(ctx context.Context, clawID, tenantID string) (host string, port int, user string, err error) {
	err = r.st.queryRowScan(ctx,
		`SELECT ssh_host, ssh_port, ssh_user FROM claws WHERE id = ? AND tenant_id = ?`,
		[]any{clawID, tenantID}, &host, &port, &user,
	)
	return host, port, user, err
}

// WorkspaceRepos returns the claw's workspace (template) name and raw
// github_repos JSON for GitHub token minting.
func (r *ClawsRepo) WorkspaceRepos(ctx context.Context, clawID string) (workspace, reposJSON string, err error) {
	err = r.st.queryRowScan(ctx,
		`SELECT COALESCE(template,''), github_repos FROM claws WHERE id = ?`,
		[]any{clawID}, &workspace, &reposJSON,
	)
	return workspace, reposJSON, err
}

// StatusRow is the projection the user-WS connect path emits for claws
// that are not yet bridge-connected.
type StatusRow struct {
	ID                  string
	Name                string
	Status              string
	TagsJSON            string
	BootstrapStatus     string
	BootstrapDiagnostic string
	GitHubIssueID       string
}

// ListStatusRows returns the tenant's claws that are not offline (the
// user-WS initial snapshot). Scan errors on individual rows are ignored
// (pre-extraction behavior).
func (r *ClawsRepo) ListStatusRows(ctx context.Context, tenantID string) []StatusRow {
	var out []StatusRow
	err := r.st.queryScan(ctx,
		`SELECT id, name, status, COALESCE(tags,'[]'), COALESCE(bootstrap_status,''), COALESCE(bootstrap_diagnostic,''), COALESCE(github_issue_id,'') FROM claws WHERE tenant_id=? AND status NOT IN ('offline')`,
		[]any{tenantID},
		func(rows *sql.Rows) error {
			out = out[:0]
			for rows.Next() {
				var c StatusRow
				_ = rows.Scan(&c.ID, &c.Name, &c.Status, &c.TagsJSON, &c.BootstrapStatus, &c.BootstrapDiagnostic, &c.GitHubIssueID)
				out = append(out, c)
			}
			return nil
		},
	)
	if err != nil {
		return nil
	}
	return out
}

// Name returns the claw's display name ("" when absent).
func (r *ClawsRepo) Name(ctx context.Context, clawID string) string {
	var name string
	_ = r.st.queryRowScan(ctx, `SELECT name FROM claws WHERE id=?`, []any{clawID}, &name)
	return name
}

// TenantID returns the claw's tenant ("" when absent).
func (r *ClawsRepo) TenantID(ctx context.Context, clawID string) string {
	var tenantID string
	_ = r.st.queryRowScan(ctx, `SELECT tenant_id FROM claws WHERE id=?`, []any{clawID}, &tenantID)
	return tenantID
}

// TemplateFilesTags returns the claw's raw template_files and tags JSON.
func (r *ClawsRepo) TemplateFilesTags(ctx context.Context, clawID string) (filesJSON, tagsJSON string, err error) {
	err = r.st.queryRowScan(ctx, `SELECT template_files, tags FROM claws WHERE id=?`, []any{clawID}, &filesJSON, &tagsJSON)
	return filesJSON, tagsJSON, err
}

// NixEnabled reports whether the claw was provisioned with Nix.
func (r *ClawsRepo) NixEnabled(ctx context.Context, clawID string) (bool, error) {
	var nixEnabled int
	if err := r.st.queryRowScan(ctx, `SELECT nix FROM claws WHERE id=?`, []any{clawID}, &nixEnabled); err != nil {
		return false, err
	}
	return nixEnabled != 0, nil
}

// TriggerActorJSON returns the claw's raw trigger actor JSON.
func (r *ClawsRepo) TriggerActorJSON(ctx context.Context, clawID string) (string, error) {
	var actorJSON string
	err := r.st.queryRowScan(ctx, `SELECT COALESCE(trigger_actor_json,'') FROM claws WHERE id=?`, []any{clawID}, &actorJSON)
	return actorJSON, err
}

// StatusCounts returns the number of claws per status (the Prometheus
// claw-status gauge).
func (r *ClawsRepo) StatusCounts(ctx context.Context) (map[string]float64, error) {
	var counts map[string]float64
	err := r.st.queryScan(ctx, `SELECT status, COUNT(*) FROM claws GROUP BY status`, nil,
		func(rows *sql.Rows) error {
			counts = make(map[string]float64)
			for rows.Next() {
				var status string
				var count float64
				if err := rows.Scan(&status, &count); err != nil {
					continue
				}
				counts[status] = count
			}
			return nil
		},
	)
	if err != nil {
		return nil, err
	}
	return counts, nil
}
