package aichat

import (
	"context"
	"database/sql"
	"encoding/json"

	"github.com/elasticclaw/elasticclaw/pkg/hub/aichat/config"
	"github.com/elasticclaw/elasticclaw/pkg/hub/aichat/tools"
	"github.com/elasticclaw/elasticclaw/pkg/hub/aichat/tracker"
)

type savedTicketAction struct {
	draft   tools.TicketRequest
	card    TicketCard
	state   string
	unknown bool
}

// The durable send marker also covers a restart before the final card was saved.
// Callers hold the thread's Acquire slot while checking and creating a ticket.
func (s *Store) ticketActions(ctx context.Context, threadID, excludeMessageID string) ([]savedTicketAction, error) {
	rows, err := s.DB.QueryContext(ctx, `SELECT r.args_json,r.result_json,m.blocks_json
		FROM ai_chat_tool_runs r JOIN ai_chat_messages m ON m.id=r.message_id
		WHERE m.thread_id=? AND m.id<>? AND r.tool='ticket' ORDER BY m.seq DESC,r.seq DESC`, threadID, excludeMessageID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var actions []savedTicketAction
	for rows.Next() {
		var args, result, rawBlocks sql.NullString
		if err := rows.Scan(&args, &result, &rawBlocks); err != nil {
			return nil, err
		}
		var action savedTicketAction
		if err := json.Unmarshal([]byte(args.String), &action.draft); err != nil {
			return nil, err
		}
		var marker struct{ RequestSent bool }
		if result.Valid && result.String != "" {
			if err := json.Unmarshal([]byte(result.String), &marker); err != nil {
				return nil, err
			}
		}
		action.unknown = marker.RequestSent
		var blocks []tools.Block
		if rawBlocks.Valid && rawBlocks.String != "" {
			if err := json.Unmarshal([]byte(rawBlocks.String), &blocks); err != nil {
				return nil, err
			}
		}
		for _, block := range blocks {
			if block.Type != "action" || block.Kind != "ticket" {
				continue
			}
			if err := json.Unmarshal(block.Card, &action.card); err != nil {
				return nil, err
			}
			action.state = block.State
			if block.State == "created" || block.State == "failed" {
				action.unknown = action.card.OutcomeUnknown
			}
		}
		actions = append(actions, action)
	}
	return actions, rows.Err()
}

func (d Deps) ticketCreator(workspace string, cfg *config.Config) (tracker.Creator, tracker.Target, []string) {
	if cfg.IssueTracker == nil || d.LinearToken == nil {
		return nil, tracker.Target{}, nil
	}
	token, ok := d.LinearToken(workspace)
	if !ok || token == "" {
		return nil, tracker.Target{}, nil
	}
	fields := cfg.IssueTracker.DefaultFields
	return tracker.NewLinear(fields.Team, token, d.HTTPClient), tracker.LinearTarget(fields.Team, fields.State, fields.Labels), []string{token}
}

func (d Deps) availableActions(workspace string, cfg *config.Config) []string {
	actions := []string{"artifact"}
	if creator, _, _ := d.ticketCreator(workspace, cfg); creator != nil {
		actions = append(actions, "ticket")
	}
	return actions
}
