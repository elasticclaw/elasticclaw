package aichat

import (
	"context"
	"database/sql"
	"encoding/json"
	"regexp"

	"github.com/elasticclaw/elasticclaw/pkg/hub/aichat/config"
	"github.com/elasticclaw/elasticclaw/pkg/hub/aichat/tools"
	"github.com/elasticclaw/elasticclaw/pkg/hub/aichat/tracker"
)

type savedTicketAction struct {
	actionKey        string
	attemptID        string
	confirmedUnknown string
	draft            tools.TicketRequest
	card             TicketCard
	state            string
	unknown          bool
}

// The durable send marker also covers a restart before the final card was saved.
// Callers hold the thread's Acquire slot while checking and creating a ticket.
func (s *Store) ticketActions(ctx context.Context, threadID, excludeMessageID string) ([]savedTicketAction, error) {
	rows, err := s.DB.QueryContext(ctx, `SELECT m.id,r.args_json,r.result_json,m.blocks_json
		FROM ai_chat_tool_runs r JOIN ai_chat_messages m ON m.id=r.message_id
		WHERE m.thread_id=? AND m.id<>? AND r.tool='ticket' AND r.provider='AI Chat' ORDER BY m.seq DESC,r.seq DESC`, threadID, excludeMessageID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var actions []savedTicketAction
rowsLoop:
	for rows.Next() {
		var args, result, rawBlocks sql.NullString
		var messageID string
		if err := rows.Scan(&messageID, &args, &result, &rawBlocks); err != nil {
			return nil, err
		}
		var action savedTicketAction
		var argsCard TicketCard
		if json.Unmarshal([]byte(args.String), &argsCard) != nil {
			continue
		}
		action.actionKey = argsCard.ActionKey
		action.confirmedUnknown = argsCard.ConfirmedUnknown
		action.card = argsCard
		action.draft = argsCard.TicketRequest
		var marker struct{ RequestSent bool }
		if result.Valid && result.String != "" {
			if err := json.Unmarshal([]byte(result.String), &marker); err != nil {
				continue rowsLoop
			}
		}
		action.unknown = marker.RequestSent
		var blocks []tools.Block
		if rawBlocks.Valid && rawBlocks.String != "" {
			if err := json.Unmarshal([]byte(rawBlocks.String), &blocks); err != nil {
				continue rowsLoop
			}
		}
		for _, block := range blocks {
			if block.Type != "action" || block.Kind != "ticket" {
				continue
			}
			if err := json.Unmarshal(block.Card, &action.card); err != nil {
				continue rowsLoop
			}
			action.state = block.State
			if block.State == "created" || block.State == "failed" {
				action.unknown = action.card.OutcomeUnknown
			}
		}
		action.attemptID = action.card.AttemptID
		if action.attemptID == "" {
			action.attemptID = messageID
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
	if creator, _, _ := d.ticketCreator(workspace, cfg); creator != nil && !d.health.tokenRejected(sourceHealthKey(workspace, cfg, "issue_tracker")) {
		actions = append(actions, "ticket")
	}
	return actions
}

var ticketActionKey = regexp.MustCompile(`^[A-Za-z0-9-]{1,64}$`)

func createdTicketAction(previous []savedTicketAction, key string) *savedTicketAction {
	if key == "" {
		return nil
	}
	for i := range previous {
		if previous[i].actionKey == key && previous[i].state == "created" {
			return &previous[i]
		}
	}
	return nil
}

// Replayed cards do not resolve unknown attempts. Only an explicit confirmation
// of that attempt does; a later unknown attempt needs its own confirmation.
func unknownTicketAction(previous []savedTicketAction) *savedTicketAction {
	confirmed := map[string]bool{}
	for _, action := range previous {
		if action.confirmedUnknown != "" {
			confirmed[action.confirmedUnknown] = true
		}
	}
	for i := range previous {
		if previous[i].unknown && !confirmed[previous[i].attemptID] {
			return &previous[i]
		}
	}
	return nil
}

// Ticket actions need source credentials for redaction, without health requests
// that would contact the tracker again when replaying a created action.
func (d Deps) ticketSecrets(ctx context.Context, workspace string, cfg *config.Config) []string {
	ctx, cancel := context.WithTimeout(ctx, healthTimeout)
	defer cancel()
	cache := d.health
	if cache == nil {
		cache = newSourceHealth()
	}
	var secrets []string
	for _, source := range cfg.Sources {
		if source.Status == "invalid" {
			continue
		}
		candidate := cache.prepare(ctx, sourceHealthKey(workspace, cfg, source.Kind), func() sourceCandidate { return d.candidate(workspace, cfg, source) })
		secrets = append(secrets, candidate.secrets...)
	}
	return secrets
}
