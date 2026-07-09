package hub

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/elasticclaw/elasticclaw/pkg/types"
	"nhooyr.io/websocket"
	"nhooyr.io/websocket/wsjson"
)

func TestHandleMessagesFiltersWakeMarkers(t *testing.T) {
	s, db := NewTestServerWithConfig(t, nil, "", "", "")
	_, err := db.Exec(
		`INSERT INTO claws(id, tenant_id, name, tags, created_at) VALUES(?,?,?,?,datetime('now'))`,
		"claw-1", "test-tenant-id", "claw 1", `[]`,
	)
	if err != nil {
		t.Fatal(err)
	}
	for _, msg := range []types.HubMessage{
		{ID: "wake-1", ClawID: "claw-1", TenantID: "test-tenant-id", Role: "system", Content: wakeMessageMarker, CreatedAt: now()},
		{ID: "plan-required-1", ClawID: "claw-1", TenantID: "test-tenant-id", Role: "system", Content: initialPlanRequiredMarker, CreatedAt: now()},
		{ID: "plan-accepted-1", ClawID: "claw-1", TenantID: "test-tenant-id", Role: "system", Content: initialPlanAcceptedMarker, CreatedAt: now()},
		{ID: "plan-correction-1", ClawID: "claw-1", TenantID: "test-tenant-id", Role: "system", Content: initialPlanCorrectionSentMarker, CreatedAt: now()},
		{ID: "user-1", ClawID: "claw-1", TenantID: "test-tenant-id", Role: "user", Content: "hello", CreatedAt: now()},
	} {
		_, err := db.Exec(
			`INSERT INTO messages(id,claw_id,tenant_id,role,content,created_at) VALUES(?,?,?,?,?,?)`,
			msg.ID, msg.ClawID, msg.TenantID, msg.Role, msg.Content, msg.CreatedAt,
		)
		if err != nil {
			t.Fatal(err)
		}
	}

	req := httptest.NewRequest(http.MethodGet, "/api/messages/claw-1", nil)
	req = req.WithContext(context.WithValue(req.Context(), ctxTenantKey{}, "test-tenant-id"))
	rec := httptest.NewRecorder()

	s.handleMessages(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected status %d, got %d", http.StatusOK, rec.Code)
	}
	var msgs []types.HubMessage
	if err := json.NewDecoder(rec.Body).Decode(&msgs); err != nil {
		t.Fatal(err)
	}
	if len(msgs) != 1 || msgs[0].ID != "user-1" {
		t.Fatalf("expected only user message, got %#v", msgs)
	}
}

func TestMessageTimelineSummarizesActivityWithoutCrowdingConversation(t *testing.T) {
	s, db := NewTestServerWithConfig(t, nil, "", "", "")
	_, err := db.Exec(
		`INSERT INTO claws(id, tenant_id, name, tags, created_at) VALUES(?,?,?,?,?)`,
		"claw-1", "test-tenant-id", "claw 1", `[]`, time.Date(2026, 6, 6, 12, 0, 0, 0, time.UTC),
	)
	if err != nil {
		t.Fatal(err)
	}
	base := time.Date(2026, 6, 6, 12, 0, 0, 0, time.UTC)
	insertMessage := func(id, role, content string, offsetSeconds int) {
		t.Helper()
		_, err := db.Exec(
			`INSERT INTO messages(id,claw_id,tenant_id,role,content,format,created_at) VALUES(?,?,?,?,?,?,?)`,
			id, "claw-1", "test-tenant-id", role, content, "", base.Add(time.Duration(offsetSeconds)*time.Second),
		)
		if err != nil {
			t.Fatal(err)
		}
	}
	insertMessage("user-1", "user", "start", 1)
	for i := 0; i < 120; i++ {
		insertMessage(fmt.Sprintf("activity-%03d", i), "activity", "tool", 2+i)
	}
	insertMessage("claw-1", "claw", "done", 200)

	req := httptest.NewRequest(http.MethodGet, "/api/messages/claw-1/timeline", nil)
	req = req.WithContext(context.WithValue(req.Context(), ctxTenantKey{}, "test-tenant-id"))
	rec := httptest.NewRecorder()

	s.handleMessages(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected status %d, got %d: %s", http.StatusOK, rec.Code, rec.Body.String())
	}
	var msgs []types.HubMessage
	if err := json.NewDecoder(rec.Body).Decode(&msgs); err != nil {
		t.Fatal(err)
	}
	if len(msgs) != 3 {
		t.Fatalf("timeline len = %d, want 3: %#v", len(msgs), msgs)
	}
	if msgs[0].ID != "user-1" || msgs[1].Role != "activity_summary" || msgs[2].ID != "claw-1" {
		t.Fatalf("timeline did not preserve conversation with summary: %#v", msgs)
	}
	if !strings.Contains(msgs[1].Format, `"count":120`) {
		t.Fatalf("summary format missing count: %s", msgs[1].Format)
	}
}

func TestMessageActivityEndpointExpandsSummaryRange(t *testing.T) {
	s, db := NewTestServerWithConfig(t, nil, "", "", "")
	_, err := db.Exec(
		`INSERT INTO claws(id, tenant_id, name, tags, created_at) VALUES(?,?,?,?,?)`,
		"claw-1", "test-tenant-id", "claw 1", `[]`, time.Date(2026, 6, 6, 12, 0, 0, 0, time.UTC),
	)
	if err != nil {
		t.Fatal(err)
	}
	base := time.Date(2026, 6, 6, 12, 0, 0, 0, time.UTC)
	for i := 0; i < 3; i++ {
		_, err := db.Exec(
			`INSERT INTO messages(id,claw_id,tenant_id,role,content,format,created_at) VALUES(?,?,?,?,?,?,?)`,
			fmt.Sprintf("activity-%d", i), "claw-1", "test-tenant-id", "activity", "tool", `activity:{"kind":"tool"}`, base.Add(time.Duration(i+1)*time.Second),
		)
		if err != nil {
			t.Fatal(err)
		}
	}

	req := httptest.NewRequest(http.MethodGet, "/api/messages/claw-1/activity?limit=2", nil)
	req = req.WithContext(context.WithValue(req.Context(), ctxTenantKey{}, "test-tenant-id"))
	rec := httptest.NewRecorder()

	s.handleMessages(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected status %d, got %d: %s", http.StatusOK, rec.Code, rec.Body.String())
	}
	var msgs []types.HubMessage
	if err := json.NewDecoder(rec.Body).Decode(&msgs); err != nil {
		t.Fatal(err)
	}
	if len(msgs) != 2 || msgs[0].ID != "activity-0" || msgs[1].ID != "activity-1" {
		t.Fatalf("activity messages = %#v, want first two", msgs)
	}
}

func TestMessageActivityEndpointCanReturnNewestActivities(t *testing.T) {
	s, db := NewTestServerWithConfig(t, nil, "", "", "")
	_, err := db.Exec(
		`INSERT INTO claws(id, tenant_id, name, tags, created_at) VALUES(?,?,?,?,?)`,
		"claw-1", "test-tenant-id", "claw 1", `[]`, time.Date(2026, 6, 6, 12, 0, 0, 0, time.UTC),
	)
	if err != nil {
		t.Fatal(err)
	}
	base := time.Date(2026, 6, 6, 12, 0, 0, 0, time.UTC)
	for i := 0; i < 6; i++ {
		_, err := db.Exec(
			`INSERT INTO messages(id,claw_id,tenant_id,role,content,format,created_at) VALUES(?,?,?,?,?,?,?)`,
			fmt.Sprintf("activity-%d", i), "claw-1", "test-tenant-id", "activity", "tool", `activity:{"kind":"tool"}`, base.Add(time.Duration(i+1)*time.Second),
		)
		if err != nil {
			t.Fatal(err)
		}
	}

	req := httptest.NewRequest(http.MethodGet, "/api/messages/claw-1/activity?limit=2&order=desc", nil)
	req = req.WithContext(context.WithValue(req.Context(), ctxTenantKey{}, "test-tenant-id"))
	rec := httptest.NewRecorder()

	s.handleMessages(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected status %d, got %d: %s", http.StatusOK, rec.Code, rec.Body.String())
	}
	var msgs []types.HubMessage
	if err := json.NewDecoder(rec.Body).Decode(&msgs); err != nil {
		t.Fatal(err)
	}
	if len(msgs) != 2 || msgs[0].ID != "activity-5" || msgs[1].ID != "activity-4" {
		t.Fatalf("activity messages = %#v, want newest two in descending order", msgs)
	}
}

func TestMessageTimelineIncludesActivityBeforeFirstConversationMessage(t *testing.T) {
	s, db := NewTestServerWithConfig(t, nil, "", "", "")
	_, err := db.Exec(
		`INSERT INTO claws(id, tenant_id, name, tags, created_at) VALUES(?,?,?,?,?)`,
		"claw-1", "test-tenant-id", "claw 1", `[]`, time.Date(2026, 6, 6, 12, 0, 0, 0, time.UTC),
	)
	if err != nil {
		t.Fatal(err)
	}
	base := time.Date(2026, 6, 6, 12, 0, 0, 0, time.UTC)
	for i := 0; i < 4; i++ {
		_, err := db.Exec(
			`INSERT INTO messages(id,claw_id,tenant_id,role,content,format,created_at) VALUES(?,?,?,?,?,?,?)`,
			fmt.Sprintf("activity-%d", i), "claw-1", "test-tenant-id", "activity", "tool", `activity:{"kind":"tool"}`, base.Add(time.Duration(i+1)*time.Second),
		)
		if err != nil {
			t.Fatal(err)
		}
	}
	_, err = db.Exec(
		`INSERT INTO messages(id,claw_id,tenant_id,role,content,format,created_at) VALUES(?,?,?,?,?,?,?)`,
		"hub-1", "claw-1", "test-tenant-id", "hub", "Injected proceed message", "", base.Add(10*time.Second),
	)
	if err != nil {
		t.Fatal(err)
	}

	req := httptest.NewRequest(http.MethodGet, "/api/messages/claw-1/timeline", nil)
	req = req.WithContext(context.WithValue(req.Context(), ctxTenantKey{}, "test-tenant-id"))
	rec := httptest.NewRecorder()

	s.handleMessages(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected status %d, got %d: %s", http.StatusOK, rec.Code, rec.Body.String())
	}
	var msgs []types.HubMessage
	if err := json.NewDecoder(rec.Body).Decode(&msgs); err != nil {
		t.Fatal(err)
	}
	if len(msgs) != 2 || msgs[0].Role != "activity_summary" || msgs[1].ID != "hub-1" {
		t.Fatalf("timeline = %#v, want pre-message activity summary then hub message", msgs)
	}
	meta := decodeActivitySummaryMeta(t, msgs[0])
	if meta.Count != 4 {
		t.Fatalf("pre-message summary count = %d, want 4", meta.Count)
	}
}

func TestMessageTimelinePreservesDisplayedStateAcrossActivityRuns(t *testing.T) {
	s, db := NewTestServerWithConfig(t, nil, "", "", "")
	_, err := db.Exec(
		`INSERT INTO claws(id, tenant_id, name, tags, created_at) VALUES(?,?,?,?,?)`,
		"claw-1", "test-tenant-id", "claw 1", `[]`, time.Date(2026, 6, 6, 12, 0, 0, 0, time.UTC),
	)
	if err != nil {
		t.Fatal(err)
	}
	base := time.Date(2026, 6, 6, 12, 0, 0, 0, time.UTC)
	offset := 0
	insertMessage := func(id, role, content, format string) {
		t.Helper()
		offset++
		_, err := db.Exec(
			`INSERT INTO messages(id,claw_id,tenant_id,role,content,format,created_at) VALUES(?,?,?,?,?,?,?)`,
			id, "claw-1", "test-tenant-id", role, content, format, base.Add(time.Duration(offset)*time.Second),
		)
		if err != nil {
			t.Fatal(err)
		}
	}
	insertActivityRun := func(prefix string, count int) {
		t.Helper()
		for i := 0; i < count; i++ {
			insertMessage(fmt.Sprintf("%s-%03d", prefix, i), "activity", "tool", `activity:{"kind":"tool","tool":"exec"}`)
		}
	}

	insertMessage("hub-1", "hub", "Injected proceed message", "")
	insertActivityRun("activity-a", 35)
	insertMessage("claw-1", "claw", "Assistant message 1", "")
	insertActivityRun("activity-b", 65)
	insertMessage("claw-2", "claw", "Assistant message 2", "")
	insertActivityRun("activity-c", 150)
	insertMessage("claw-3", "claw", "Assistant message 3", "")

	req := httptest.NewRequest(http.MethodGet, "/api/messages/claw-1/timeline", nil)
	req = req.WithContext(context.WithValue(req.Context(), ctxTenantKey{}, "test-tenant-id"))
	rec := httptest.NewRecorder()

	s.handleMessages(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected status %d, got %d: %s", http.StatusOK, rec.Code, rec.Body.String())
	}
	var timeline []types.HubMessage
	if err := json.NewDecoder(rec.Body).Decode(&timeline); err != nil {
		t.Fatal(err)
	}

	type expectedItem struct {
		role    string
		id      string
		content string
		count   int
	}
	want := []expectedItem{
		{role: "hub", id: "hub-1", content: "Injected proceed message"},
		{role: "activity_summary", count: 35},
		{role: "claw", id: "claw-1", content: "Assistant message 1"},
		{role: "activity_summary", count: 65},
		{role: "claw", id: "claw-2", content: "Assistant message 2"},
		{role: "activity_summary", count: 150},
		{role: "claw", id: "claw-3", content: "Assistant message 3"},
	}
	if len(timeline) != len(want) {
		t.Fatalf("timeline len = %d, want %d: %#v", len(timeline), len(want), timeline)
	}

	for i, wantItem := range want {
		got := timeline[i]
		if got.Role != wantItem.role {
			t.Fatalf("timeline[%d].role = %q, want %q; item=%#v", i, got.Role, wantItem.role, got)
		}
		if wantItem.id != "" && got.ID != wantItem.id {
			t.Fatalf("timeline[%d].id = %q, want %q", i, got.ID, wantItem.id)
		}
		if wantItem.content != "" && got.Content != wantItem.content {
			t.Fatalf("timeline[%d].content = %q, want %q", i, got.Content, wantItem.content)
		}
		if wantItem.count > 0 {
			meta := decodeActivitySummaryMeta(t, got)
			if meta.Count != wantItem.count {
				t.Fatalf("timeline[%d] summary count = %d, want %d", i, meta.Count, wantItem.count)
			}
			expanded := getActivityMessagesForSummary(t, s, got)
			if len(expanded) != wantItem.count {
				t.Fatalf("timeline[%d] expanded activity len = %d, want %d", i, len(expanded), wantItem.count)
			}
		}
	}
}

func TestStreamingSegmentsPersistAroundActivityForRefreshTimeline(t *testing.T) {
	s, db := NewTestServerWithConfig(t, nil, "", "", "")
	_, err := db.Exec(
		`INSERT INTO claws(id, tenant_id, name, tags, created_at) VALUES(?,?,?,?,?)`,
		"claw-1", "test-tenant-id", "claw 1", `[]`, time.Date(2026, 6, 6, 12, 0, 0, 0, time.UTC),
	)
	if err != nil {
		t.Fatal(err)
	}
	base := time.Date(2026, 6, 6, 12, 0, 0, 0, time.UTC)
	cc := &clawConn{ClawID: "claw-1", TenantID: "test-tenant-id"}
	persistSegment := func(id, content string, offsetSeconds int) {
		t.Helper()
		cc.Mu.Lock()
		cc.StreamingMsgID = id
		cc.StreamingBuf.WriteString(content)
		cc.Mu.Unlock()
		if err := s.flushStreamingSegment("claw-1", "test-tenant-id", cc); err != nil {
			t.Fatal(err)
		}
		_, err := db.Exec(`UPDATE messages SET created_at=? WHERE id=?`, base.Add(time.Duration(offsetSeconds)*time.Second), id)
		if err != nil {
			t.Fatal(err)
		}
	}
	insertActivity := func(id string, offsetSeconds int) {
		t.Helper()
		_, err := db.Exec(
			`INSERT INTO messages(id,claw_id,tenant_id,role,content,format,created_at) VALUES(?,?,?,?,?,?,?)`,
			id, "claw-1", "test-tenant-id", "activity", "exec", `activity:{"kind":"tool","tool":"exec"}`, base.Add(time.Duration(offsetSeconds)*time.Second),
		)
		if err != nil {
			t.Fatal(err)
		}
	}

	persistSegment("seg-1", "Assistant segment 1", 1)
	insertActivity("activity-1", 2)
	persistSegment("seg-2", "Assistant segment 2", 3)
	insertActivity("activity-2", 4)
	_, err = db.Exec(
		`INSERT INTO messages(id,claw_id,tenant_id,role,content,format,created_at) VALUES(?,?,?,?,?,?,?)`,
		"seg-3", "claw-1", "test-tenant-id", "claw", "Assistant segment 3", "", base.Add(5*time.Second),
	)
	if err != nil {
		t.Fatal(err)
	}

	req := httptest.NewRequest(http.MethodGet, "/api/messages/claw-1/timeline", nil)
	req = req.WithContext(context.WithValue(req.Context(), ctxTenantKey{}, "test-tenant-id"))
	rec := httptest.NewRecorder()

	s.handleMessages(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected status %d, got %d: %s", http.StatusOK, rec.Code, rec.Body.String())
	}
	var timeline []types.HubMessage
	if err := json.NewDecoder(rec.Body).Decode(&timeline); err != nil {
		t.Fatal(err)
	}
	if len(timeline) != 5 {
		t.Fatalf("timeline len = %d, want 5: %#v", len(timeline), timeline)
	}
	wantRoles := []string{"claw", "activity_summary", "claw", "activity_summary", "claw"}
	wantIDs := []string{"seg-1", "", "seg-2", "", "seg-3"}
	for i := range wantRoles {
		if timeline[i].Role != wantRoles[i] {
			t.Fatalf("timeline[%d].role = %q, want %q; timeline=%#v", i, timeline[i].Role, wantRoles[i], timeline)
		}
		if wantIDs[i] != "" && timeline[i].ID != wantIDs[i] {
			t.Fatalf("timeline[%d].id = %q, want %q", i, timeline[i].ID, wantIDs[i])
		}
		if timeline[i].Role == "activity_summary" {
			meta := decodeActivitySummaryMeta(t, timeline[i])
			if meta.Count != 1 {
				t.Fatalf("timeline[%d] activity count = %d, want 1", i, meta.Count)
			}
		}
	}
}

func TestSplitStreamingTurnDoesNotBroadcastGhostFinalMessage(t *testing.T) {
	ready := true
	clawID := "claw-ghost-final-message"
	s, db := NewTestServerWithConfig(t, &types.HubConfig{ClawToken: "claw-token"}, "", "", "")
	ts := httptest.NewServer(s.Handler())
	t.Cleanup(ts.Close)

	userCtx, cancelUser := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancelUser()
	userWS, _, err := websocket.Dial(userCtx, "ws"+strings.TrimPrefix(ts.URL, "http")+"/api/ws", &websocket.DialOptions{
		HTTPHeader: http.Header{"Authorization": {"Bearer test-token"}},
	})
	if err != nil {
		t.Fatalf("dial user ws: %v", err)
	}
	t.Cleanup(func() { _ = userWS.Close(websocket.StatusNormalClosure, "done") })

	clawCtx, cancelClaw := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancelClaw()
	clawWS, _, err := websocket.Dial(clawCtx, "ws"+strings.TrimPrefix(ts.URL, "http")+"/claw/ws", nil)
	if err != nil {
		t.Fatalf("dial claw ws: %v", err)
	}
	t.Cleanup(func() { _ = clawWS.Close(websocket.StatusNormalClosure, "done") })
	if err := wsjson.Write(clawCtx, clawWS, types.WSMessage{
		Type: "register",
		Payload: types.RegisterPayload{
			ClawID:       clawID,
			Name:         "claw 1",
			Template:     "elasticclaw",
			Token:        "claw-token",
			GatewayReady: &ready,
		},
	}); err != nil {
		t.Fatalf("register claw: %v", err)
	}
	var registered types.WSMessage
	if err := wsjson.Read(clawCtx, clawWS, &registered); err != nil {
		t.Fatalf("read registration ack: %v", err)
	}
	if registered.Type != "registered" {
		t.Fatalf("registration ack type = %q, want registered", registered.Type)
	}

	if err := wsjson.Write(clawCtx, clawWS, types.WSMessage{
		Type:    "chunk",
		Payload: map[string]string{"content": "Assistant segment 1"},
	}); err != nil {
		t.Fatalf("write chunk: %v", err)
	}
	if err := wsjson.Write(clawCtx, clawWS, types.WSMessage{
		Type: "agent_activity",
		Payload: map[string]interface{}{
			"kind":    "tool",
			"tool":    "exec",
			"command": "echo split",
		},
	}); err != nil {
		t.Fatalf("write activity: %v", err)
	}
	finalContent := "Assistant segment 1\nAssistant segment 2"
	if err := wsjson.Write(clawCtx, clawWS, types.WSMessage{
		Type: "message",
		Payload: types.HubMessage{
			Content: finalContent,
		},
	}); err != nil {
		t.Fatalf("write final message: %v", err)
	}

	seenIdle := false
	seenGhostMessage := false
	readUntil := time.Now().Add(2 * time.Second)
	for time.Now().Before(readUntil) && !seenIdle {
		readCtx, cancel := context.WithTimeout(context.Background(), 250*time.Millisecond)
		var msg types.WSMessage
		err := wsjson.Read(readCtx, userWS, &msg)
		cancel()
		if err != nil {
			continue
		}
		switch msg.Type {
		case "message":
			payload, _ := json.Marshal(msg.Payload)
			var hm types.HubMessage
			if err := json.Unmarshal(payload, &hm); err == nil && hm.Content == finalContent {
				seenGhostMessage = true
			}
		case "agent_typing":
			payload, _ := json.Marshal(msg.Payload)
			var typing struct {
				ClawID string `json:"claw_id"`
				Status string `json:"status"`
			}
			if err := json.Unmarshal(payload, &typing); err == nil && typing.ClawID == clawID && typing.Status == "idle" {
				seenIdle = true
			}
		}
	}
	if !seenIdle {
		t.Fatal("did not observe final idle typing event")
	}
	if seenGhostMessage {
		t.Fatal("observed unpersisted final full-response message over user websocket")
	}

	var finalRows int
	if err := db.QueryRow(`SELECT COUNT(*) FROM messages WHERE claw_id=? AND role='claw' AND content=?`, clawID, finalContent).Scan(&finalRows); err != nil {
		t.Fatal(err)
	}
	if finalRows != 0 {
		t.Fatalf("final full-response rows = %d, want 0", finalRows)
	}
	var segmentRows int
	if err := db.QueryRow(`SELECT COUNT(*) FROM messages WHERE claw_id=? AND role='claw' AND content=?`, clawID, "Assistant segment 1").Scan(&segmentRows); err != nil {
		t.Fatal(err)
	}
	if segmentRows != 1 {
		t.Fatalf("persisted segment rows = %d, want 1", segmentRows)
	}
}

func TestClawWSPipelineDoneTriggerTracksAnalytics(t *testing.T) {
	ready := true
	clawID := "claw-pipeline-done-ws"
	cfg := &types.HubConfig{
		Token:     "test-token",
		ClawToken: "claw-token",
		Factories: []*types.FactoryConfig{
			{
				Name:     "faster_apps",
				Template: "elasticclaw",
				PipelineYAML: `
stages:
  - id: working
    label: Working
    entry: true
  - id: android_validation
    label: Android Validation
    triggers:
      - message_contains: "[DONE]"
    on_enter:
      inject: "Android validation started"
`,
			},
		},
	}
	s, db := NewTestServerWithConfig(t, cfg, "", "", "")
	_, err := db.Exec(
		`INSERT INTO claws(id, tenant_id, name, template, status, tags, linear_issue_id, pipeline_stage, created_at) VALUES(?,?,?,?,?,?,?,?,datetime('now'))`,
		clawID, "test-tenant-id", "NEXT-257", "elasticclaw", "connected", `["factory:faster_apps"]`, "NEXT-257", "working",
	)
	if err != nil {
		t.Fatalf("insert claw: %v", err)
	}
	ts := httptest.NewServer(s.Handler())
	t.Cleanup(ts.Close)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	clawWS, _, err := websocket.Dial(ctx, "ws"+strings.TrimPrefix(ts.URL, "http")+"/claw/ws", nil)
	if err != nil {
		t.Fatalf("dial claw ws: %v", err)
	}
	t.Cleanup(func() { _ = clawWS.Close(websocket.StatusNormalClosure, "done") })
	if err := wsjson.Write(ctx, clawWS, types.WSMessage{
		Type: "register",
		Payload: types.RegisterPayload{
			ClawID:       clawID,
			Name:         "NEXT-257",
			Template:     "elasticclaw",
			Token:        "claw-token",
			GatewayReady: &ready,
		},
	}); err != nil {
		t.Fatalf("register claw: %v", err)
	}
	var registered types.WSMessage
	if err := wsjson.Read(ctx, clawWS, &registered); err != nil {
		t.Fatalf("read registration ack: %v", err)
	}
	if registered.Type != "registered" {
		t.Fatalf("registration ack type = %q, want registered", registered.Type)
	}
	if err := wsjson.Write(ctx, clawWS, types.WSMessage{
		Type: "message",
		Payload: types.HubMessage{
			Content: "[DONE]",
		},
	}); err != nil {
		t.Fatalf("write done message: %v", err)
	}

	var stage string
	var analyticsCount int
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		_ = db.QueryRow(`SELECT pipeline_stage FROM claws WHERE id=?`, clawID).Scan(&stage)
		_ = db.QueryRow(`SELECT COUNT(*) FROM factory_analytics WHERE claw_id=? AND issue_id=? AND factory_name=? AND action='done_signal'`, clawID, "NEXT-257", "factory:faster_apps").Scan(&analyticsCount)
		if stage == "android_validation" && analyticsCount == 1 {
			break
		}
		time.Sleep(25 * time.Millisecond)
	}
	if stage != "android_validation" {
		t.Fatalf("pipeline_stage = %q, want android_validation", stage)
	}
	if analyticsCount != 1 {
		t.Fatalf("done_signal analytics count = %d, want 1", analyticsCount)
	}

	var noPRWarnings int
	if err := db.QueryRow(`SELECT COUNT(*) FROM messages WHERE claw_id=? AND role='user' AND content LIKE '%no PR URLs%'`, clawID).Scan(&noPRWarnings); err != nil {
		t.Fatalf("count no-pr warnings: %v", err)
	}
	if noPRWarnings != 0 {
		t.Fatalf("expected no PR URL warning to be suppressed, got %d", noPRWarnings)
	}
}

func decodeActivitySummaryMeta(t *testing.T, msg types.HubMessage) activitySummaryMeta {
	t.Helper()
	if !strings.HasPrefix(msg.Format, "activity_summary:") {
		t.Fatalf("summary format = %q, want activity_summary prefix", msg.Format)
	}
	var meta activitySummaryMeta
	if err := json.Unmarshal([]byte(strings.TrimPrefix(msg.Format, "activity_summary:")), &meta); err != nil {
		t.Fatalf("decode summary meta: %v", err)
	}
	return meta
}

func getActivityMessagesForSummary(t *testing.T, s *Server, msg types.HubMessage) []types.HubMessage {
	t.Helper()
	meta := decodeActivitySummaryMeta(t, msg)
	req := httptest.NewRequest(http.MethodGet, fmt.Sprintf("/api/messages/%s/activity?from=%s&to=%s&limit=500", msg.ClawID, url.QueryEscape(meta.From), url.QueryEscape(meta.To)), nil)
	req = req.WithContext(context.WithValue(req.Context(), ctxTenantKey{}, msg.TenantID))
	rec := httptest.NewRecorder()

	s.handleMessages(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected status %d, got %d: %s", http.StatusOK, rec.Code, rec.Body.String())
	}
	var msgs []types.HubMessage
	if err := json.NewDecoder(rec.Body).Decode(&msgs); err != nil {
		t.Fatal(err)
	}
	return msgs
}
