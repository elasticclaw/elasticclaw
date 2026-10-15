package aichat

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"net/http"
	"time"

	"github.com/elasticclaw/elasticclaw/pkg/hub/aichat/tools"
)

func (a *api) dismissTicketAttempt(w http.ResponseWriter, r *http.Request) {
	t, ok := a.owned(w, r)
	if !ok {
		return
	}
	if err := a.runner.RecoverInterrupted(r.Context(), t); err != nil {
		http.Error(w, "unable to recover interrupted turn", 500)
		return
	}
	err := a.store.dismissTicketAttempt(r.Context(), t.ID, r.PathValue("attemptId"))
	if errors.Is(err, sql.ErrNoRows) {
		http.NotFound(w, r)
		return
	}
	if err != nil {
		http.Error(w, "unable to dismiss ticket attempt", 500)
		return
	}
	a.publish(t)
	w.WriteHeader(http.StatusNoContent)
}

func (s *Store) dismissTicketAttempt(ctx context.Context, threadID, attemptID string) error {
	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var runID, result, rawBlocks string
	err = tx.QueryRowContext(ctx, `SELECT r.id,r.result_json,COALESCE(m.blocks_json,'[]')
		FROM ai_chat_tool_runs r JOIN ai_chat_messages m ON m.id=r.message_id
		WHERE m.thread_id=? AND m.id=? AND m.status<>'streaming'
		AND r.tool='ticket' AND r.provider='AI Chat' AND json_extract(NULLIF(r.result_json,''),'$.requestSent')=1`, threadID, attemptID).Scan(&runID, &result, &rawBlocks)
	if err != nil {
		return err
	}
	var marker struct{ Dismissed bool }
	var blocks []tools.Block
	if json.Unmarshal([]byte(result), &marker) != nil || json.Unmarshal([]byte(rawBlocks), &blocks) != nil {
		return sql.ErrNoRows
	}
	found := false
	for i := range blocks {
		block := &blocks[i]
		if block.Type != "action" || block.Kind != "ticket" {
			continue
		}
		var card TicketCard
		if block.State != "failed" || json.Unmarshal(block.Card, &card) != nil || !card.OutcomeUnknown {
			return sql.ErrNoRows
		}
		card.Dismissed = true
		block.Card = redactedJSON(card, nil)
		found = true
	}
	if !found {
		return sql.ErrNoRows
	}
	if marker.Dismissed {
		return nil
	}
	if _, err = tx.ExecContext(ctx, `UPDATE ai_chat_tool_runs SET result_json=json_set(result_json,'$.dismissed',json('true')) WHERE id=?`, runID); err != nil {
		return err
	}
	if _, err = tx.ExecContext(ctx, `UPDATE ai_chat_messages SET blocks_json=? WHERE id=?`, string(redactedJSON(blocks, nil)), attemptID); err != nil {
		return err
	}
	if _, err = tx.ExecContext(ctx, `UPDATE ai_chat_threads SET updated_at=? WHERE id=?`, time.Now().UnixMilli(), threadID); err != nil {
		return err
	}
	return tx.Commit()
}
