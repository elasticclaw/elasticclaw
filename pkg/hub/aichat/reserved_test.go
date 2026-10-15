package aichat

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/elasticclaw/elasticclaw/pkg/hub/aichat/config"
	"github.com/elasticclaw/elasticclaw/pkg/hub/aichat/llm"
	"github.com/elasticclaw/elasticclaw/pkg/hub/aichat/tools"
)

const testInterviewArgs = `{"title":"Framing the problem","steps":[{"questions":[{"prompt":"When?","kind":"single","options":[{"label":"Planning"},{"label":"Shopping"},{"label":"Other"}]},{"prompt":"Who?","label":"Who","kind":"multi","options":[{"label":"Clients"},{"label":"Coaches"}]}]},{"questions":[{"prompt":"Where?","kind":"single","options":[{"label":"Home"},{"label":"Away"}]}]}]}`
const testBlockArgs = `{"type":"stat_tiles","data":{"tiles":[{"label":"Users","value":"42","caption":"PostHog"}]}}`

func TestOfferedToolSchemasHaveObjectRoots(t *testing.T) {
	cfg := &config.Config{
		KnowledgeBase: &config.KnowledgeBase{Repo: "acme/docs", Entry: "README.md"},
		Repositories:  []string{"acme/app"},
		PostHog:       &config.PostHog{Host: "https://posthog.test", ProjectID: "42", APIKey: "PH_KEY"},
		Datadog:       &config.Datadog{Site: "datadoghq.com", Env: "prod", APIKey: "DD_KEY", AppKey: "DD_APP"},
		IssueTracker:  &config.IssueTracker{},
	}
	deps := Deps{GitHubToken: func(string) string { return "test-token" }, Secret: func(string, string) (string, bool) { return "test-token", true }, LinearToken: func(string) (string, bool) { return "test-token", true }}
	var reads []tools.Tool
	for _, kind := range []string{"knowledge_base", "repositories", "posthog", "datadog", "issue_tracker"} {
		reads = append(reads, deps.candidate("workspace", cfg, config.Source{Kind: kind}).tools...)
	}
	registry, err := tools.NewReadRegistry(reads...)
	if err != nil {
		t.Fatal(err)
	}
	s := testStore(t)
	thread := testThread(t, s)
	ctx := context.Background()
	m, err := s.BeginTurn(ctx, thread, "question", "fake", false)
	if err != nil {
		t.Fatal(err)
	}
	runner := &Runner{Store: s, Reads: registry, AvailableActions: func(*config.Config) []string { return []string{"artifact", "ticket"} }}
	p := fakeProvider{stream: func(_ context.Context, req llm.Request, _ func(string) error) (llm.Response, error) {
		if len(req.Tools) != 16 {
			t.Fatalf("expected 12 read and 4 reserved tools, got %d", len(req.Tools))
		}
		for _, tool := range req.Tools {
			var schema map[string]json.RawMessage
			if err := json.Unmarshal(tool.InputSchema, &schema); err != nil {
				t.Fatal(tool.Name, err)
			}
			if string(schema["type"]) != `"object"` {
				t.Fatalf("%s: root must be an object", tool.Name)
			}
			for _, keyword := range []string{"oneOf", "anyOf", "allOf"} {
				if _, exists := schema[keyword]; exists {
					t.Errorf("%s: Anthropic rejects root %s", tool.Name, keyword)
				}
			}
		}
		return llm.Response{}, nil
	}}
	if err := runner.Run(ctx, thread, m, cfg, p, func(string, any) error { return nil }); err != nil {
		t.Fatal(err)
	}
}

func TestReservedRunnerBlocksAndHistory(t *testing.T) {
	s := testStore(t)
	thread := testThread(t, s)
	ctx := context.Background()
	m, _ := s.BeginTurn(ctx, thread, "show metrics", "fake", false)
	calls := 0
	var events []string
	var payloads []string
	p := fakeProvider{stream: func(_ context.Context, req llm.Request, emit func(string) error) (llm.Response, error) {
		calls++
		if len(req.Tools) != 2 || req.Tools[0].Name != tools.EmitBlock || req.Tools[1].Name != tools.AskInterview {
			t.Fatalf("tools: %+v", req.Tools)
		}
		if calls == 1 {
			if err := emit("A😀界"); err != nil {
				return llm.Response{}, err
			}
			return llm.Response{Text: "A😀界", ToolCalls: []llm.ToolCall{{ID: "b", Name: tools.EmitBlock, Arguments: testBlockArgs}}}, nil
		}
		if req.Messages[len(req.Messages)-1].Content != "Shown to the user." {
			t.Fatal(req.Messages)
		}
		return llm.Response{Text: "after"}, emit("after")
	}}
	runner := &Runner{Store: s}
	if err := runner.Run(ctx, thread, m, &config.Config{}, p, func(event string, payload any) error {
		events = append(events, event)
		raw, _ := json.Marshal(payload)
		payloads = append(payloads, string(raw))
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	messages, _ := s.Messages(ctx, thread.ID, 0)
	saved := messages[1]
	var blocks []tools.Block
	if err := json.Unmarshal(saved.Blocks, &blocks); err != nil {
		t.Fatal(err)
	}
	if len(blocks) != 1 || blocks[0].At != 3 || blocks[0].ID == "" || saved.Content != "A😀界after" {
		t.Fatalf("%+v %+v", saved, blocks)
	}
	if strings.Contains(strings.Join(events, ","), "tool_") || !strings.Contains(strings.Join(events, ","), EventBlock) || !strings.Contains(strings.Join(payloads, ","), `"at":3`) {
		t.Fatal(events, payloads)
	}
	var count int
	var summary, provider string
	if err := s.DB.QueryRow(`SELECT COUNT(*),summary,provider FROM ai_chat_tool_runs WHERE message_id=?`, m.ID).Scan(&count, &summary, &provider); err != nil || count != 1 || summary != "Showed stat tiles" || provider != "AI Chat" {
		t.Fatal(count, summary, provider, err)
	}
	if !strings.Contains(historyContent(saved), "Users: 42 (PostHog)") {
		t.Fatal(historyContent(saved))
	}
	next, _ := s.BeginTurn(ctx, thread, "remember?", "fake", false)
	p.stream = func(_ context.Context, req llm.Request, _ func(string) error) (llm.Response, error) {
		if !strings.Contains(req.Messages[1].Content, "Users: 42") {
			t.Fatal(req.Messages)
		}
		return llm.Response{}, nil
	}
	if err := runner.Run(ctx, thread, next, &config.Config{}, p, func(string, any) error { return nil }); err != nil {
		t.Fatal(err)
	}
}

func TestReservedBlockOffsetAfterRedactorTail(t *testing.T) {
	for _, tc := range []struct{ secret, after, wantAfter string }{
		{"ghp_x", "\n\nThree directions...", "\n\nThree directions..."},
		{"lin_api_x", "\n\nThree directions...", "\n\nThree directions..."},
		{"phx_x", "\n\nThree directions...", "\n\nThree directions..."},
		{"ghp_x", "hp_x", "[redacted]"},
	} {
		t.Run(tc.secret+tc.after, func(t *testing.T) {
			secret := tc.secret
			s := testStore(t)
			thread := testThread(t, s)
			ctx := context.Background()
			m, err := s.BeginTurn(ctx, thread, "show metrics", "fake", false)
			if err != nil {
				t.Fatal(err)
			}
			before := "😀 Here is what PostHog is showing"
			if secret[0] != 'g' {
				before = "😀 Result " + secret[:1]
			}
			runner := &Runner{Store: s, PrepareReads: func(_ context.Context, _ string, cfg *config.Config) (*tools.ReadRegistry, *config.Config, []string) {
				return nil, cfg, []string{secret}
			}}
			calls := 0
			p := fakeProvider{stream: func(_ context.Context, _ llm.Request, emit func(string) error) (llm.Response, error) {
				calls++
				if calls == 1 {
					return llm.Response{Text: before, ToolCalls: []llm.ToolCall{{ID: "b", Name: tools.EmitBlock, Arguments: testBlockArgs}}}, emit(before)
				}
				return llm.Response{Text: tc.after}, emit(tc.after)
			}}
			var streamed string
			var shown tools.Block
			if err := runner.Run(ctx, thread, m, &config.Config{}, p, func(event string, payload any) error {
				switch event {
				case EventToken:
					streamed += payload.(map[string]string)["text"]
				case EventBlock:
					if err := json.Unmarshal(payload.(json.RawMessage), &shown); err != nil {
						t.Fatal(err)
					}
					if streamed != before || shown.At != utf8.RuneCountInString(before) {
						t.Fatalf("block at %d after %q", shown.At, streamed)
					}
				}
				return nil
			}); err != nil {
				t.Fatal(err)
			}
			messages, err := s.Messages(ctx, thread.ID, 0)
			if err != nil {
				t.Fatal(err)
			}
			var blocks []tools.Block
			if err := json.Unmarshal(messages[1].Blocks, &blocks); err != nil {
				t.Fatal(err)
			}
			if calls != 2 || streamed != before+tc.wantAfter || messages[1].Content != streamed || len(blocks) != 1 || blocks[0].At != shown.At {
				t.Fatalf("calls=%d streamed=%q saved=%+v blocks=%+v", calls, streamed, messages[1], blocks)
			}
		})
	}
}

func TestInterruptedInterviewRejectsAnswers(t *testing.T) {
	for _, status := range []string{"cancelled", "error"} {
		t.Run(status, func(t *testing.T) {
			p := fakeProvider{stream: func(_ context.Context, _ llm.Request, _ func(string) error) (llm.Response, error) {
				return llm.Response{ToolCalls: []llm.ToolCall{{ID: "i", Name: tools.AskInterview, Arguments: testInterviewArgs}}}, nil
			}}
			handler, s, _ := apiFixture(t, p)
			thread := testThread(t, s)
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			m, err := s.BeginTurn(ctx, thread, "explore", "fake", false)
			if err != nil {
				t.Fatal(err)
			}
			var interview tools.Interview
			runner := &Runner{Store: s}
			writeErr := errors.New("SSE disconnected")
			err = runner.Run(ctx, thread, m, &config.Config{}, p, func(event string, payload any) error {
				if event == EventInterview {
					if err := json.Unmarshal(payload.(json.RawMessage), &interview); err != nil {
						t.Fatal(err)
					}
					if status == "cancelled" {
						cancel()
					} else {
						return writeErr
					}
				}
				return nil
			})
			if status == "error" && !errors.Is(err, writeErr) {
				t.Fatal(err)
			}
			messages, err := s.Messages(context.Background(), thread.ID, 0)
			if err != nil {
				t.Fatal(err)
			}
			if interview.QuestionSetID == "" || messages[1].Status != status || len(messages[1].Interview) != 0 {
				t.Fatalf("interview=%+v message=%+v", interview, messages[1])
			}
			raw, _ := json.Marshal(map[string]any{"answers": InterviewAnswers{QuestionSetID: interview.QuestionSetID, Steps: []InterviewStepAnswers{{Skipped: true}, {Skipped: true}}}})
			rec := request(handler, "POST", "/api/ai-chat/threads/"+thread.ID+"/messages", string(raw), "Octocat", "on")
			if rec.Code != 409 {
				t.Fatal(rec.Code, rec.Body.String())
			}
		})
	}
}

func TestReservedInterviewEndsTurnAndRedacts(t *testing.T) {
	s := testStore(t)
	thread := testThread(t, s)
	ctx := context.Background()
	m, _ := s.BeginTurn(ctx, thread, "explore", "fake", false)
	secret := "WORKSPACE_SENTINEL"
	args := strings.Replace(testInterviewArgs, "When?", secret+" When?", 1)
	args = strings.Replace(args, "Framing the problem", secret, 1)
	runner := &Runner{Store: s, PrepareReads: func(_ context.Context, _ string, cfg *config.Config) (*tools.ReadRegistry, *config.Config, []string) {
		return nil, cfg, []string{secret}
	}}
	count := 0
	var frames []string
	p := fakeProvider{stream: func(_ context.Context, _ llm.Request, emit func(string) error) (llm.Response, error) {
		count++
		if count > 1 {
			t.Fatal("model called after interview")
		}
		if err := emit("Before."); err != nil {
			return llm.Response{}, err
		}
		block := strings.Replace(testBlockArgs, "PostHog", secret, 1)
		return llm.Response{ToolCalls: []llm.ToolCall{{ID: "block", Name: tools.EmitBlock, Arguments: block}, {ID: "interview", Name: tools.AskInterview, Arguments: args}, {ID: "ignored", Name: "read_test", Arguments: `{}`}}}, nil
	}}
	if err := runner.Run(ctx, thread, m, &config.Config{}, p, func(event string, payload any) error {
		raw, _ := json.Marshal(payload)
		frames = append(frames, event+" "+string(raw))
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	messages, _ := s.History(ctx, thread.ID)
	saved := messages[1]
	var interview tools.Interview
	if err := json.Unmarshal(saved.Interview, &interview); err != nil {
		t.Fatal(err)
	}
	if saved.Status != "completed" || saved.Content != "Before." || interview.QuestionSetID == "" || interview.Answered || interview.Steps[0].Questions[0].ID != "q1" || !interview.Steps[0].Questions[0].Other || !interview.Steps[0].Skippable || len(interview.Steps[0].Questions[0].Options) != 2 {
		t.Fatalf("%+v %+v", saved, interview)
	}
	joined := strings.Join(frames, "\n") + string(saved.Blocks) + string(saved.Interview)
	var runs int
	var argsJSON string
	if err := s.DB.QueryRow(`SELECT COUNT(*),group_concat(args_json) FROM ai_chat_tool_runs WHERE message_id=?`, m.ID).Scan(&runs, &argsJSON); err != nil {
		t.Fatal(err)
	}
	if runs != 2 || strings.Contains(joined+argsJSON, secret) || strings.Contains(joined, "tool_started") || !strings.Contains(joined, `"step":1`) || !strings.Contains(joined, `"total":2`) {
		t.Fatal(runs, joined, argsJSON)
	}
	if !strings.Contains(historyContent(saved), "[Asked: [redacted] When? (Planning | Shopping | other)]") {
		t.Fatal(historyContent(saved))
	}
}

func TestReservedLimitsAndAvailability(t *testing.T) {
	for _, tc := range []struct {
		name       string
		available  []string
		tool, args string
		calls      int
		wantError  string
		wantBlocks int
	}{
		{"block limit", nil, tools.EmitBlock, testBlockArgs, 7, "Block limit reached.", 6},
		{"filtered actions", []string{"artifact"}, tools.OfferActions, `{"actions":["artifact","ticket"]}`, 1, "", 1},
		{"duplicate actions", []string{"artifact"}, tools.OfferActions, `{"actions":["artifact"]}`, 2, "Wrap-up already offered.", 1},
		{"actions unavailable", nil, tools.OfferActions, `{"actions":["artifact"]}`, 1, "Tool not available in this release.", 0},
		{"panel unavailable", nil, tools.SetPanel, `{}`, 1, "Tool not available in this release.", 0},
		{"tool limit", nil, tools.EmitBlock, testBlockArgs, 21, "Block limit reached.", 6},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := testStore(t)
			thread := testThread(t, s)
			ctx := context.Background()
			m, _ := s.BeginTurn(ctx, thread, "question", "fake", false)
			r := &Runner{Store: s, AvailableActions: func(*config.Config) []string { return tc.available }}
			n := 0
			p := fakeProvider{stream: func(_ context.Context, req llm.Request, _ func(string) error) (llm.Response, error) {
				n++
				for _, tool := range req.Tools {
					if tool.Name == tools.SetPanel || (tool.Name == tools.OfferActions && len(tc.available) == 0) {
						t.Fatal(req.Tools)
					}
				}
				if len(tc.available) > 0 && len(req.Tools) != 4 {
					t.Fatal(req.Tools)
				}
				if n == 1 {
					calls := []llm.ToolCall{}
					for i := 0; i < tc.calls; i++ {
						calls = append(calls, llm.ToolCall{ID: fmt.Sprint(i), Name: tc.tool, Arguments: tc.args})
					}
					return llm.Response{ToolCalls: calls}, nil
				}
				last := req.Messages[len(req.Messages)-1]
				if tc.wantError != "" && (!last.IsError || last.Content != tc.wantError) {
					t.Fatal(last)
				}
				return llm.Response{}, nil
			}}
			err := r.Run(ctx, thread, m, &config.Config{}, p, func(event string, _ any) error {
				if event == EventToolStarted || event == EventToolFinished {
					t.Fatal(event)
				}
				return nil
			})
			if tc.calls > MaxToolCalls {
				if !errors.Is(err, errLimit) {
					t.Fatal(err)
				}
			} else if err != nil {
				t.Fatal(err)
			}
			messages, _ := s.Messages(ctx, thread.ID, 0)
			var blocks []tools.Block
			_ = json.Unmarshal(messages[1].Blocks, &blocks)
			if len(blocks) != tc.wantBlocks {
				t.Fatal(blocks)
			}
			if len(tc.available) > 0 && len(blocks) > 0 && (len(blocks[0].Actions) != 1 || blocks[0].Actions[0] != "artifact") {
				t.Fatal(blocks)
			}
			var count int
			s.DB.QueryRow(`SELECT COUNT(*) FROM ai_chat_tool_runs WHERE message_id=?`, m.ID).Scan(&count)
			if count != min(tc.calls, MaxToolCalls) {
				t.Fatal(count)
			}
		})
	}
}

func TestInterviewAnswersAPI(t *testing.T) {
	for _, scenario := range []string{"happy", "skip all", "wrong set", "invalid choice", "single plus other", "duplicate question", "missing step", "long other", "text supersedes", "cancelled", "error", "limit"} {
		t.Run(scenario, func(t *testing.T) {
			calls := 0
			p := fakeProvider{stream: func(_ context.Context, req llm.Request, _ func(string) error) (llm.Response, error) {
				calls++
				if !strings.Contains(req.Messages[len(req.Messages)-1].Content, "Answers to") && scenario != "text supersedes" {
					t.Fatal(req.Messages)
				}
				return llm.Response{}, nil
			}}
			handler, s, _ := apiFixture(t, p)
			thread := testThread(t, s)
			ctx := context.Background()
			m, _ := s.BeginTurn(ctx, thread, "explore", "fake", false)
			interview, err := tools.ParseInterview(json.RawMessage(testInterviewArgs))
			if err != nil {
				t.Fatal(err)
			}
			interview.QuestionSetID = "set-1"
			m.Interview, _ = json.Marshal(interview)
			m.Status = "completed"
			if scenario == "cancelled" || scenario == "error" || scenario == "limit" {
				m.Status = scenario
			}
			if err := s.FinishMessage(ctx, m); err != nil {
				t.Fatal(err)
			}
			answers := InterviewAnswers{QuestionSetID: "set-1", Steps: []InterviewStepAnswers{
				{Answers: []InterviewAnswer{{QuestionID: "q1", Choices: []string{"Shopping"}}, {QuestionID: "q2", Choices: []string{"Clients", "Coaches"}, Other: "  Friends  "}}},
				{Skipped: true},
			}}
			want := 200
			switch scenario {
			case "cancelled", "error", "limit":
				want = 409
			case "skip all":
				answers.Steps[0] = InterviewStepAnswers{Skipped: true}
			case "wrong set":
				answers.QuestionSetID = "stale"
				want = 409
			case "invalid choice":
				answers.Steps[0].Answers[0].Choices = []string{"Not an option"}
				want = 400
			case "single plus other":
				answers.Steps[0].Answers[0].Other = "Other answer"
				want = 400
			case "duplicate question":
				answers.Steps[0].Answers[1].QuestionID = "q1"
				want = 400
			case "missing step":
				answers.Steps = answers.Steps[:1]
				want = 400
			case "long other":
				answers.Steps[0].Answers[1].Other = strings.Repeat("界", 301)
				want = 400
			case "text supersedes":
				rec := request(handler, "POST", "/api/ai-chat/threads/"+thread.ID+"/messages", `{"text":"another topic"}`, "Octocat", "on")
				if rec.Code != 200 {
					t.Fatal(rec.Code, rec.Body.String())
				}
				want = 409
			}
			raw, _ := json.Marshal(map[string]any{"answers": answers})
			rec := request(handler, "POST", "/api/ai-chat/threads/"+thread.ID+"/messages", string(raw), "Octocat", "on")
			if rec.Code != want {
				t.Fatal(rec.Code, rec.Body.String())
			}
			messages, _ := s.Messages(ctx, thread.ID, 0)
			var stored tools.Interview
			_ = json.Unmarshal(messages[1].Interview, &stored)
			if want != 200 {
				if stored.Answered {
					t.Fatal("invalid submission marked answered")
				}
				if scenario != "text supersedes" && (len(messages) != 2 || calls != 0) {
					t.Fatal(messages, calls)
				}
				return
			}
			if !stored.Answered || len(messages) != 4 {
				t.Fatal(messages)
			}
			var summary AnswersSummary
			if err := json.Unmarshal(messages[2].Interview, &summary); err != nil {
				t.Fatal(err)
			}
			if scenario == "happy" {
				if len(summary.Answers) != 2 || summary.Answers[0].Question != "When?" || summary.Answers[1].Question != "Who" || summary.Answers[1].Answer != "Clients, Coaches, Friends" || summary.Skipped != 1 || !strings.Contains(messages[2].Content, "- When? Shopping") || !strings.Contains(messages[2].Content, "(Skipped step 2)") {
					t.Fatal(summary, messages[2])
				}
			} else if summary.Skipped != 2 || len(summary.Answers) != 0 {
				t.Fatal(summary)
			}
			rec = request(handler, "POST", "/api/ai-chat/threads/"+thread.ID+"/messages", string(raw), "Octocat", "on")
			if rec.Code != 409 {
				t.Fatal("double answer", rec.Code)
			}
		})
	}
}

func TestReservedRedactionCollisionsAndInvalidArgs(t *testing.T) {
	for _, scenario := range []string{"id collision", "label collision", "malformed"} {
		t.Run(scenario, func(t *testing.T) {
			s := testStore(t)
			thread := testThread(t, s)
			ctx := context.Background()
			m, _ := s.BeginTurn(ctx, thread, "question", "fake", false)
			args := testInterviewArgs
			switch scenario {
			case "id collision":
				args = strings.Replace(args, `"prompt":"When?"`, `"id":"SECRET_ONE","prompt":"When?"`, 1)
				args = strings.Replace(args, `"prompt":"Who?"`, `"id":"SECRET_TWO","prompt":"Who?"`, 1)
			case "label collision":
				args = strings.Replace(args, "Planning", "SECRET_ONE", 1)
				args = strings.Replace(args, "Shopping", "SECRET_TWO", 1)
			case "malformed":
				args = `{"title":"SECRET_ONE"`
			}
			r := &Runner{Store: s}
			result, stop, err := r.runReserved(ctx, &m, 1, llm.ToolCall{ID: "x", Name: tools.AskInterview, Arguments: args}, func(string, any) error { t.Fatal("invalid interview emitted"); return nil }, nil, []string{"SECRET_ONE", "SECRET_TWO"}, 0)
			if err != nil || stop || !result.IsError || len(m.Interview) > 0 {
				t.Fatal(result, stop, err, m)
			}
		})
	}
}

func TestReservedHistoryCalloutTableAndRenderingCap(t *testing.T) {
	blocks := []tools.Block{
		{Type: "callout", Data: json.RawMessage(`{"tone":"info","title":"Notice","text":"Grounded finding"}`)},
		{Type: "table", Data: json.RawMessage(`{"columns":["Metric","Count"],"rows":[["Users","42"]]}`)},
	}
	raw, _ := json.Marshal(blocks)
	content := historyContent(Message{Role: "assistant", Content: "Answer", Blocks: raw})
	if !strings.Contains(content, "Notice: Grounded finding") || !strings.Contains(content, "Metric | Count\nUsers | 42") {
		t.Fatal(content)
	}
	if len(capRendering(strings.Repeat("界", 1000))) > 1027 {
		t.Fatal("rendering not capped")
	}
}

func TestReservedHistoryActionsAndTileCaptions(t *testing.T) {
	blocks := []tools.Block{
		{Type: "stat_tiles", Data: json.RawMessage(`{"tiles":[{"label":"Users","value":"42"},{"label":"Sessions","value":"84","caption":"PostHog"}]}`)},
		{Type: "actions", Actions: []string{"artifact", "ticket"}},
	}
	raw, _ := json.Marshal(blocks)
	got := historyContent(Message{Role: "assistant", Content: "Answer", Blocks: raw})
	want := "Answer\n[Shown: Users: 42; Sessions: 84 (PostHog); ]\n[Offered wrap-up: artifact, ticket]"
	if got != want {
		t.Fatalf("got %q, want %q", got, want)
	}
}
