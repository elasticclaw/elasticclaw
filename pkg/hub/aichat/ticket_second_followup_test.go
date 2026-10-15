package aichat

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/elasticclaw/elasticclaw/pkg/hub/aichat/config"
	"github.com/elasticclaw/elasticclaw/pkg/hub/aichat/llm"
	"github.com/elasticclaw/elasticclaw/pkg/hub/aichat/tools"
	"github.com/elasticclaw/elasticclaw/pkg/hub/aichat/tracker"
)

func TestTicketPartialResponseBlocksRetry(t *testing.T) {
	writes := 0
	h, s := ticketFixture(t, nil, ticketHTTP(t, func(w http.ResponseWriter, _ *http.Request, _ map[string]any) {
		writes++
		fmt.Fprint(w, `{"data":{"issueCreate":{"success":true,"issue":null}},"errors":[{"message":"Permission denied sentinel-ticket-token"}]}`)
	}))
	thread := testThread(t, s)
	path := "/api/ai-chat/threads/" + thread.ID + "/actions"
	body := `{"kind":"ticket","actionKey":"click","title":"Draft","body":"Evidence"}`
	if rec := request(h, "POST", path, body, "octocat", "on"); rec.Code != 200 || strings.Contains(rec.Body.String(), ticketSecret) {
		t.Fatal(rec.Code, rec.Body)
	}
	_, block, card := latestTicket(t, s, thread)
	if block.State != "failed" || !card.OutcomeUnknown || card.Error != tracker.LinearUnconfirmed {
		t.Fatal(block, card)
	}
	if rec := request(h, "POST", path, body, "octocat", "on"); rec.Code != 409 || writes != 1 {
		t.Fatal(rec.Code, rec.Body, writes)
	}
}

func TestTicketSentMutationSurvivesCancellation(t *testing.T) {
	for _, rejected := range []bool{false, true} {
		t.Run(fmt.Sprint(rejected), func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			s := testStore(t)
			thread := testThread(t, s)
			m, err := s.beginAction(ctx, thread, "", "", "ticket")
			if err != nil {
				t.Fatal(err)
			}
			entered, release := make(chan struct{}), make(chan struct{}, 1)
			defer close(release)
			writes := 0
			d := Deps{LinearToken: func(string) (string, bool) { return ticketSecret, true }, HTTPClient: ticketHTTP(t, func(w http.ResponseWriter, req *http.Request, v map[string]any) {
				writes++
				close(entered)
				select {
				case <-release:
				case <-req.Context().Done():
					t.Error("sent mutation was cancelled", req.Context().Err())
					return
				}
				if req.Context().Err() != nil {
					t.Error("mutation inherited turn cancellation")
				}
				if deadline, ok := req.Context().Deadline(); !ok || time.Until(deadline) > 30*time.Second {
					t.Error("mutation is missing its bounded deadline")
				}
				if rejected {
					w.WriteHeader(403)
				} else {
					ticketSuccess(w, v["title"].(string))
				}
			})}
			cfg := &config.Config{IssueTracker: &config.IssueTracker{}}
			cfg.IssueTracker.DefaultFields.Team = "Product"
			runner := &Runner{Store: s, TicketCreator: d.ticketCreator}
			done := make(chan error, 1)
			go func() {
				done <- runner.RunTicket(ctx, thread, m, cfg, nil, tools.TicketRequest{Title: "Draft", Body: "Evidence"}, "click", "", func(string, any) error {
					return ctx.Err() // The client is gone after cancellation.
				})
			}()
			select {
			case <-entered:
			case <-time.After(5 * time.Second):
				t.Fatal("mutation did not start")
			}
			cancel()
			workingMessage, workingBlock, workingCard := latestTicket(t, s, thread)
			if workingMessage.Status != "streaming" || workingBlock.State != "working" || workingCard.Title != "Draft" || workingCard.Body != "Evidence" {
				t.Fatal(workingMessage, workingBlock, workingCard)
			}
			var marker string
			if err := s.DB.QueryRow(`SELECT result_json FROM ai_chat_tool_runs WHERE message_id=?`, m.ID).Scan(&marker); err != nil || !strings.Contains(marker, `"requestSent":true`) {
				t.Fatal(marker, err)
			}
			release <- struct{}{}
			select {
			case err = <-done:
			case <-time.After(5 * time.Second):
				t.Fatal("mutation did not finish")
			}
			if err != nil {
				t.Fatal(err)
			}
			_, block, card := latestTicket(t, s, thread)
			wantState, wantCount := "created", 1
			if rejected {
				wantState, wantCount = "failed", 0
				if card.Error != "The workspace token cannot create issues in team Product." || card.FailureHeadline != "Linear did not accept the ticket" {
					t.Fatal(card)
				}
			}
			if block.State != wantState || card.OutcomeUnknown || writes != 1 {
				t.Fatal(block, card, writes)
			}
			var count int
			if err := s.DB.QueryRow(`SELECT COUNT(*) FROM ai_chat_handoffs WHERE thread_id=?`, thread.ID).Scan(&count); err != nil || count != wantCount {
				t.Fatal(count, err)
			}
			var summary, runError string
			if err := s.DB.QueryRow(`SELECT COALESCE(summary,''),COALESCE(error,'') FROM ai_chat_tool_runs WHERE message_id=?`, m.ID).Scan(&summary, &runError); err != nil {
				t.Fatal(err)
			}
			if !rejected && (summary != "Opened PRO-1423" || runError != "") {
				t.Fatal(summary, runError)
			}
			previous, err := s.ticketActions(context.Background(), thread.ID, "")
			if err != nil || unknownTicketAction(previous) != nil {
				t.Fatal(previous, err)
			}
		})
	}
}

func TestDismissUnknownTicketAttempt(t *testing.T) {
	writes := 0
	var deps Deps
	p := fakeProvider{stream: func(context.Context, llm.Request, func(string) error) (llm.Response, error) {
		return llm.Response{Text: `{"title":"Another draft","body":"New evidence"}`}, nil
	}}
	h, s := ticketFixture(t, p, ticketHTTP(t, func(w http.ResponseWriter, _ *http.Request, v map[string]any) {
		writes++
		if writes == 1 || writes == 3 {
			w.WriteHeader(502)
			return
		}
		ticketSuccess(w, v["title"].(string))
	}), func(d *Deps) { deps = *d })
	thread := testThread(t, s)
	path := "/api/ai-chat/threads/" + thread.ID
	if rec := request(h, "POST", path+"/actions", `{"kind":"ticket","title":"Draft","body":"Evidence"}`, "octocat", "on"); rec.Code != 200 {
		t.Fatal(rec.Code, rec.Body)
	}
	_, _, card := latestTicket(t, s, thread)
	// A model refusal repeats the original attempt ID on a later card.
	cfg, err := config.Load(deps.ManagedDir(thread.Workspace))
	if err != nil {
		t.Fatal(err)
	}
	m, err := s.BeginTurn(context.Background(), thread, "Open another ticket", "", false)
	if err != nil {
		t.Fatal(err)
	}
	runner := &Runner{Store: s, TicketCreator: deps.ticketCreator}
	if err := runner.RunTicket(context.Background(), thread, m, cfg, nil, tools.TicketRequest{Title: "Other", Body: "Evidence"}, "other", "", func(string, any) error { return nil }); err != nil {
		t.Fatal(err)
	}
	dismissPath := path + "/ticket-attempts/" + card.AttemptID + "/dismiss"
	for _, tc := range []struct {
		login, flag string
		status      int
	}{
		{"", "on", 401}, {"octocat", "off", 404}, {"someone", "on", 404}, {"octocat", "on", 204}, {"octocat", "on", 204},
	} {
		if rec := request(h, "POST", dismissPath, "", tc.login, tc.flag); rec.Code != tc.status {
			t.Fatal(rec.Code, rec.Body, tc)
		}
	}
	var result string
	if err := s.DB.QueryRow(`SELECT result_json FROM ai_chat_tool_runs WHERE message_id=?`, card.AttemptID).Scan(&result); err != nil || !strings.Contains(result, `"dismissed":true`) || !strings.Contains(result, `"requestSent":true`) {
		t.Fatal(result, err)
	}
	var count int
	if err := s.DB.QueryRow(`SELECT COUNT(*) FROM ai_chat_handoffs WHERE thread_id=?`, thread.ID).Scan(&count); err != nil || count != 0 || writes != 1 {
		t.Fatal(count, writes, err)
	}
	// Reload from durable storage, using a fresh handler and store.
	h = Routes(deps)
	reloaded := request(h, "GET", path, "", "octocat", "on")
	var conversation struct{ Messages []Message }
	if reloaded.Code != 200 || json.Unmarshal(reloaded.Body.Bytes(), &conversation) != nil {
		t.Fatal(reloaded.Code, reloaded.Body)
	}
	var blocks []tools.Block
	if json.Unmarshal(conversation.Messages[0].Blocks, &blocks) != nil || len(blocks) != 1 || blocks[0].State != "failed" || !strings.Contains(string(blocks[0].Card), `"dismissed":true`) {
		t.Fatal(blocks)
	}
	previous, err := (&Store{DB: deps.DB}).ticketActions(context.Background(), thread.ID, "")
	if err != nil || unknownTicketAction(previous) != nil {
		t.Fatal(previous, err)
	}
	// The ordinary wrap-up action drafts a new ticket without a confirmation.
	if rec := request(h, "POST", path+"/actions", `{"kind":"ticket"}`, "octocat", "on"); rec.Code != 200 || writes != 2 {
		t.Fatal(rec.Code, rec.Body, writes)
	}
	_, created, _ := latestTicket(t, s, thread)
	if created.State != "created" {
		t.Fatal(created)
	}
	fresh := `{"kind":"ticket","title":"Third draft","body":"More evidence"}`
	if rec := request(h, "POST", path+"/actions", fresh, "octocat", "on"); rec.Code != 200 || writes != 3 {
		t.Fatal(rec.Code, rec.Body, writes)
	}
	if rec := request(h, "POST", path+"/actions", fresh, "octocat", "on"); rec.Code != 409 || writes != 3 {
		t.Fatal("dismissal must not clear a later unknown attempt", rec.Code, writes)
	}
}

func TestDismissTicketAttemptRejectsOtherAttempts(t *testing.T) {
	for _, outcome := range []string{"created", "rejected", "foreign", "missing", "not sent", "in flight"} {
		t.Run(outcome, func(t *testing.T) {
			h, s := ticketFixture(t, nil, ticketHTTP(t, func(w http.ResponseWriter, _ *http.Request, v map[string]any) {
				switch outcome {
				case "created":
					ticketSuccess(w, v["title"].(string))
				case "rejected":
					w.WriteHeader(400)
				default:
					w.WriteHeader(502)
				}
			}))
			thread := testThread(t, s)
			path := "/api/ai-chat/threads/" + thread.ID
			if rec := request(h, "POST", path+"/actions", `{"kind":"ticket","title":"Draft","body":"Evidence"}`, "octocat", "on"); rec.Code != 200 {
				t.Fatal(rec.Code, rec.Body)
			}
			_, _, card := latestTicket(t, s, thread)
			switch outcome {
			case "foreign":
				path = "/api/ai-chat/threads/" + testThread(t, s).ID
			case "missing":
				card.AttemptID = "missing"
			case "not sent":
				if _, err := s.DB.Exec(`UPDATE ai_chat_tool_runs SET result_json='null' WHERE message_id=?`, card.AttemptID); err != nil {
					t.Fatal(err)
				}
			case "in flight":
				if _, err := s.DB.Exec(`UPDATE ai_chat_messages SET status='streaming' WHERE id=?`, card.AttemptID); err != nil {
					t.Fatal(err)
				}
				if err := s.dismissTicketAttempt(context.Background(), thread.ID, card.AttemptID); !errors.Is(err, sql.ErrNoRows) {
					t.Fatal(err)
				}
				return
			}
			if rec := request(h, "POST", path+"/ticket-attempts/"+card.AttemptID+"/dismiss", "", "octocat", "on"); rec.Code != 404 {
				t.Fatal(rec.Code, rec.Body)
			}
		})
	}
}

type failingTicketCreator struct {
	tracker.Creator
	sent bool
}

func (c failingTicketCreator) Create(_ context.Context, draft tracker.Draft) (tracker.Issue, error) {
	if c.sent {
		if err := draft.BeforeSend(); err != nil {
			return tracker.Issue{}, err
		}
	}
	return tracker.Issue{}, errors.New("transport failed " + ticketSecret)
}

func TestTicketTransportFailureIsUnknownOnlyAfterSend(t *testing.T) {
	for _, sent := range []bool{false, true} {
		t.Run(fmt.Sprint(sent), func(t *testing.T) {
			s := testStore(t)
			thread := testThread(t, s)
			ctx := context.Background()
			m, err := s.beginAction(ctx, thread, "", "", "ticket")
			if err != nil {
				t.Fatal(err)
			}
			runner := &Runner{Store: s}
			err = runner.createTicket(ctx, thread, &m, nil, tools.TicketRequest{Title: "Draft", Body: "Evidence"}, "click", failingTicketCreator{sent: sent}, tracker.LinearTarget("Product", "", nil), []string{ticketSecret}, 1, true, "", func(string, any) error { return nil })
			if err != nil {
				t.Fatal(err)
			}
			_, _, card := latestTicket(t, s, thread)
			if card.OutcomeUnknown != sent || strings.Contains(card.Error, ticketSecret) {
				t.Fatal(card)
			}
		})
	}
}

func TestDismissInterruptedTicketAttempt(t *testing.T) {
	h, s := ticketFixture(t, nil, ticketHTTP(t, func(http.ResponseWriter, *http.Request, map[string]any) { t.Fatal("dismissal contacted Linear") }))
	thread := testThread(t, s)
	ctx := context.Background()
	m, err := s.beginAction(ctx, thread, "", "", "ticket")
	if err != nil {
		t.Fatal(err)
	}
	card := TicketCard{AttemptID: m.ID, Target: tracker.LinearTarget("Product", "", nil), TicketRequest: tools.TicketRequest{Title: "Draft", Body: "Evidence"}}
	if _, err := s.DB.Exec(`UPDATE ai_chat_tool_runs SET args_json=?,result_json='{"requestSent":true}' WHERE message_id=?`, string(redactedJSON(card, nil)), m.ID); err != nil {
		t.Fatal(err)
	}
	path := "/api/ai-chat/threads/" + thread.ID + "/ticket-attempts/" + m.ID + "/dismiss"
	if rec := request(h, "POST", path, "", "octocat", "on"); rec.Code != 204 {
		t.Fatal(rec.Code, rec.Body)
	}
	_, block, card := latestTicket(t, s, thread)
	if block.State != "failed" || !card.Dismissed || !card.OutcomeUnknown {
		t.Fatal(block, card)
	}
}
