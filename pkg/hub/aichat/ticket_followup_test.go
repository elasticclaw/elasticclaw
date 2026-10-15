package aichat

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/elasticclaw/elasticclaw/pkg/hub/aichat/config"
	"github.com/elasticclaw/elasticclaw/pkg/hub/aichat/internal/readhttp"
	"github.com/elasticclaw/elasticclaw/pkg/hub/aichat/llm"
	"github.com/elasticclaw/elasticclaw/pkg/hub/aichat/tools"
)

func TestTicketActionKeyReplaysBeforeDrafting(t *testing.T) {
	drafts, writes := 0, 0
	p := fakeProvider{stream: func(context.Context, llm.Request, func(string) error) (llm.Response, error) {
		drafts++
		return llm.Response{Text: fmt.Sprintf(`{"title":"Draft %d","body":"Evidence"}`, drafts)}, nil
	}}
	var deps Deps
	h, s := ticketFixture(t, p, ticketHTTP(t, func(w http.ResponseWriter, _ *http.Request, v map[string]any) {
		writes++
		ticketSuccess(w, v["title"].(string))
	}), func(d *Deps) { deps = *d })
	thread := testThread(t, s)
	path := "/api/ai-chat/threads/" + thread.ID + "/actions"
	body := `{"kind":"ticket","actionKey":"click-123"}`
	if rec := request(h, "POST", path, body, "octocat", "on"); rec.Code != 200 {
		t.Fatal(rec.Body)
	}
	m, _, original := latestTicket(t, s, thread)
	var args TicketCard
	var raw string
	if err := s.DB.QueryRow(`SELECT args_json FROM ai_chat_tool_runs WHERE message_id=?`, m.ID).Scan(&raw); err != nil {
		t.Fatal(err)
	}
	if json.Unmarshal([]byte(raw), &args) != nil || args.ActionKey != "click-123" || original.ActionKey != args.ActionKey {
		t.Fatal(raw, original)
	}
	// Recovery after restart must not even need a working drafting provider.
	deps.LLM = func(string) (llm.Provider, error) {
		t.Fatal("draft provider requested on replay")
		return nil, errors.New("unavailable")
	}
	originalClient := deps.HTTPClient
	deps.HTTPClient = sourceClient(func(http.ResponseWriter, *http.Request) { t.Fatal("provider contacted on replay") })
	h = Routes(deps)
	for i := 0; i < 2; i++ {
		rec := request(h, "POST", path, body, "octocat", "on")
		latest, block, card := latestTicket(t, s, thread)
		if err := s.DB.QueryRow(`SELECT args_json FROM ai_chat_tool_runs WHERE message_id=?`, latest.ID).Scan(&raw); err != nil {
			t.Fatal(err)
		}
		if json.Unmarshal([]byte(raw), &args) != nil || args.ActionKey != "click-123" {
			t.Fatal(raw)
		}
		if rec.Code != 200 || block.State != "created" || !reflect.DeepEqual(card, original) || drafts != 1 || writes != 1 {
			t.Fatal(rec.Code, rec.Body, card, drafts, writes)
		}
	}
	var count int
	if err := s.DB.QueryRow(`SELECT COUNT(*) FROM ai_chat_handoffs WHERE thread_id=?`, thread.ID).Scan(&count); err != nil || count != 1 {
		t.Fatal(count, err)
	}
	// Keys are scoped to the thread, not the user or workspace.
	deps.HTTPClient = originalClient
	deps.LLM = func(string) (llm.Provider, error) { return p, nil }
	h = Routes(deps)
	other := testThread(t, s)
	if rec := request(h, "POST", "/api/ai-chat/threads/"+other.ID+"/actions", body, "octocat", "on"); rec.Code != 200 || writes != 2 {
		t.Fatal(rec.Code, writes)
	}
}

func TestTicketActionKeyValidation(t *testing.T) {
	h, s := ticketFixture(t, nil, ticketHTTP(t, func(http.ResponseWriter, *http.Request, map[string]any) { t.Fatal("unexpected write") }))
	thread := testThread(t, s)
	for _, key := range []string{strings.Repeat("a", 65), "a_b", "a b", "a/b", "á"} {
		body := string(redactedJSON(map[string]any{"kind": "ticket", "actionKey": key}, nil))
		rec := request(h, "POST", "/api/ai-chat/threads/"+thread.ID+"/actions", body, "octocat", "on")
		if rec.Code != 400 || !strings.Contains(rec.Body.String(), "actionKey") {
			t.Fatal(rec.Code, rec.Body)
		}
	}
	if !ticketActionKey.MatchString(strings.Repeat("a", 64)) {
		t.Fatal("valid boundary key rejected")
	}
}

func TestTicketUnknownGateUsesCardTracker(t *testing.T) {
	for _, name := range []string{"Example", ""} {
		t.Run(name, func(t *testing.T) {
			h, s := ticketFixture(t, nil, ticketHTTP(t, func(w http.ResponseWriter, _ *http.Request, _ map[string]any) { w.WriteHeader(502) }))
			thread := testThread(t, s)
			path := "/api/ai-chat/threads/" + thread.ID + "/actions"
			body := `{"kind":"ticket","title":"Draft","body":"Evidence","actionKey":"unknown-123"}`
			if rec := request(h, "POST", path, body, "octocat", "on"); rec.Code != 200 {
				t.Fatal(rec.Body)
			}
			m, _, card := latestTicket(t, s, thread)
			card.Tracker = name
			setTicketBlock(&m, "failed", card)
			if err := s.FinishMessage(context.Background(), m); err != nil {
				t.Fatal(err)
			}
			expected := name
			if expected == "" {
				expected = "Linear"
			}
			rec := request(h, "POST", path, body, "octocat", "on")
			if rec.Code != 409 || !strings.Contains(rec.Body.String(), "Check "+expected+" first") {
				t.Fatal(rec.Code, rec.Body)
			}
		})
	}
}

func TestTicketActionsIgnoreUnrelatedAndMalformedRows(t *testing.T) {
	s := testStore(t)
	thread := testThread(t, s)
	ctx := context.Background()
	for _, tc := range []struct{ provider, args, result, blocks string }{
		{"AI Chat", `{}`, `null`, `[]`},
		{"model", `{}`, `{"requestSent":true}`, `[]`},
		{"AI Chat", `{`, `null`, `[]`},
		{"AI Chat", `{}`, `{`, `[]`},
		{"AI Chat", `{}`, `null`, `{`},
		{"AI Chat", `{}`, `null`, `[{"type":"action","kind":"ticket","card":"bad"}]`},
	} {
		m, err := s.beginAction(ctx, thread, "fake", "", "ticket")
		if err != nil {
			t.Fatal(err)
		}
		// Completed malformed records must not participate in interrupted-turn recovery.
		if _, err = s.DB.Exec(`UPDATE ai_chat_messages SET status='completed',blocks_json=? WHERE id=?`, tc.blocks, m.ID); err != nil {
			t.Fatal(err)
		}
		if _, err = s.DB.Exec(`UPDATE ai_chat_tool_runs SET provider=?,args_json=?,result_json=? WHERE message_id=?`, tc.provider, tc.args, tc.result, m.ID); err != nil {
			t.Fatal(err)
		}
	}
	actions, err := s.ticketActions(ctx, thread.ID, "")
	if err != nil || len(actions) != 1 || actions[0].unknown {
		t.Fatal(actions, err)
	}
}

func TestTicketDraftMarkdownFences(t *testing.T) {
	for _, fence := range []string{"", "```", "```json", "inline"} {
		t.Run(fence, func(t *testing.T) {
			s := testStore(t)
			thread := testThread(t, s)
			calls := 0
			p := fakeProvider{stream: func(context.Context, llm.Request, func(string) error) (llm.Response, error) {
				calls++
				raw := `{"title":"Draft","body":"Evidence"}`
				if fence == "inline" {
					raw = "```json " + raw + " ```"
				} else if fence != "" {
					raw = fence + "\n" + raw + "\n```"
				}
				return llm.Response{Text: " \n" + raw + "\n "}, nil
			}}
			draft, err := (&Runner{Store: s}).draftTicket(context.Background(), thread, &Message{}, p, nil)
			if err != nil || calls != 1 || draft.Title != "Draft" {
				t.Fatal(draft, err, calls)
			}
		})
	}
}

func TestTicketFailureHeadlinesByPhase(t *testing.T) {
	for _, tc := range []struct{ phase, headline string }{
		{"team", "Could not prepare the ticket"}, {"state", "Could not prepare the ticket"}, {"label", "Could not prepare the ticket"},
		{"rejected", "Linear did not accept the ticket"}, {"unknown", "Linear did not confirm the ticket"},
	} {
		t.Run(tc.phase, func(t *testing.T) {
			client := sourceClient(func(w http.ResponseWriter, r *http.Request) {
				var req struct{ Query string }
				_ = json.NewDecoder(r.Body).Decode(&req)
				switch {
				case strings.Contains(req.Query, "teams("):
					if tc.phase == "team" {
						fmt.Fprint(w, `{"data":{"teams":{"nodes":[]}}}`)
					} else {
						fmt.Fprint(w, `{"data":{"teams":{"nodes":[{"id":"team","name":"Product"}]}}}`)
					}
				case strings.Contains(req.Query, "workflowStates("):
					if tc.phase == "state" {
						fmt.Fprint(w, `{"data":{"workflowStates":{"nodes":[]}}}`)
					} else {
						fmt.Fprint(w, `{"data":{"workflowStates":{"nodes":[{"id":"state","name":"Backlog"}]}}}`)
					}
				case strings.Contains(req.Query, "issueLabels("):
					if tc.phase == "label" {
						fmt.Fprint(w, `{"data":{"issueLabels":{"nodes":[]}}}`)
					} else {
						fmt.Fprint(w, `{"data":{"issueLabels":{"nodes":[{"id":"label","name":"ai-chat"}]}}}`)
					}
				default:
					if tc.phase == "unknown" {
						w.WriteHeader(502)
					} else if tc.phase == "rejected" {
						w.WriteHeader(400)
					} else {
						t.Fatal("write before validation")
					}
				}
			})
			h, s := ticketFixture(t, nil, client)
			thread := testThread(t, s)
			rec := request(h, "POST", "/api/ai-chat/threads/"+thread.ID+"/actions", `{"kind":"ticket","title":"Draft","body":"Evidence"}`, "octocat", "on")
			_, b, card := latestTicket(t, s, thread)
			if rec.Code != 200 || b.State != "failed" || card.FailureHeadline != tc.headline {
				t.Fatal(rec.Code, card)
			}
		})
	}
	// A model-path draft still gets validated before provider creation.
	s := testStore(t)
	thread := testThread(t, s)
	ctx := context.Background()
	m, _ := s.beginAction(ctx, thread, "fake", "", "ticket")
	d := Deps{LinearToken: func(string) (string, bool) { return ticketSecret, true }}
	cfg := &config.Config{IssueTracker: &config.IssueTracker{}}
	creator, target, secrets := d.ticketCreator(thread.Workspace, cfg)
	r := &Runner{Store: s}
	if err := r.createTicket(ctx, thread, &m, nil, tools.TicketRequest{Title: "Draft"}, "", creator, target, secrets, 1, false, false, func(string, any) error { return nil }); err != nil {
		t.Fatal(err)
	}
	_, _, card := latestTicket(t, s, thread)
	if card.FailureHeadline != "Could not prepare the ticket" || card.ActionKey == "" {
		t.Fatal(card)
	}
}

func TestAvailableTicketActionsUseCachedAuthHealth(t *testing.T) {
	cfg := &config.Config{IssueTracker: &config.IssueTracker{}}
	d := Deps{health: newSourceHealth(), LinearToken: func(string) (string, bool) { return ticketSecret, true }, HTTPClient: sourceClient(func(http.ResponseWriter, *http.Request) { t.Fatal("availability made a network call") })}
	key := sourceHealthKey("workspace", cfg, "issue_tracker")
	for _, tc := range []struct {
		err       error
		available bool
	}{
		{nil, true}, {&readhttp.StatusError{Code: 401}, false}, {&readhttp.StatusError{Code: 403}, false}, {&readhttp.StatusError{Code: 503}, true},
	} {
		d.health.entries[key] = &healthEntry{result: healthResult{err: tc.err}, started: time.Now()}
		if got := d.availableActions("workspace", cfg); slices.Contains(got, "ticket") != tc.available {
			t.Fatal(got, tc.err)
		}
	}
	delete(d.health.entries, key)
	if !slices.Contains(d.availableActions("workspace", cfg), "ticket") {
		t.Fatal("unknown health blocked ticket")
	}
}

func TestSourcesRejectedTokenOmitsTicketAction(t *testing.T) {
	calls := 0
	h, _ := ticketFixture(t, nil, sourceClient(func(w http.ResponseWriter, r *http.Request) {
		calls++
		w.WriteHeader(401)
	}))
	for i := 0; i < 2; i++ {
		rec := request(h, "GET", "/api/ai-chat/sources?workspace=workspace", "", "octocat", "on")
		var response SourcesResponse
		if json.Unmarshal(rec.Body.Bytes(), &response) != nil || rec.Code != 200 {
			t.Fatal(rec.Code, rec.Body)
		}
		if slices.Contains(response.Actions, "ticket") || len(response.Sources) != 1 || response.Sources[0].Error != "The API key was rejected." {
			t.Fatal(response)
		}
	}
	if calls != 1 {
		t.Fatal("availability repeated the health call", calls)
	}
}
