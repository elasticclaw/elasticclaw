// HTTP handlers and queries for claw conversation messages and activity.
//
// Split out of the former server.go; same package, no behavior changes.
package hub

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/elasticclaw/elasticclaw/pkg/hub/store"
	"github.com/elasticclaw/elasticclaw/pkg/types"
	"github.com/google/uuid"
	"nhooyr.io/websocket/wsjson"
)

func (s *Server) handleMessages(w http.ResponseWriter, r *http.Request) {
	tenantID := tenantFromCtx(r)
	path := strings.Trim(strings.TrimPrefix(r.URL.Path, "/api/messages/"), "/")
	parts := strings.Split(path, "/")
	clawID := parts[0]
	if clawID == "" {
		writeErr(w, http.StatusNotFound, "not_found", "not found")
		return
	}
	if len(parts) > 1 {
		switch parts[1] {
		case "timeline":
			s.handleMessageTimeline(w, r, tenantID, clawID)
		case "activity":
			s.handleMessageActivity(w, r, tenantID, clawID)
		default:
			writeErr(w, http.StatusNotFound, "not_found", "not found")
		}
		return
	}
	ghLoginMsg := githubLoginFromContext(r.Context())
	var accessCfgMsg *types.AccessConfig
	if ghLoginMsg != "" {
		s.mu.RLock()
		if s.hubCfg.Auth != nil {
			accessCfgMsg = s.hubCfg.Auth.Access
		}
		s.mu.RUnlock()
	}

	if r.Method == http.MethodPost {
		var body struct {
			Content string `json:"content"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil || body.Content == "" {
			writeErr(w, http.StatusBadRequest, "bad_request", "invalid request")
			return
		}

		// Apply tag-based interact filter for GitHub OAuth users
		if ghLoginMsg != "" {
			// Fetch claw tags to check interact permission
			clawTagsMsg, err := s.st().Claws().Tags(r.Context(), clawID, tenantID)
			if err != nil {
				if err == sql.ErrNoRows {
					writeErr(w, http.StatusNotFound, "not_found", "not found")
				} else {
					writeErr(w, http.StatusInternalServerError, "internal", "db error")
				}
				return
			}
			if !canInteractWithClaw(accessCfgMsg, ghLoginMsg, clawTagsMsg) {
				writeErr(w, http.StatusForbidden, "forbidden", "forbidden")
				return
			}
		}

		msg := types.HubMessage{
			ID: uuid.New().String(), ClawID: clawID, TenantID: tenantID,
			Role: "user", Content: body.Content, CreatedAt: now(),
		}
		if err := s.st().Messages().Insert(r.Context(), msg); err != nil {
			writeErr(w, http.StatusInternalServerError, "internal", "db error")
			return
		}
		s.recordTaskRunDashboardMessage(clawID, ghLoginMsg, msg.ID)
		// Forward to claw if connected (or queue if busy)
		s.mu.RLock()
		cc := s.claws[clawID]
		s.mu.RUnlock()
		if cc != nil {
			cc.Mu.Lock()
			cc.LastUserMessageAt = time.Now()
			// Check if claw is currently streaming/processing
			isBusy := !cc.StreamingStartedAt.IsZero() || cc.StreamingMsgID != ""
			if isBusy {
				// Queue the message for later delivery
				cc.MessageQueue = append(cc.MessageQueue, msg)
				queueLen := len(cc.MessageQueue)
				cc.Mu.Unlock()
				logfCtx(r.Context(), "[hub] message queued for %s (queue length: %d)", clawID[:8], queueLen)
			} else {
				cc.Mu.Unlock()
				// Send immediately
				_ = wsjson.Write(r.Context(), cc.WS, types.WSMessage{Type: "message", Payload: msg})
				s.metrics.wsMessage("out", "claw")
				// Immediately signal to UI that agent is working, before first chunk arrives
				s.broadcastToUsers(tenantID, types.WSMessage{
					Type: "agent_typing",
					Payload: map[string]string{
						"claw_id": clawID,
						"status":  "typing",
					},
				})
			}
		}
		jsonOK(w, msg)
		return
	}
	if ghLoginMsg != "" {
		clawTagsMsg, err := s.st().Claws().Tags(r.Context(), clawID, tenantID)
		if err == sql.ErrNoRows {
			writeErr(w, http.StatusNotFound, "not_found", "not found")
			return
		}
		if !canViewClaw(accessCfgMsg, ghLoginMsg, clawTagsMsg) {
			writeErr(w, http.StatusForbidden, "forbidden", "forbidden")
			return
		}
	}

	// Pagination: ?before=<created_at>&limit=<n> for older messages
	// ?after=<created_at>&limit=<n> for newer messages
	// Default: last 100 messages
	const defaultLimit = 100
	limit := defaultLimit
	before := r.URL.Query().Get("before") // ISO timestamp — return messages older than this
	after := r.URL.Query().Get("after")   // ISO timestamp — return messages newer than this

	msgs, err := s.st().Messages().ListVisible(r.Context(), store.VisibleWindow{
		ClawID:               clawID,
		TenantID:             tenantID,
		Before:               before,
		After:                after,
		Limit:                limit,
		HiddenSystemContents: hiddenSystemMessagesContents(),
	})
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "internal", "db error")
		return
	}
	// Reverse DESC results to get ASC order
	if before != "" || (before == "" && after == "") {
		for i, j := 0, len(msgs)-1; i < j; i, j = i+1, j-1 {
			msgs[i], msgs[j] = msgs[j], msgs[i]
		}
	}
	if msgs == nil {
		msgs = []types.HubMessage{}
	}
	jsonOK(w, msgs)
}

type activitySummaryMeta struct {
	Count int    `json:"count"`
	From  string `json:"from"`
	To    string `json:"to,omitempty"`
}

// hiddenSystemMessagesContents lists the system-role marker messages the
// UI never shows (wake and initial-plan markers). The store excludes
// them from every conversation page.
func hiddenSystemMessagesContents() []string {
	return []string{
		wakeMessageMarker,
		defaultWakeContent,
		initialPlanWakeContent,
		initialPlanRequiredMarker,
		initialPlanAcceptedMarker,
		initialPlanCorrectionSentMarker,
	}
}

func (s *Server) handleMessageTimeline(w http.ResponseWriter, r *http.Request, tenantID, clawID string) {
	if r.Method != http.MethodGet {
		writeErr(w, http.StatusMethodNotAllowed, "method_not_allowed", "method not allowed")
		return
	}
	if !s.canViewMessages(w, r, tenantID, clawID) {
		return
	}

	limit := parsePositiveLimit(r, 50, 100)
	before := r.URL.Query().Get("before")
	rows, err := s.queryConversationMessages(clawID, tenantID, before, limit)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "internal", "db error")
		return
	}

	if len(rows) == 0 {
		summary, err := s.activitySummary(clawID, tenantID, nil, parseTimeCursor(before), "", before)
		if err != nil {
			writeErr(w, http.StatusInternalServerError, "internal", "db error")
			return
		}
		if summary == nil {
			jsonOK(w, []types.HubMessage{})
			return
		}
		jsonOK(w, []types.HubMessage{*summary})
		return
	}

	timeline := make([]types.HubMessage, 0, len(rows)*2)
	firstCreated := rows[0].CreatedAt
	hasOlderConversation, err := s.hasConversationBefore(clawID, tenantID, firstCreated)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "internal", "db error")
		return
	}
	if !hasOlderConversation {
		firstCursor := firstCreated.Format(time.RFC3339Nano)
		summary, err := s.activitySummary(clawID, tenantID, nil, &firstCreated, "", firstCursor)
		if err != nil {
			writeErr(w, http.StatusInternalServerError, "internal", "db error")
			return
		}
		if summary != nil {
			timeline = append(timeline, *summary)
		}
	}
	for i, msg := range rows {
		timeline = append(timeline, msg)
		lower := msg.CreatedAt
		lowerCursor := lower.Format(time.RFC3339Nano)
		var upper *time.Time
		upperCursor := ""
		if i+1 < len(rows) {
			nextCreated := rows[i+1].CreatedAt
			upper = &nextCreated
			upperCursor = nextCreated.Format(time.RFC3339Nano)
		} else if before != "" {
			upper = parseTimeCursor(before)
			upperCursor = before
		}
		summary, err := s.activitySummary(clawID, tenantID, &lower, upper, lowerCursor, upperCursor)
		if err != nil {
			writeErr(w, http.StatusInternalServerError, "internal", "db error")
			return
		}
		if summary != nil {
			timeline = append(timeline, *summary)
		}
	}
	jsonOK(w, timeline)
}

func (s *Server) handleMessageActivity(w http.ResponseWriter, r *http.Request, tenantID, clawID string) {
	if r.Method != http.MethodGet {
		writeErr(w, http.StatusMethodNotAllowed, "method_not_allowed", "method not allowed")
		return
	}
	if !s.canViewMessages(w, r, tenantID, clawID) {
		return
	}

	from := r.URL.Query().Get("from")
	to := r.URL.Query().Get("to")
	before := r.URL.Query().Get("before")
	limit := parsePositiveLimit(r, 200, 500)
	order := strings.ToLower(r.URL.Query().Get("order"))
	if order != "desc" {
		order = "asc"
	}

	msgs, err := s.st().Messages().ListActivity(r.Context(), store.ActivityWindow{
		ClawID:   clawID,
		TenantID: tenantID,
		From:     cursorValue(from),
		To:       cursorValue(to),
		Before:   cursorValue(before),
		Limit:    limit,
		Desc:     order == "desc",
	})
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "internal", "db error")
		return
	}
	if msgs == nil {
		msgs = []types.HubMessage{}
	}
	jsonOK(w, msgs)
}

func parsePositiveLimit(r *http.Request, def, max int) int {
	limit := def
	if raw := r.URL.Query().Get("limit"); raw != "" {
		if parsed, err := strconv.Atoi(raw); err == nil && parsed > 0 {
			limit = parsed
		}
	}
	if limit > max {
		return max
	}
	return limit
}

func parseTimeCursor(raw string) *time.Time {
	if raw == "" {
		return nil
	}
	if parsed, err := time.Parse(time.RFC3339Nano, raw); err == nil {
		return &parsed
	}
	if parsed, err := time.Parse(time.RFC3339, raw); err == nil {
		return &parsed
	}
	return nil
}

// cursorValue converts a raw query-string cursor to the value bound in
// the SQL comparison: the parsed time when it is RFC3339, the raw string
// otherwise, nil when absent (pre-extraction behavior).
func cursorValue(raw string) any {
	if raw == "" {
		return nil
	}
	if parsed := parseTimeCursor(raw); parsed != nil {
		return *parsed
	}
	return raw
}

func (s *Server) queryConversationMessages(clawID, tenantID, before string, limit int) ([]types.HubMessage, error) {
	msgs, err := s.st().Messages().ListConversation(context.Background(), clawID, tenantID, cursorValue(before), limit, hiddenSystemMessagesContents())
	if err != nil {
		return nil, err
	}
	for i, j := 0, len(msgs)-1; i < j; i, j = i+1, j-1 {
		msgs[i], msgs[j] = msgs[j], msgs[i]
	}
	return msgs, nil
}

func (s *Server) hasConversationBefore(clawID, tenantID string, before time.Time) (bool, error) {
	return s.st().Messages().HasConversationBefore(context.Background(), clawID, tenantID, before, hiddenSystemMessagesContents())
}

func (s *Server) activitySummary(clawID, tenantID string, from, to *time.Time, fromCursor, toCursor string) (*types.HubMessage, error) {
	count, _, maxCreated, err := s.st().Messages().ActivityStats(context.Background(), clawID, tenantID, from, to)
	if err != nil {
		return nil, err
	}
	if count == 0 {
		return nil, nil
	}
	meta := activitySummaryMeta{Count: count, From: fromCursor, To: toCursor}
	data, err := json.Marshal(meta)
	if err != nil {
		return nil, err
	}
	createdAt := now()
	if maxCreated != "" {
		if parsed, err := time.Parse(time.RFC3339Nano, maxCreated); err == nil {
			createdAt = parsed
		}
	}
	return &types.HubMessage{
		ID:        "activity-summary-" + uuid.NewSHA1(uuid.NameSpaceOID, []byte(clawID+"|"+fromCursor+"|"+toCursor)).String(),
		ClawID:    clawID,
		TenantID:  tenantID,
		Role:      "activity_summary",
		Content:   fmt.Sprintf("%d tool calls", count),
		Format:    "activity_summary:" + string(data),
		CreatedAt: createdAt,
	}, nil
}

func (s *Server) canViewMessages(w http.ResponseWriter, r *http.Request, tenantID, clawID string) bool {
	ghLoginMsg := githubLoginFromContext(r.Context())
	if ghLoginMsg == "" {
		return true
	}
	var accessCfgMsg *types.AccessConfig
	s.mu.RLock()
	if s.hubCfg.Auth != nil {
		accessCfgMsg = s.hubCfg.Auth.Access
	}
	s.mu.RUnlock()

	clawTagsMsg, err := s.st().Claws().Tags(r.Context(), clawID, tenantID)
	if err == sql.ErrNoRows {
		http.Error(w, "not found", http.StatusNotFound)
		return false
	}
	if err != nil {
		http.Error(w, "db error", http.StatusInternalServerError)
		return false
	}
	if !canViewClaw(accessCfgMsg, ghLoginMsg, clawTagsMsg) {
		http.Error(w, "forbidden", http.StatusForbidden)
		return false
	}
	return true
}
