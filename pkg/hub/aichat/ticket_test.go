package aichat

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/elasticclaw/elasticclaw/pkg/hub/aichat/config"
	"github.com/elasticclaw/elasticclaw/pkg/hub/aichat/llm"
	"github.com/elasticclaw/elasticclaw/pkg/hub/aichat/tools"
	"github.com/elasticclaw/elasticclaw/pkg/hub/aichat/tracker"
)

const ticketSecret = "sentinel-ticket-token"
const ticketYAML = `about: Test
issue_tracker:
  name: product
  provider: linear
  default_fields:
    team: Product
    state: Backlog
    labels: [ai-chat]
`

func ticketHTTP(t *testing.T, create func(http.ResponseWriter, *http.Request, map[string]any)) *http.Client {
	t.Helper()
	return sourceClient(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		if strings.Contains(string(raw), ticketSecret) {
			t.Error("secret in provider body")
		}
		var req struct {
			Query     string
			Variables map[string]any
		}
		if err := json.Unmarshal(raw, &req); err != nil {
			t.Error(err)
			return
		}
		switch {
		case strings.Contains(req.Query, "issueCreate("):
			create(w, r, req.Variables["input"].(map[string]any))
		case strings.Contains(req.Query, "workflowStates("):
			fmt.Fprint(w, `{"data":{"workflowStates":{"nodes":[{"id":"state","name":"Backlog"}]}}}`)
		case strings.Contains(req.Query, "issueLabels("):
			fmt.Fprint(w, `{"data":{"issueLabels":{"nodes":[{"id":"label","name":"ai-chat"}]}}}`)
		case strings.Contains(req.Query, "teams("):
			fmt.Fprint(w, `{"data":{"teams":{"nodes":[{"id":"team","name":"Product"}]}}}`)
		default:
			t.Errorf("unexpected query %s", req.Query)
		}
	})
}
func ticketSuccess(w http.ResponseWriter, title string) {
	_ = json.NewEncoder(w).Encode(map[string]any{"data": map[string]any{"issueCreate": map[string]any{"success": true, "issue": map[string]any{"identifier": "PRO-1423", "url": "https://linear.app/PRO-1423", "title": title, "state": map[string]string{"name": "Backlog"}}}}})
}
func ticketFixture(t *testing.T, p llm.Provider, client *http.Client, configure ...func(*Deps)) (http.Handler, *Store) {
	t.Helper()
	h, s, _ := apiFixture(t, p, func(d *Deps) {
		if err := os.WriteFile(filepath.Join(d.ManagedDir("workspace"), "ai_chat.yaml"), []byte(ticketYAML), 0600); err != nil {
			t.Fatal(err)
		}
		d.LinearToken = func(string) (string, bool) { return ticketSecret, true }
		d.HTTPClient = client
		d.PublicURL = func() string { return "https://hub.test/" }
		for _, f := range configure {
			f(d)
		}
	})
	return h, s
}
func latestTicket(t *testing.T, s *Store, thread Thread) (Message, tools.Block, TicketCard) {
	t.Helper()
	messages, err := s.Messages(context.Background(), thread.ID, 0)
	if err != nil || len(messages) == 0 {
		t.Fatal(err)
	}
	m := messages[len(messages)-1]
	var blocks []tools.Block
	_ = json.Unmarshal(m.Blocks, &blocks)
	for _, b := range blocks {
		if b.Kind == "ticket" {
			var card TicketCard
			if err := json.Unmarshal(b.Card, &card); err != nil {
				t.Fatal(err)
			}
			return m, b, card
		}
	}
	t.Fatalf("missing ticket in %+v", m)
	return Message{}, tools.Block{}, TicketCard{}
}
func addTicketArtifact(t *testing.T, s *Store, thread Thread, title string) Artifact {
	t.Helper()
	ctx := context.Background()
	m, err := s.beginAction(ctx, thread, "fake", "")
	if err != nil {
		t.Fatal(err)
	}
	a, err := s.publishArtifact(ctx, t.TempDir(), thread, &m, tools.ArtifactRequest{Title: title}, testHTML)
	if err != nil {
		t.Fatal(err)
	}
	m.Status = "completed"
	if err = s.FinishMessage(ctx, m); err != nil {
		t.Fatal(err)
	}
	return a
}

func TestTicketActionDraftArtifactRecordsAndRedaction(t *testing.T) {
	drafts, writes := 0, 0
	p := fakeProvider{stream: func(_ context.Context, req llm.Request, emit func(string) error) (llm.Response, error) {
		drafts++
		if len(req.Tools) != 0 || req.MaxTokens == 0 || strings.Contains(fmt.Sprint(req), ticketSecret) {
			t.Fatal("unsafe drafting request")
		}
		if drafts == 1 {
			return llm.Response{Text: "not JSON"}, nil
		}
		return llm.Response{Text: `{"title":"Swap ` + ticketSecret + `","body":"Clients ask 31 times a week. ` + ticketSecret + `\n\nEvidence: 12% found swap."}`, InputTokens: 7, OutputTokens: 9}, nil
	}}
	var input map[string]any
	h, s := ticketFixture(t, p, ticketHTTP(t, func(w http.ResponseWriter, _ *http.Request, v map[string]any) {
		writes++
		input = v
		ticketSuccess(w, v["title"].(string))
	}))
	thread := testThread(t, s)
	old := addTicketArtifact(t, s, thread, "Old")
	artifact := addTicketArtifact(t, s, thread, "Prototype")
	// Make ordering deterministic even on a coarse system clock.
	_, _ = s.DB.Exec(`UPDATE ai_chat_artifacts SET updated_at=updated_at-1000 WHERE id=?`, old.ID)
	rec := request(h, "POST", "/api/ai-chat/threads/"+thread.ID+"/actions", `{"kind":"ticket"}`, "octocat", "on")
	if rec.Code != 200 || drafts != 2 || writes != 1 {
		t.Fatalf("%d %s drafts=%d writes=%d", rec.Code, rec.Body, drafts, writes)
	}
	expectedBody := "Clients ask 31 times a week. [redacted]\n\nEvidence: 12% found swap.\n\nArtifact: Prototype — https://hub.test/artifacts/" + artifact.Slug + "\n\n---\nFrom AI Chat · octocat"
	want := map[string]any{"teamId": "team", "title": "Swap [redacted]", "description": expectedBody, "stateId": "state", "labelIds": []any{"label"}}
	if !reflect.DeepEqual(input, want) {
		t.Fatalf("input %#v", input)
	}
	m, b, card := latestTicket(t, s, thread)
	if b.State != "created" || card.ArtifactID != artifact.ID || card.Identifier != "PRO-1423" || card.Summary != "Clients ask 31 times a week. [redacted]" || m.InputTokens != 7 || m.OutputTokens != 9 {
		t.Fatal(m, b, card)
	}
	if !strings.Contains(m.Content, "Product backlog with the artifact linked") || !strings.Contains(historyContent(m), "[Linear ticket PRO-1423 created: https://linear.app/PRO-1423]") {
		t.Fatal(m.Content, historyContent(m))
	}
	if strings.Contains(card.Body, "From AI Chat") {
		t.Fatal("draft contains server footer")
	}
	var provider, ref, url, payload, summary string
	if err := s.DB.QueryRow(`SELECT provider,external_ref,url,payload_json FROM ai_chat_handoffs WHERE message_id=?`, m.ID).Scan(&provider, &ref, &url, &payload); err != nil {
		t.Fatal(err)
	}
	if provider != "linear" || ref != "PRO-1423" || url != card.URL || !strings.Contains(payload, artifact.Slug) {
		t.Fatal(provider, ref, url, payload)
	}
	if err := s.DB.QueryRow(`SELECT summary FROM ai_chat_tool_runs WHERE message_id=? AND tool='ticket'`, m.ID).Scan(&summary); err != nil || summary != "Opened PRO-1423" {
		t.Fatal(summary, err)
	}
	loaded := request(h, "GET", "/api/ai-chat/threads/"+thread.ID, "", "octocat", "on")
	if !strings.Contains(loaded.Body.String(), "PRO-1423") || strings.Contains(loaded.Body.String()+rec.Body.String()+payload, ticketSecret) {
		t.Fatal("reload or redaction")
	}
	rows, err := s.DB.Query(`SELECT COALESCE(args_json,'')||COALESCE(result_json,'')||COALESCE(error,'')||COALESCE(summary,'') FROM ai_chat_tool_runs`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	for rows.Next() {
		var raw string
		_ = rows.Scan(&raw)
		if strings.Contains(raw, ticketSecret) {
			t.Fatal("secret persisted")
		}
	}
}

func TestTicketFailureRetryPreservesDraft(t *testing.T) {
	drafts, writes := 0, 0
	p := fakeProvider{stream: func(context.Context, llm.Request, func(string) error) (llm.Response, error) {
		drafts++
		return llm.Response{Text: `{"title":"Swap","body":"31 requests a week."}`}, nil
	}}
	var sent []string
	h, s := ticketFixture(t, p, ticketHTTP(t, func(w http.ResponseWriter, _ *http.Request, v map[string]any) {
		writes++
		sent = append(sent, v["description"].(string))
		if writes == 1 {
			w.WriteHeader(403)
			fmt.Fprint(w, ticketSecret)
			return
		}
		ticketSuccess(w, "Swap")
	}))
	thread := testThread(t, s)
	a := addTicketArtifact(t, s, thread, "Prototype")
	rec := request(h, "POST", "/api/ai-chat/threads/"+thread.ID+"/actions", `{"kind":"ticket","artifactId":"`+a.ID+`"}`, "octocat", "on")
	if rec.Code != 200 {
		t.Fatal(rec.Body)
	}
	m, b, card := latestTicket(t, s, thread)
	if b.State != "failed" || card.Title != "Swap" || card.Body != "31 requests a week." || card.ArtifactID != a.ID || !strings.Contains(historyContent(m), "[Ticket failed:") {
		t.Fatal(card)
	}
	body := string(redactedJSON(map[string]any{"kind": "ticket", "title": card.Title, "body": card.Body, "artifactId": card.ArtifactID}, nil))
	rec = request(h, "POST", "/api/ai-chat/threads/"+thread.ID+"/actions", body, "octocat", "on")
	_, b, _ = latestTicket(t, s, thread)
	if rec.Code != 200 || b.State != "created" || drafts != 1 || writes != 2 || sent[0] != sent[1] {
		t.Fatal(rec.Body, b, drafts, writes, sent)
	}
}

func TestTicketDraftFailureAndAvailability(t *testing.T) {
	for _, mode := range []string{"no tracker", "no token", "invalid JSON"} {
		t.Run(mode, func(t *testing.T) {
			calls := 0
			p := fakeProvider{stream: func(context.Context, llm.Request, func(string) error) (llm.Response, error) {
				calls++
				return llm.Response{Text: `{"title":"only"}`}, nil
			}}
			h, s := ticketFixture(t, p, ticketHTTP(t, func(http.ResponseWriter, *http.Request, map[string]any) { t.Fatal("unexpected create") }), func(d *Deps) {
				if mode == "no token" {
					d.LinearToken = nil
				}
				if mode == "no tracker" {
					_ = os.WriteFile(filepath.Join(d.ManagedDir("workspace"), "ai_chat.yaml"), []byte("about: Test"), 0600)
				}
			})
			thread := testThread(t, s)
			rec := request(h, "POST", "/api/ai-chat/threads/"+thread.ID+"/actions", `{"kind":"ticket"}`, "octocat", "on")
			if mode == "invalid JSON" {
				_, b, card := latestTicket(t, s, thread)
				if calls != 2 || b.State != "failed" || card.Error != "Could not draft the ticket." {
					t.Fatal(calls, b, card)
				}
			} else if rec.Code != 400 || !strings.Contains(rec.Body.String(), "not available") || calls != 0 {
				t.Fatal(rec.Code, rec.Body, calls)
			}
			res := request(h, "GET", "/api/ai-chat/sources?workspace=workspace", "", "octocat", "on")
			var sources SourcesResponse
			if json.Unmarshal(res.Body.Bytes(), &sources) != nil {
				t.Fatal(res.Body)
			}
			available := mode == "invalid JSON"
			if slices.Contains(sources.Actions, "ticket") != available || (sources.Ticket != nil) != available {
				t.Fatal(sources)
			}
			if available && (sources.Sources[0].Access != "read_create_ticket" || sources.Ticket.Verb != "Open Linear ticket") {
				t.Fatal(sources)
			}
		})
	}
}

func TestTicketActionOwnershipAndGates(t *testing.T) {
	h, s := ticketFixture(t, nil, ticketHTTP(t, func(http.ResponseWriter, *http.Request, map[string]any) { t.Fatal("unexpected mutation") }))
	thread := testThread(t, s)
	other := testThread(t, s)
	artifact := addTicketArtifact(t, s, other, "Other")
	path := "/api/ai-chat/threads/" + thread.ID + "/actions"
	for _, tc := range []struct {
		login, flag, body string
		status            int
	}{
		{"", "on", `{"kind":"ticket"}`, 401}, {"octocat", "off", `{"kind":"ticket"}`, 404}, {"someone", "on", `{"kind":"ticket"}`, 404},
		{"octocat", "on", `{"kind":"ticket","artifactId":"` + artifact.ID + `"}`, 404},
		{"octocat", "on", `{"kind":"ticket","title":"Missing body"}`, 400},
		{"octocat", "on", `{"kind":"ticket","title":"` + strings.Repeat("a", 71) + `","body":"Body"}`, 400},
	} {
		if rec := request(h, "POST", path, tc.body, tc.login, tc.flag); rec.Code != tc.status {
			t.Fatal(rec.Code, rec.Body)
		}
	}
}

func TestStartTicketEndsBatchAndAvailability(t *testing.T) {
	for _, available := range []bool{false, true} {
		t.Run(fmt.Sprint(available), func(t *testing.T) {
			s := testStore(t)
			thread := testThread(t, s)
			ctx := context.Background()
			m, _ := s.BeginTurn(ctx, thread, "Open a ticket", "fake", false)
			writes, modelCalls := 0, 0
			d := Deps{LinearToken: func(string) (string, bool) { return ticketSecret, true }, HTTPClient: ticketHTTP(t, func(w http.ResponseWriter, _ *http.Request, v map[string]any) {
				writes++
				ticketSuccess(w, v["title"].(string))
			})}
			cfg := &config.Config{}
			if available {
				cfg.IssueTracker = &config.IssueTracker{}
				cfg.IssueTracker.DefaultFields.Team = "Product"
			}
			r := &Runner{Store: s, AvailableActions: d.availableActions, TicketCreator: d.ticketCreator}
			p := fakeProvider{stream: func(_ context.Context, req llm.Request, emit func(string) error) (llm.Response, error) {
				modelCalls++
				offered := false
				for _, tool := range req.Tools {
					if tool.Name == tools.StartTicket {
						offered = true
					}
				}
				if offered != available {
					t.Fatal("wrong offered tools")
				}
				if modelCalls > 1 {
					return llm.Response{}, nil
				}
				_ = emit("I will open the ticket.")
				return llm.Response{ToolCalls: []llm.ToolCall{{ID: "ticket", Name: tools.StartTicket, Arguments: `{"title":"Swap","body":"31 requests ` + ticketSecret + `"}`}, {ID: "later", Name: tools.EmitBlock, Arguments: testBlockArgs}}}, nil
			}}
			rec := httptest.NewRecorder()
			if err := r.Run(ctx, thread, m, cfg, p, streamWriter(rec)); err != nil {
				t.Fatal(err)
			}
			if strings.Contains(rec.Body.String(), ticketSecret) {
				t.Fatal("SSE secret")
			}
			if available {
				m, b, card := latestTicket(t, s, thread)
				if writes != 1 || modelCalls != 1 || b.State != "created" || m.Content != "I will open the ticket." || strings.Contains(card.Body, ticketSecret) {
					t.Fatal(writes, modelCalls, m, b, card)
				}
				if strings.Contains(string(m.Blocks), "stat_tiles") {
					t.Fatal("later tool executed")
				}
			} else if writes != 0 {
				t.Fatal(writes)
			}
		})
	}
}

func TestTicketConcurrentSubmitAndCancellation(t *testing.T) {
	entered := make(chan struct{})
	var writes atomic.Int32
	p := fakeProvider{stream: func(ctx context.Context, _ llm.Request, _ func(string) error) (llm.Response, error) {
		close(entered)
		<-ctx.Done()
		return llm.Response{}, ctx.Err()
	}}
	h, s := ticketFixture(t, p, ticketHTTP(t, func(http.ResponseWriter, *http.Request, map[string]any) { writes.Add(1) }))
	thread := testThread(t, s)
	path := "/api/ai-chat/threads/" + thread.ID
	done := make(chan *httptest.ResponseRecorder, 1)
	go func() { done <- request(h, "POST", path+"/actions", `{"kind":"ticket"}`, "octocat", "on") }()
	<-entered
	rec := request(h, "POST", path+"/actions", `{"kind":"ticket"}`, "octocat", "on")
	if rec.Code != 429 {
		t.Fatal(rec.Code)
	}
	request(h, "POST", path+"/cancel", "", "octocat", "on")
	<-done
	m, b, card := latestTicket(t, s, thread)
	if writes.Load() != 0 || m.Status != "cancelled" || b.State != "failed" || card.Error != "Stopped. Nothing was created." {
		t.Fatal(m, b, card)
	}
}

func TestTicketUnknownOutcomeAndRecovery(t *testing.T) {
	for _, sent := range []bool{false, true} {
		t.Run(fmt.Sprint(sent), func(t *testing.T) {
			s := testStore(t)
			thread := testThread(t, s)
			ctx := context.Background()
			m, err := s.beginAction(ctx, thread, "fake", "", "ticket")
			if err != nil {
				t.Fatal(err)
			}
			card := TicketCard{Target: tracker.LinearTarget("Product", "Backlog", nil), TicketRequest: tools.TicketRequest{Title: "Swap", Body: "31 requests"}}
			marker := `null`
			if sent {
				marker = `{"requestSent":true}`
			}
			_, err = s.DB.Exec(`UPDATE ai_chat_tool_runs SET args_json=?,result_json=? WHERE message_id=?`, string(redactedJSON(card, nil)), marker, m.ID)
			if err != nil {
				t.Fatal(err)
			}
			if err = (&Runner{Store: s}).RecoverInterrupted(ctx, thread); err != nil {
				t.Fatal(err)
			}
			_, b, card := latestTicket(t, s, thread)
			want := "Stopped. Nothing was created."
			if sent {
				want = tracker.LinearUnconfirmed
			}
			if b.State != "failed" || card.Error != want || card.Body != "31 requests" {
				t.Fatal(b, card)
			}
		})
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	s := testStore(t)
	thread := testThread(t, s)
	m, _ := s.beginAction(ctx, thread, "fake", "", "ticket")
	d := Deps{LinearToken: func(string) (string, bool) { return ticketSecret, true }, HTTPClient: ticketHTTP(t, func(http.ResponseWriter, *http.Request, map[string]any) { cancel() })}
	cfg := &config.Config{IssueTracker: &config.IssueTracker{}}
	cfg.IssueTracker.DefaultFields.Team = "Product"
	r := &Runner{Store: s, TicketCreator: d.ticketCreator}
	if err := r.RunTicket(ctx, thread, m, cfg, nil, tools.TicketRequest{Title: "Swap", Body: "31 requests"}, func(string, any) error { return nil }); err != nil && !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	_, _, card := latestTicket(t, s, thread)
	if card.Error != tracker.LinearUnconfirmed {
		t.Fatal(card)
	}
}

func TestTicketWrapUpMetadataAndHistory(t *testing.T) {
	s := testStore(t)
	thread := testThread(t, s)
	ctx := context.Background()
	m, _ := s.BeginTurn(ctx, thread, "Ideas", "fake", false)
	d := Deps{LinearToken: func(string) (string, bool) { return ticketSecret, true }}
	cfg := &config.Config{IssueTracker: &config.IssueTracker{}}
	cfg.IssueTracker.DefaultFields.Team = "Product"
	cfg.IssueTracker.DefaultFields.State = "Backlog"
	calls := 0
	p := fakeProvider{stream: func(context.Context, llm.Request, func(string) error) (llm.Response, error) {
		calls++
		if calls == 1 {
			return llm.Response{ToolCalls: []llm.ToolCall{{ID: "wrap", Name: tools.OfferActions, Arguments: `{"actions":["artifact","ticket"]}`}}}, nil
		}
		return llm.Response{}, nil
	}}
	r := &Runner{Store: s, AvailableActions: d.availableActions, TicketCreator: d.ticketCreator}
	if err := r.Run(ctx, thread, m, cfg, p, func(string, any) error { return nil }); err != nil {
		t.Fatal(err)
	}
	messages, _ := s.Messages(ctx, thread.ID, 0)
	var blocks []tools.Block
	_ = json.Unmarshal(messages[len(messages)-1].Blocks, &blocks)
	if len(blocks) != 1 || !strings.Contains(string(blocks[0].Ticket), `"verb":"Open Linear ticket"`) || !strings.Contains(string(blocks[0].Ticket), `"state":"Backlog"`) {
		t.Fatal(blocks)
	}
	if len([]rune(ticketSummary(strings.Repeat("界", 300)))) > 280 {
		t.Fatal("summary too long")
	}
}

func TestStartTicketInterruptedBeforeSendRecoversDraft(t *testing.T) {
	ctx := context.Background()
	s := testStore(t)
	thread := testThread(t, s)
	m, err := s.BeginTurn(ctx, thread, "Open a ticket", "fake", false)
	if err != nil {
		t.Fatal(err)
	}
	r := &Runner{Store: s}
	input := tools.TicketRequest{Title: "Swap", Body: "31 requests"}
	creator := tracker.NewLinear("Product", ticketSecret, ticketHTTP(t, func(http.ResponseWriter, *http.Request, map[string]any) { t.Fatal("unexpected create") }))
	func() {
		defer func() {
			if recover() == nil {
				t.Error("expected simulated restart")
			}
		}()
		_ = r.createTicket(ctx, thread, &m, nil, input, creator, tracker.LinearTarget("Product", "", nil), []string{ticketSecret}, 1, false, func(event string, _ any) error {
			if event == EventAction {
				panic("restart")
			}
			return nil
		})
	}()
	if err = r.RecoverInterrupted(ctx, thread); err != nil {
		t.Fatal(err)
	}
	_, b, card := latestTicket(t, s, thread)
	if b.State != "failed" || card.Error != "Stopped. Nothing was created." || card.Body != "31 requests" || card.OutcomeUnknown {
		t.Fatal(b, card)
	}
}
