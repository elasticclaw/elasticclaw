package aichat

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"net/http"
	"time"
)

type ArtifactListItem struct {
	ID             string `json:"id"`
	Slug           string `json:"slug"`
	Title          string `json:"title"`
	CurrentVersion int    `json:"currentVersion"`
	ThreadID       string `json:"threadId"`
	ThreadTitle    string `json:"threadTitle"`
	UpdatedAt      int64  `json:"updatedAt"`
	URL            string `json:"url"`
}

func (s *Store) Artifacts(ctx context.Context, tenant, owner string) ([]ArtifactListItem, error) {
	rows, err := s.DB.QueryContext(ctx, `SELECT a.id,a.slug,a.title,a.current_version,a.thread_id,t.title,a.updated_at
 FROM ai_chat_artifacts a JOIN ai_chat_threads t ON t.id=a.thread_id AND t.tenant_id=a.tenant_id AND t.owner_login=a.owner_login
 WHERE a.tenant_id=? AND a.owner_login=? AND a.deleted_at IS NULL ORDER BY a.updated_at DESC,a.id DESC LIMIT 100`, tenant, OwnerLogin(owner))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	items := []ArtifactListItem{}
	for rows.Next() {
		var item ArtifactListItem
		if err := rows.Scan(&item.ID, &item.Slug, &item.Title, &item.CurrentVersion, &item.ThreadID, &item.ThreadTitle, &item.UpdatedAt); err != nil {
			return nil, err
		}
		item.URL = "/artifacts/" + item.Slug
		items = append(items, item)
	}
	return items, rows.Err()
}

func (a *api) listArtifacts(w http.ResponseWriter, r *http.Request) {
	tenant, owner := a.identity(r)
	items, err := a.store.Artifacts(r.Context(), tenant, owner)
	if err != nil {
		http.Error(w, "Unable to load artifacts.", 500)
		return
	}
	jsonResponse(w, 200, items)
}

func (s *Store) DeleteArtifact(ctx context.Context, tenant, owner, slug string) (Thread, error) {
	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		return Thread{}, err
	}
	defer tx.Rollback()
	// Scope the mutation too; a repeated deletion preserves its original timestamp.
	now := time.Now().UnixMilli()
	result, err := tx.ExecContext(ctx, `UPDATE ai_chat_artifacts SET deleted_at=COALESCE(deleted_at,?),updated_at=CASE WHEN deleted_at IS NULL THEN ? ELSE updated_at END WHERE slug=? AND tenant_id=? AND owner_login=?`, now, now, slug, tenant, OwnerLogin(owner))
	if err != nil {
		return Thread{}, err
	}
	n, err := result.RowsAffected()
	if err != nil {
		return Thread{}, err
	}
	if n == 0 {
		return Thread{}, sql.ErrNoRows
	}
	var id string
	if err := tx.QueryRowContext(ctx, `SELECT thread_id FROM ai_chat_artifacts WHERE slug=? AND tenant_id=? AND owner_login=?`, slug, tenant, OwnerLogin(owner)).Scan(&id); err != nil {
		return Thread{}, err
	}
	t, err := scanThread(tx.QueryRowContext(ctx, `SELECT `+threadColumns+` FROM ai_chat_threads WHERE id=? AND tenant_id=? AND owner_login=?`, id, tenant, OwnerLogin(owner)))
	if err != nil {
		return Thread{}, err
	}
	return t, tx.Commit()
}

func (a *api) deleteArtifact(w http.ResponseWriter, r *http.Request) {
	tenant, owner := a.identity(r)
	t, err := a.store.DeleteArtifact(r.Context(), tenant, owner, r.PathValue("slug"))
	if errors.Is(err, sql.ErrNoRows) {
		http.NotFound(w, r)
		return
	}
	if err != nil {
		http.Error(w, "Unable to delete artifact.", 500)
		return
	}
	a.publish(t)
	w.WriteHeader(http.StatusNoContent)
}

// Annotate copies at read time, including model history. Persisted blocks remain
// immutable, and unknown block fields survive as the protocol grows.
func (s *Store) annotateDeletedArtifacts(ctx context.Context, threadID string, messages []Message) error {
	rows, err := s.DB.QueryContext(ctx, `SELECT a.id FROM ai_chat_artifacts a JOIN ai_chat_threads t ON t.id=a.thread_id AND t.tenant_id=a.tenant_id AND t.owner_login=a.owner_login WHERE t.id=? AND a.deleted_at IS NOT NULL`, threadID)
	if err != nil {
		return err
	}
	defer rows.Close()
	deleted := map[string]bool{}
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return err
		}
		deleted[id] = true
	}
	if err := rows.Err(); err != nil {
		return err
	}
	if len(deleted) == 0 {
		return nil
	}
	for i := range messages {
		var blocks []map[string]json.RawMessage
		if json.Unmarshal(messages[i].Blocks, &blocks) != nil {
			continue
		}
		changed := false
		for _, block := range blocks {
			if string(block["type"]) != `"action"` || string(block["kind"]) != `"artifact"` {
				continue
			}
			var card map[string]json.RawMessage
			if json.Unmarshal(block["card"], &card) != nil {
				continue
			}
			var id string
			if json.Unmarshal(card["artifactId"], &id) != nil || !deleted[id] {
				continue
			}
			card["deleted"] = json.RawMessage(`true`)
			block["card"], _ = json.Marshal(card)
			changed = true
		}
		if changed {
			messages[i].Blocks, _ = json.Marshal(blocks)
		}
	}
	return nil
}
