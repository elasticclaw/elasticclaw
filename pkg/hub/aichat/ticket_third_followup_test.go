package aichat

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"slices"
	"strings"
	"testing"

	"github.com/elasticclaw/elasticclaw/pkg/hub/aichat/config"
	"github.com/elasticclaw/elasticclaw/pkg/hub/aichat/llm"
	"github.com/elasticclaw/elasticclaw/pkg/hub/aichat/tools"
	"github.com/elasticclaw/elasticclaw/pkg/hub/aichat/tracker"
)

func assertTicketReplay(t *testing.T, s *Store, messageID, attemptID, state string, unknown bool) {
	t.Helper()
	var summary, failure, raw string
	if err := s.DB.QueryRow(`SELECT summary,error,result_json FROM ai_chat_tool_runs WHERE message_id=? AND tool='ticket' AND provider='AI Chat'`, messageID).Scan(&summary, &failure, &raw); err != nil {
		t.Fatal(err)
	}
	var result struct {
		ReplayOf, State string
		OutcomeUnknown  bool
	}
	if json.Unmarshal([]byte(raw), &result) != nil || result.ReplayOf != attemptID || result.State != state || result.OutcomeUnknown != unknown || summary == "" || (failure != "") != (state == "failed") {
		t.Fatal(summary, failure, raw)
	}
}

type ticketCreateFunc func(context.Context, tracker.Draft) (tracker.Issue, error)

func (f ticketCreateFunc) Create(ctx context.Context, draft tracker.Draft) (tracker.Issue, error) {
	return f(ctx, draft)
}

func TestTicketUnknownOutcomeUsesErrorFlag(t *testing.T) {
	for _, unknown := range []bool{true, false} {
		t.Run(fmt.Sprint(unknown), func(t *testing.T) {
			s := testStore(t)
			thread := testThread(t, s)
			ctx := context.Background()
			m, err := s.beginAction(ctx, thread, "", "", "ticket")
			if err != nil {
				t.Fatal(err)
			}
			message := tracker.LinearUnconfirmed
			if unknown {
				message = "A different safe message " + ticketSecret
			}
			creator := ticketCreateFunc(func(_ context.Context, d tracker.Draft) (tracker.Issue, error) {
				if err := d.BeforeSend(); err != nil {
					return tracker.Issue{}, err
				}
				return tracker.Issue{}, fmt.Errorf("wrapped: %w", &tracker.CreateError{Message: message, Unconfirmed: unknown})
			})
			r := &Runner{Store: s}
			if err := r.createTicket(ctx, thread, &m, nil, tools.TicketRequest{Title: "Draft", Body: "Evidence"}, "click", creator, tracker.LinearTarget("Product", "", nil), []string{ticketSecret}, 1, false, "", func(_ string, payload any) error {
				if strings.Contains(string(redactedJSON(payload, nil)), ticketSecret) {
					t.Error("secret emitted")
				}
				return nil
			}); err != nil {
				t.Fatal(err)
			}
			_, _, card := latestTicket(t, s, thread)
			if card.OutcomeUnknown != unknown || strings.Contains(card.Error, ticketSecret) {
				t.Fatal(card)
			}
			previous, err := s.ticketActions(ctx, thread.ID, "")
			if err != nil || (unknownTicketAction(previous) != nil) != unknown {
				t.Fatal(previous, err)
			}
		})
	}
}

func TestIdenticalModelTicketDraftsCreateSeparateIssues(t *testing.T) {
	writes := 0
	p := fakeProvider{stream: func(context.Context, llm.Request, func(string) error) (llm.Response, error) {
		return llm.Response{ToolCalls: []llm.ToolCall{{ID: "ticket", Name: tools.StartTicket, Arguments: `{"title":"Draft","body":"Evidence"}`}}}, nil
	}}
	h, s := ticketFixture(t, p, ticketHTTP(t, func(w http.ResponseWriter, _ *http.Request, input map[string]any) {
		writes++
		if input["title"] != "Draft" {
			t.Fatal(input)
		}
		fmt.Fprintf(w, `{"data":{"issueCreate":{"success":true,"issue":{"identifier":"PRO-%d","url":"https://linear.app/PRO-%d","title":"Normalized draft"}}}}`, writes, writes)
	}))
	thread := testThread(t, s)
	var first TicketCard
	for i := 1; i <= 2; i++ {
		rec := request(h, "POST", "/api/ai-chat/threads/"+thread.ID+"/messages", `{"text":"Create another identical ticket"}`, "octocat", "on")
		_, block, card := latestTicket(t, s, thread)
		if rec.Code != 200 || writes != i || block.State != "created" || card.Identifier != fmt.Sprintf("PRO-%d", i) || card.Title != "Normalized draft" {
			t.Fatal(rec.Code, rec.Body, writes, card)
		}
		if i == 1 {
			first = card
		} else if first.AttemptID == card.AttemptID || first.ActionKey == card.ActionKey {
			t.Fatal(first, card)
		}
	}
	var count int
	if err := s.DB.QueryRow(`SELECT COUNT(*) FROM ai_chat_handoffs WHERE thread_id=?`, thread.ID).Scan(&count); err != nil || count != 2 {
		t.Fatal(count, err)
	}
}

func TestTicketTokenResolvedOncePerRequest(t *testing.T) {
	calls := 0
	p := fakeProvider{stream: func(context.Context, llm.Request, func(string) error) (llm.Response, error) {
		return llm.Response{}, nil
	}}
	h, s := ticketFixture(t, p, ticketHTTP(t, func(w http.ResponseWriter, _ *http.Request, input map[string]any) {
		ticketSuccess(w, input["title"].(string))
	}), func(d *Deps) {
		d.LinearToken = func(string) (string, bool) { calls++; return fmt.Sprintf("token-%d", calls), true }
		transport := d.HTTPClient.Transport
		d.HTTPClient = &http.Client{Transport: sourceTransport(func(r *http.Request) (*http.Response, error) {
			if r.Header.Get("Authorization") != fmt.Sprintf("token-%d", calls) {
				t.Error("stale token", r.Header.Get("Authorization"))
			}
			return transport.RoundTrip(r)
		})}
	})
	thread := testThread(t, s)
	for _, endpoint := range []string{"sources", "messages", "actions"} {
		for i := 0; i < 2; i++ {
			before := calls
			method, path, body := "GET", "/api/ai-chat/sources?workspace=workspace", ""
			if endpoint != "sources" {
				method, path = "POST", "/api/ai-chat/threads/"+thread.ID+"/"+endpoint
				body = `{"text":"Hello"}`
				if endpoint == "actions" {
					body = `{"kind":"ticket","title":"Draft","body":"Evidence"}`
				}
			}
			rec := request(h, method, path, body, "octocat", "on")
			if rec.Code != 200 || calls != before+1 {
				t.Fatal(endpoint, rec.Code, rec.Body, calls, before)
			}
			if endpoint == "sources" {
				var response SourcesResponse
				if json.Unmarshal(rec.Body.Bytes(), &response) != nil || !slices.Contains(response.Actions, "ticket") || response.Ticket == nil || len(response.Sources) != 1 || response.Sources[0].Access != "read_create_ticket" {
					t.Fatal(response)
				}
			}
		}
	}
}

func TestRunnerCopiesPreparedSecrets(t *testing.T) {
	s := testStore(t)
	thread := testThread(t, s)
	ctx := context.Background()
	m, err := s.BeginTurn(ctx, thread, "Hello", "fake", false)
	if err != nil {
		t.Fatal(err)
	}
	backing := []string{"read-secret", "untouched"}
	r := &Runner{Store: s, PrepareReads: func(_ context.Context, _ string, cfg *config.Config) (*tools.ReadRegistry, *config.Config, []string) {
		return nil, cfg, backing[:1]
	}, TicketCreator: func(context.Context, string, *config.Config) (tracker.Creator, tracker.Target, []string) {
		return nil, tracker.Target{}, []string{ticketSecret}
	}}
	p := fakeProvider{stream: func(context.Context, llm.Request, func(string) error) (llm.Response, error) {
		return llm.Response{}, nil
	}}
	if err := r.Run(ctx, thread, m, &config.Config{}, p, func(string, any) error { return nil }); err != nil {
		t.Fatal(err)
	}
	if backing[1] != "untouched" {
		t.Fatal("PrepareReads slice was overwritten", backing)
	}
}

func TestRecoverWorkingTicketCard(t *testing.T) {
	for _, sent := range []bool{false, true} {
		t.Run(fmt.Sprint(sent), func(t *testing.T) {
			ctx := context.Background()
			s := testStore(t)
			thread := testThread(t, s)
			m, err := s.beginAction(ctx, thread, "", "", "ticket")
			if err != nil {
				t.Fatal(err)
			}
			card := TicketCard{AttemptID: m.ID, ActionKey: "click", Target: tracker.LinearTarget("Product", "Backlog", nil), TicketRequest: tools.TicketRequest{Title: "Draft", Body: "Evidence"}}
			m.Blocks = []byte(`[{"type":"callout","id":"note","at":0,"data":{"text":"Keep this"}}]`)
			setTicketBlock(&m, "working", card)
			if err = s.FinishMessage(ctx, m); err != nil {
				t.Fatal(err)
			}
			if _, err = s.DB.Exec(`UPDATE ai_chat_tool_runs SET args_json=?,result_json=? WHERE message_id=?`, string(redactedJSON(card, nil)), fmt.Sprintf(`{"requestSent":%t}`, sent), m.ID); err != nil {
				t.Fatal(err)
			}
			if err = s.RecoverInterrupted(ctx, thread.ID); err != nil {
				t.Fatal(err)
			}
			recovered, block, got := latestTicket(t, s, thread)
			if recovered.Status != "error" || block.State != "failed" || got.OutcomeUnknown != sent || got.Title != card.Title || got.AttemptID != card.AttemptID || !strings.Contains(string(recovered.Blocks), "Keep this") {
				t.Fatal(recovered, block, got)
			}
			want := "Stopped. Nothing was created."
			if sent {
				want = tracker.LinearUnconfirmed
			}
			if got.Error != want {
				t.Fatal(got)
			}
			original := string(recovered.Blocks)
			if err = s.RecoverInterrupted(ctx, thread.ID); err != nil {
				t.Fatal(err)
			}
			recovered, _, _ = latestTicket(t, s, thread)
			if string(recovered.Blocks) != original {
				t.Fatal("recovery was not idempotent")
			}
		})
	}
}

func TestTicketUnknownGateWithoutConfiguredTracker(t *testing.T) {
	var deps Deps
	h, s := ticketFixture(t, nil, ticketHTTP(t, func(w http.ResponseWriter, _ *http.Request, _ map[string]any) { w.WriteHeader(502) }), func(d *Deps) { deps = *d })
	thread := testThread(t, s)
	path := "/api/ai-chat/threads/" + thread.ID + "/actions"
	body := `{"kind":"ticket","title":"Draft","body":"Evidence"}`
	if rec := request(h, "POST", path, body, "octocat", "on"); rec.Code != 200 {
		t.Fatal(rec.Code, rec.Body)
	}
	m, _, card := latestTicket(t, s, thread)
	card.Tracker = ""
	setTicketBlock(&m, "failed", card)
	if err := s.FinishMessage(context.Background(), m); err != nil {
		t.Fatal(err)
	}
	deps.LinearToken = nil
	h = Routes(deps)
	if rec := request(h, "POST", path, body, "octocat", "on"); rec.Code != 409 || !strings.HasPrefix(rec.Body.String(), "The tracker did not confirm") {
		t.Fatal(rec.Code, rec.Body)
	}
	m, err := s.beginAction(context.Background(), thread, "", "", "ticket")
	if err != nil {
		t.Fatal(err)
	}
	if err = (&Runner{Store: s}).RunTicket(context.Background(), thread, m, &config.Config{}, nil, tools.TicketRequest{Title: "Draft", Body: "Evidence"}, "", "", func(string, any) error { return nil }); err != nil {
		t.Fatal(err)
	}
	_, _, blocked := latestTicket(t, s, thread)
	if blocked.Tracker != "The tracker" || !blocked.OutcomeUnknown {
		t.Fatal(blocked)
	}
}
