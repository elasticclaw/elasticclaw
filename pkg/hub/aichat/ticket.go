package aichat

import (
	"context"
	"database/sql"
	"encoding/json"
	"regexp"
	"sync"

	"github.com/elasticclaw/elasticclaw/pkg/hub/aichat/config"
	"github.com/elasticclaw/elasticclaw/pkg/hub/aichat/tools"
	"github.com/elasticclaw/elasticclaw/pkg/hub/aichat/tracker"
)

type savedTicketAction struct {
	actionKey        string
	attemptID        string
	confirmedUnknown string
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
		var marker struct{ RequestSent, Dismissed bool }
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
		if marker.Dismissed {
			action.card.Dismissed = true
		}
		action.unknown = action.unknown && !action.card.Dismissed
		action.attemptID = action.card.AttemptID
		if action.attemptID == "" {
			action.attemptID = messageID
		}
		actions = append(actions, action)
	}
	return actions, rows.Err()
}

// A turn uses one workspace token snapshot; the next request resolves it anew.
type ticketTokenKey struct{}
type ticketToken struct {
	once  sync.Once
	value string
	ok    bool
}

func withTicketToken(ctx context.Context) context.Context {
	if ctx.Value(ticketTokenKey{}) != nil {
		return ctx
	}
	return context.WithValue(ctx, ticketTokenKey{}, &ticketToken{})
}

func (d Deps) linearToken(ctx context.Context, workspace string) (string, bool) {
	if d.LinearToken == nil {
		return "", false
	}
	if token, ok := ctx.Value(ticketTokenKey{}).(*ticketToken); ok {
		token.once.Do(func() { token.value, token.ok = d.LinearToken(workspace) })
		return token.value, token.ok
	}
	return d.LinearToken(workspace)
}

func (d Deps) ticketCreator(ctx context.Context, workspace string, cfg *config.Config) (tracker.Creator, tracker.Target, []string) {
	if cfg.IssueTracker == nil || d.LinearToken == nil {
		return nil, tracker.Target{}, nil
	}
	token, ok := d.linearToken(ctx, workspace)
	if !ok || token == "" {
		return nil, tracker.Target{}, nil
	}
	fields := cfg.IssueTracker.DefaultFields
	return tracker.NewLinear(fields.Team, token, d.HTTPClient), tracker.LinearTarget(fields.Team, fields.State, fields.Labels), []string{token}
}

func (d Deps) availableActions(ctx context.Context, workspace string, cfg *config.Config) []string {
	actions := []string{"artifact"}
	if creator, _, _ := d.ticketCreator(ctx, workspace, cfg); creator != nil && !d.health.tokenRejected(sourceHealthKey(workspace, cfg, "issue_tracker")) {
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

// Dismissal resolves the original action and invalidates confirmations from stale cards.
func dismissedTicketAction(previous []savedTicketAction, key, confirmUnknown string) *savedTicketAction {
	for i := range previous {
		if previous[i].card.Dismissed && (key != "" && previous[i].actionKey == key || confirmUnknown != "" && previous[i].attemptID == confirmUnknown) {
			return &previous[i]
		}
	}
	return nil
}

// Replayed cards do not resolve unknown attempts. Only an explicit confirmation
// or dismissal of that attempt does; a later unknown attempt needs its own check.
func unknownTicketAction(previous []savedTicketAction) *savedTicketAction {
	confirmed := map[string]bool{}
	for _, action := range previous {
		if action.card.Dismissed {
			confirmed[action.attemptID] = true
		}
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
		candidate := cache.prepare(ctx, sourceHealthKey(workspace, cfg, source.Kind), func() sourceCandidate { return d.candidate(ctx, workspace, cfg, source) })
		secrets = append(secrets, candidate.secrets...)
	}
	return secrets
}
