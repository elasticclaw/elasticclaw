package store

import (
	"context"
	"database/sql"
	"strings"
	"time"

	"github.com/elasticclaw/elasticclaw/pkg/types"
)

// MessagesRepo is the repository for the conversation-message aggregate.
// SQL moved verbatim from the pkg/hub handlers and the claws service
// during the phase-2 store extraction.
type MessagesRepo struct {
	st *Store
}

const messageColumns = `id, claw_id, tenant_id, role, content, COALESCE(format,''), created_at`

// Insert stores a new message. The format column is written as-is (empty
// string matches the column default used by the pre-extraction inserts
// that omitted it).
func (r *MessagesRepo) Insert(ctx context.Context, m types.HubMessage) error {
	_, err := r.st.exec(ctx,
		`INSERT INTO messages(id,claw_id,tenant_id,role,content,format,created_at) VALUES(?,?,?,?,?,?,?)`,
		m.ID, m.ClawID, m.TenantID, m.Role, m.Content, m.Format, m.CreatedAt,
	)
	return err
}

// Upsert stores a message, replacing only the content when the ID
// already exists (streaming chunks and finalization reuse the same ID).
func (r *MessagesRepo) Upsert(ctx context.Context, m types.HubMessage) error {
	_, err := r.st.exec(ctx,
		`INSERT INTO messages(id,claw_id,tenant_id,role,content,format,created_at) VALUES(?,?,?,?,?,?,?)
		 ON CONFLICT(id) DO UPDATE SET content=excluded.content`,
		m.ID, m.ClawID, m.TenantID, m.Role, m.Content, m.Format, m.CreatedAt,
	)
	return err
}

// hiddenFilterSQL builds the system-marker exclusion clause for n marker
// contents (the wake/initial-plan markers the UI never shows).
func hiddenFilterSQL(n int) string {
	if n == 0 {
		return ""
	}
	placeholders := strings.TrimSuffix(strings.Repeat(Placeholder+", ", n), ", ")
	return ` AND NOT (role = 'system' AND content IN (` + placeholders + `))`
}

func scanMessages(rows *sql.Rows) ([]types.HubMessage, error) {
	var msgs []types.HubMessage
	for rows.Next() {
		var m types.HubMessage
		if err := rows.Scan(&m.ID, &m.ClawID, &m.TenantID, &m.Role, &m.Content, &m.Format, &m.CreatedAt); err != nil {
			return nil, err
		}
		msgs = append(msgs, m)
	}
	return msgs, rows.Err()
}

// VisibleWindow selects a page of user-visible messages (all roles,
// system markers excluded). Exactly one of Before/After may be set; the
// zero filter returns the newest Limit messages.
type VisibleWindow struct {
	ClawID   string
	TenantID string
	// Before/After are opaque cursor values compared against created_at
	// (the callers pass the raw query-string timestamp, matching the
	// pre-extraction behavior).
	Before string
	After  string
	Limit  int
	// HiddenSystemContents are system-role message contents excluded
	// from every page (wake and initial-plan markers).
	HiddenSystemContents []string
}

// ListVisible returns a page of visible messages. Pages selected with
// Before (or no cursor) are returned newest-first, pages selected with
// After oldest-first — the same order the pre-extraction queries used;
// the handler reverses DESC pages.
func (r *MessagesRepo) ListVisible(ctx context.Context, w VisibleWindow) ([]types.HubMessage, error) {
	query := `SELECT ` + messageColumns + ` FROM messages
		 WHERE claw_id = ? AND tenant_id = ?`
	args := []any{w.ClawID, w.TenantID}
	switch {
	case w.Before != "":
		query += ` AND created_at < ?`
		args = append(args, w.Before)
	case w.After != "":
		query += ` AND created_at > ?`
		args = append(args, w.After)
	}
	query += hiddenFilterSQL(len(w.HiddenSystemContents))
	for _, hidden := range w.HiddenSystemContents {
		args = append(args, hidden)
	}
	if w.After != "" {
		query += ` ORDER BY created_at ASC LIMIT ?`
	} else {
		query += ` ORDER BY created_at DESC LIMIT ?`
	}
	args = append(args, w.Limit)

	rows, err := r.st.query(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var msgs []types.HubMessage
	for rows.Next() {
		var m types.HubMessage
		if err := rows.Scan(&m.ID, &m.ClawID, &m.TenantID, &m.Role, &m.Content, &m.Format, &m.CreatedAt); err != nil {
			continue
		}
		msgs = append(msgs, m)
	}
	return msgs, rows.Err()
}

// ListConversation returns up to limit conversation messages (role !=
// 'activity', system markers excluded) older than the optional before
// cursor, newest-first. The caller reverses the page for display.
func (r *MessagesRepo) ListConversation(ctx context.Context, clawID, tenantID string, before any, limit int, hidden []string) ([]types.HubMessage, error) {
	query := `SELECT ` + messageColumns + ` FROM messages
		WHERE claw_id = ? AND tenant_id = ? AND role != 'activity' ` + hiddenFilterSQL(len(hidden))
	args := []any{clawID, tenantID}
	for _, h := range hidden {
		args = append(args, h)
	}
	if before != nil {
		query += ` AND created_at < ?`
		args = append(args, before)
	}
	query += ` ORDER BY created_at DESC LIMIT ?`
	args = append(args, limit)

	rows, err := r.st.query(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanMessages(rows)
}

// HasConversationBefore reports whether any conversation message (role
// != 'activity', system markers excluded) exists before the timestamp.
func (r *MessagesRepo) HasConversationBefore(ctx context.Context, clawID, tenantID string, before time.Time, hidden []string) (bool, error) {
	query := `SELECT COUNT(*) FROM messages
		WHERE claw_id = ? AND tenant_id = ? AND role != 'activity' AND created_at < ? ` + hiddenFilterSQL(len(hidden))
	args := []any{clawID, tenantID, before}
	for _, h := range hidden {
		args = append(args, h)
	}
	var count int
	if err := r.st.queryRowScan(ctx, query, args, &count); err != nil {
		return false, err
	}
	return count > 0, nil
}

// ActivityWindow filters activity-role messages by optional exclusive
// created_at cursors. Cursor values are opaque (time.Time or the raw
// query string, matching the pre-extraction handlers).
type ActivityWindow struct {
	ClawID   string
	TenantID string
	From     any // created_at > From
	To       any // created_at < To
	Before   any // created_at < Before
	Limit    int
	Desc     bool
}

// ListActivity returns activity-role messages inside the window.
func (r *MessagesRepo) ListActivity(ctx context.Context, w ActivityWindow) ([]types.HubMessage, error) {
	query := `SELECT ` + messageColumns + `
		FROM messages
		WHERE claw_id = ? AND tenant_id = ? AND role = 'activity'`
	args := []any{w.ClawID, w.TenantID}
	if w.From != nil {
		query += ` AND created_at > ?`
		args = append(args, w.From)
	}
	if w.To != nil {
		query += ` AND created_at < ?`
		args = append(args, w.To)
	}
	if w.Before != nil {
		query += ` AND created_at < ?`
		args = append(args, w.Before)
	}
	if w.Desc {
		query += ` ORDER BY created_at DESC LIMIT ?`
	} else {
		query += ` ORDER BY created_at ASC LIMIT ?`
	}
	args = append(args, w.Limit)

	rows, err := r.st.query(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanMessages(rows)
}

// ActivityStats returns the count and created_at bounds of activity-role
// messages between the optional exclusive from/to timestamps.
func (r *MessagesRepo) ActivityStats(ctx context.Context, clawID, tenantID string, from, to *time.Time) (count int, minCreated, maxCreated string, err error) {
	query := `SELECT COUNT(*), COALESCE(MIN(created_at), ''), COALESCE(MAX(created_at), '')
		FROM messages WHERE claw_id = ? AND tenant_id = ? AND role = 'activity'`
	args := []any{clawID, tenantID}
	if from != nil {
		query += ` AND created_at > ?`
		args = append(args, *from)
	}
	if to != nil {
		query += ` AND created_at < ?`
		args = append(args, *to)
	}
	err = r.st.queryRowScan(ctx, query, args, &count, &minCreated, &maxCreated)
	return count, minCreated, maxCreated, err
}

// CountByClaw returns the claw's total message count.
func (r *MessagesRepo) CountByClaw(ctx context.Context, clawID string) (int, error) {
	var count int
	err := r.st.queryRowScan(ctx, `SELECT COUNT(*) FROM messages WHERE claw_id = ?`, []any{clawID}, &count)
	return count, err
}

// HasSystemMarker reports whether the claw already has a system-role
// message with exactly the marker content.
func (r *MessagesRepo) HasSystemMarker(ctx context.Context, clawID, marker string) (bool, error) {
	var count int
	if err := r.st.queryRowScan(ctx, `SELECT COUNT(*) FROM messages WHERE claw_id=? AND role='system' AND content=?`, []any{clawID, marker}, &count); err != nil {
		return false, err
	}
	return count > 0, nil
}
