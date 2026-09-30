package aichat

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/elasticclaw/elasticclaw/pkg/hub/aichat/config"
	"github.com/elasticclaw/elasticclaw/pkg/hub/aichat/llm"
	"github.com/elasticclaw/elasticclaw/pkg/hub/aichat/tools"
)

func testStore(t *testing.T) *Store {
	t.Helper()
	db, err := sql.Open("sqlite", filepath.Join(t.TempDir(), "chat.db")+"?_pragma=foreign_keys(on)&_pragma=busy_timeout(5000)")
	if err != nil {
		t.Fatal(err)
	}
	db.SetMaxOpenConns(1)
	t.Cleanup(func() { db.Close() })
	if err := Migrate(context.Background(), db); err != nil {
		t.Fatal(err)
	}
	return &Store{DB: db}
}
func testThread(t *testing.T, s *Store) Thread {
	t.Helper()
	thread, err := s.CreateThread(context.Background(), "tenant", "OctoCat", "workspace", "explore_idea")
	if err != nil {
		t.Fatal(err)
	}
	return thread
}

type fakeProvider struct {
	stream func(context.Context, llm.Request, func(string) error) (llm.Response, error)
}

func (p fakeProvider) Model() string { return "fake-model" }
func (p fakeProvider) Stream(ctx context.Context, req llm.Request, emit func(string) error) (llm.Response, error) {
	return p.stream(ctx, req, emit)
}

type fakeRead struct {
	run func(context.Context, json.RawMessage) (tools.Result, error)
}

func (f fakeRead) Definition() llm.Tool {
	return llm.Tool{Name: "read_test", Description: "Read test data", InputSchema: json.RawMessage(`{"type":"object"}`)}
}
func (f fakeRead) Provider() string { return "fake" }
func (f fakeRead) Run(ctx context.Context, args json.RawMessage) (tools.Result, error) {
	return f.run(ctx, args)
}

func TestStoreRoundTrips(t *testing.T) {
	ctx := context.Background()
	s := testStore(t)
	thread := testThread(t, s)
	if thread.OwnerLogin != "octocat" {
		t.Fatal(thread)
	}
	for _, identity := range [][2]string{{"tenant", "other"}, {"other", "octocat"}} {
		if _, err := s.Thread(ctx, identity[0], identity[1], thread.ID); !errors.Is(err, sql.ErrNoRows) {
			t.Fatal(err)
		}
	}
	assistant, err := s.BeginTurn(ctx, thread, "  First question\nwith context  ", "model", false)
	if err != nil {
		t.Fatal(err)
	}
	assistant.Content = "answer"
	assistant.Status = "error"
	assistant.InputTokens = 12
	assistant.OutputTokens = 7
	if err := s.FinishMessage(ctx, assistant); err != nil {
		t.Fatal(err)
	}
	saved, err := s.Thread(ctx, "tenant", "OCTOCAT", thread.ID)
	if err != nil || saved.Title != "First question with context" {
		t.Fatalf("%+v %v", saved, err)
	}
	messages, err := s.Messages(ctx, thread.ID, 0)
	if err != nil || len(messages) != 2 || messages[1].InputTokens != 12 || messages[1].Status != "error" {
		t.Fatalf("%+v %v", messages, err)
	}
	retry, err := s.BeginTurn(ctx, thread, "", "model", true)
	if err != nil || retry.Seq != 3 {
		t.Fatalf("%+v %v", retry, err)
	}
	retry.Status = "completed"
	if err := s.FinishMessage(ctx, retry); err != nil {
		t.Fatal(err)
	}
	if _, err := s.BeginTurn(ctx, thread, "", "model", true); !errors.Is(err, ErrRetry) {
		t.Fatal(err)
	}
	title, archived := "Renamed", true
	if err := s.UpdateThread(ctx, thread, &title, &archived); err != nil {
		t.Fatal(err)
	}
	threads, err := s.Threads(ctx, "tenant", "octocat")
	if err != nil || len(threads) != 0 {
		t.Fatalf("%+v %v", threads, err)
	}
	saved, err = s.Thread(ctx, "tenant", "octocat", thread.ID)
	if err != nil || saved.ArchivedAt == nil || saved.Title != title {
		t.Fatalf("%+v %v", saved, err)
	}
	archived = false
	if err := s.UpdateThread(ctx, saved, nil, &archived); err != nil {
		t.Fatal(err)
	}
	threads, err = s.Threads(ctx, "tenant", "octocat")
	if err != nil || len(threads) != 1 {
		t.Fatalf("%+v %v", threads, err)
	}
}

func TestRunnerToolLoopAndRecording(t *testing.T) {
	s := testStore(t)
	thread := testThread(t, s)
	read := fakeRead{run: func(ctx context.Context, args json.RawMessage) (tools.Result, error) {
		deadline, ok := ctx.Deadline()
		if !ok || time.Until(deadline) > tools.Timeout {
			t.Error("missing tool timeout")
		}
		if string(args) != `{"query":"x"}` {
			t.Errorf("args=%s", args)
		}
		return tools.Result{Summary: "Two rows", Data: strings.Repeat("x", 40000), RowCount: 2}, nil
	}}
	registry, err := tools.NewReadRegistry(read)
	if err != nil {
		t.Fatal(err)
	}
	runner := &Runner{Store: s, Reads: registry}
	count := 0
	p := fakeProvider{stream: func(ctx context.Context, req llm.Request, emit func(string) error) (llm.Response, error) {
		count++
		if len(req.Tools) != 1 || req.Tools[0].Name != "read_test" {
			t.Errorf("tools=%+v", req.Tools)
		}
		if count == 1 {
			if err := emit("Checking. "); err != nil {
				return llm.Response{}, err
			}
			return llm.Response{Text: "Checking. ", ToolCalls: []llm.ToolCall{{ID: "call", Name: "read_test", Arguments: `{"query":"x"}`}}, InputTokens: 5, OutputTokens: 2}, nil
		}
		last := req.Messages[len(req.Messages)-1]
		if last.ToolCallID != "call" || len(last.Content) > tools.MaxResultBytes || !strings.Contains(last.Content, "[truncated]") {
			t.Errorf("invalid result: %d", len(last.Content))
		}
		return llm.Response{Text: "Answer", InputTokens: 9, OutputTokens: 3}, emit("Answer")
	}}
	m, err := s.BeginTurn(context.Background(), thread, "question", p.Model(), false)
	if err != nil {
		t.Fatal(err)
	}
	var events []string
	if err := runner.Run(context.Background(), thread, m, &config.Config{}, p, func(event string, _ any) error { events = append(events, event); return nil }); err != nil {
		t.Fatal(err)
	}
	want := []string{EventMessageStarted, EventToken, EventToolStarted, EventToolFinished, EventToken, EventDone}
	if !reflect.DeepEqual(events, want) {
		t.Fatalf("events=%v", events)
	}
	messages, _ := s.Messages(context.Background(), thread.ID, 0)
	got := messages[1]
	if got.Content != "Checking. Answer" || got.Model != "fake-model" || got.InputTokens != 14 || got.OutputTokens != 5 || got.Status != "completed" {
		t.Fatalf("message=%+v", got)
	}
	var tool, provider, summary, result string
	var rows int
	if err := s.DB.QueryRow(`SELECT tool,provider,summary,result_json,row_count FROM ai_chat_tool_runs WHERE message_id=?`, m.ID).Scan(&tool, &provider, &summary, &result, &rows); err != nil {
		t.Fatal(err)
	}
	if tool != "read_test" || provider != "fake" || summary != "Two rows" || rows != 2 || !json.Valid([]byte(result)) || len(result) < 40000 {
		t.Fatalf("run=%s %s %s %d", tool, provider, summary, rows)
	}
}

func TestRunnerLimits(t *testing.T) {
	for _, tc := range []struct {
		name                            string
		batch, wantIterations, wantRuns int
	}{{"iterations", 1, 12, 12}, {"tools", 3, 7, 20}} {
		t.Run(tc.name, func(t *testing.T) {
			s := testStore(t)
			thread := testThread(t, s)
			runner := &Runner{Store: s}
			iterations := 0
			p := fakeProvider{stream: func(_ context.Context, _ llm.Request, _ func(string) error) (llm.Response, error) {
				iterations++
				calls := []llm.ToolCall{}
				for i := 0; i < tc.batch; i++ {
					calls = append(calls, llm.ToolCall{ID: fmt.Sprintf("%d-%d", iterations, i), Name: "unknown", Arguments: `{}`})
				}
				return llm.Response{ToolCalls: calls}, nil
			}}
			m, _ := s.BeginTurn(context.Background(), thread, "question", p.Model(), false)
			if err := runner.Run(context.Background(), thread, m, &config.Config{}, p, func(string, any) error { return nil }); !errors.Is(err, errLimit) {
				t.Fatal(err)
			}
			var runs int
			if err := s.DB.QueryRow(`SELECT count(*) FROM ai_chat_tool_runs`).Scan(&runs); err != nil {
				t.Fatal(err)
			}
			if iterations != tc.wantIterations || runs != tc.wantRuns {
				t.Fatalf("iterations=%d runs=%d", iterations, runs)
			}
			messages, _ := s.Messages(context.Background(), thread.ID, 0)
			if messages[1].Status != "limit" {
				t.Fatal(messages[1])
			}
		})
	}
}

func TestRunnerCancellationAndConcurrency(t *testing.T) {
	s := testStore(t)
	runner := &Runner{Store: s}
	threads := []Thread{}
	releases := []func(){}
	for i := 0; i < 4; i++ {
		threads = append(threads, testThread(t, s))
	}
	for i := 0; i < 3; i++ {
		_, release, err := runner.Acquire(context.Background(), threads[i])
		if err != nil {
			t.Fatal(err)
		}
		releases = append(releases, release)
	}
	for _, thread := range []Thread{threads[0], threads[3]} {
		if _, _, err := runner.Acquire(context.Background(), thread); !errors.Is(err, ErrBusy) {
			t.Fatal(err)
		}
	}
	for _, release := range releases {
		release()
		release()
	}
	ctx, release, err := runner.Acquire(context.Background(), threads[0])
	if err != nil {
		t.Fatal(err)
	}
	defer release()
	started := make(chan struct{})
	finished := make(chan error, 1)
	p := fakeProvider{stream: func(ctx context.Context, _ llm.Request, emit func(string) error) (llm.Response, error) {
		if err := emit("Partial"); err != nil {
			return llm.Response{}, err
		}
		close(started)
		<-ctx.Done()
		return llm.Response{InputTokens: 4}, ctx.Err()
	}}
	m, _ := s.BeginTurn(ctx, threads[0], "question", p.Model(), false)
	go func() {
		finished <- runner.Run(ctx, threads[0], m, &config.Config{}, p, func(string, any) error { return nil })
	}()
	<-started
	other := threads[0]
	other.OwnerLogin = "other"
	runner.Cancel(other)
	if ctx.Err() != nil {
		t.Fatal("foreign owner cancelled turn")
	}
	runner.Cancel(threads[0])
	if err := <-finished; !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	messages, _ := s.Messages(context.Background(), threads[0].ID, 0)
	if messages[1].Status != "cancelled" || messages[1].Content != "Partial" || messages[1].InputTokens != 4 {
		t.Fatal(messages)
	}
}

func TestPromptAndHistory(t *testing.T) {
	s := testStore(t)
	thread := testThread(t, s)
	for i := 0; i < 25; i++ {
		m, err := s.BeginTurn(context.Background(), thread, fmt.Sprintf("question%d", i), "model", false)
		if err != nil {
			t.Fatal(err)
		}
		m.Content = "answer"
		m.Status = "completed"
		if err := s.FinishMessage(context.Background(), m); err != nil {
			t.Fatal(err)
		}
	}
	cfg := &config.Config{About: "about-marker", Modes: map[string]config.Mode{"explore_idea": {Prompt: "mode-marker"}}, Repositories: []string{"org/repo"}, PostHog: &config.PostHog{APIKey: "SECRET_MUST_NOT_APPEAR", ProjectID: "p"}}
	p := fakeProvider{stream: func(_ context.Context, req llm.Request, _ func(string) error) (llm.Response, error) {
		if len(req.Messages) > 40 || req.Messages[0].Role != "user" || req.Messages[len(req.Messages)-1].Content != "latest" || strings.Contains(fmt.Sprint(req.Messages), "question0") {
			t.Fatalf("history=%+v", req.Messages)
		}
		positions := []int{strings.Index(req.System, basePrompt), strings.Index(req.System, "about-marker"), strings.Index(req.System, "mode-marker"), strings.Index(req.System, "org/repo")}
		for i, pos := range positions {
			if pos < 0 || (i > 0 && pos <= positions[i-1]) {
				t.Fatal(positions)
			}
		}
		if strings.Contains(req.System, "SECRET_MUST_NOT_APPEAR") || len(req.Tools) != 0 {
			t.Fatal("secret or reserved tools in prompt")
		}
		return llm.Response{}, nil
	}}
	m, _ := s.BeginTurn(context.Background(), thread, "latest", p.Model(), false)
	if err := (&Runner{Store: s}).Run(context.Background(), thread, m, cfg, p, func(string, any) error { return nil }); err != nil {
		t.Fatal(err)
	}
}

func apiFixture(t *testing.T, p llm.Provider) (http.Handler, *Store, *[]Thread) {
	t.Helper()
	s := testStore(t)
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "ai_chat.yaml"), []byte("about: Test"), 0600); err != nil {
		t.Fatal(err)
	}
	published := &[]Thread{}
	var mu sync.Mutex
	deps := Deps{DB: s.DB, WebAuth: func(next http.HandlerFunc) http.HandlerFunc {
		return func(w http.ResponseWriter, r *http.Request) {
			if r.Header.Get("Login") == "" {
				http.Error(w, "unauthorized", 401)
				return
			}
			next(w, r)
		}
	}, WithFeature: func(key string, next http.HandlerFunc) http.HandlerFunc {
		return func(w http.ResponseWriter, r *http.Request) {
			if key != "ai-chat" || r.Header.Get("Feature") != "on" {
				http.NotFound(w, r)
				return
			}
			next(w, r)
		}
	}, CallerLogin: func(r *http.Request) string { return r.Header.Get("Login") }, TenantID: func(*http.Request) string { return "tenant" }, Workspaces: func() ([]string, error) { return []string{"workspace"}, nil }, ManagedDir: func(string) string { return dir }, LLM: func(string) (llm.Provider, error) { return p, nil }, Publish: func(thread Thread) { mu.Lock(); defer mu.Unlock(); *published = append(*published, thread) }}
	return Routes(deps), s, published
}
func request(handler http.Handler, method, path, body, login, flag string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	req.Header.Set("Login", login)
	req.Header.Set("Feature", flag)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	return rec
}
func TestConversationAPI(t *testing.T) {
	p := fakeProvider{stream: func(_ context.Context, _ llm.Request, emit func(string) error) (llm.Response, error) {
		return llm.Response{Text: "Hello", InputTokens: 2, OutputTokens: 1}, emit("Hello")
	}}
	handler, s, published := apiFixture(t, p)
	rec := request(handler, "POST", "/api/ai-chat/threads", `{"workspace":"workspace","mode":"ask_the_data"}`, "Alice", "on")
	if rec.Code != 201 {
		t.Fatal(rec.Code, rec.Body.String())
	}
	var thread Thread
	if err := json.Unmarshal(rec.Body.Bytes(), &thread); err != nil {
		t.Fatal(err)
	}
	path := "/api/ai-chat/threads/" + thread.ID
	for _, route := range []struct{ method, path, body string }{{"GET", path, ""}, {"PATCH", path, `{"title":"private"}`}, {"POST", path + "/messages", `{"text":"secret"}`}, {"POST", path + "/cancel", ""}} {
		if rec := request(handler, route.method, route.path, route.body, "Bob", "on"); rec.Code != 404 {
			t.Fatalf("owner gate %s: %d", route.path, rec.Code)
		}
		if rec := request(handler, route.method, route.path, route.body, "Alice", "off"); rec.Code != 404 {
			t.Fatalf("flag gate %s: %d", route.path, rec.Code)
		}
	}
	for _, path := range []string{"/api/ai-chat/threads", "/api/ai-chat/sources", "/api/ai-chat/future"} {
		if rec := request(handler, "GET", path, "", "Alice", "off"); rec.Code != 404 {
			t.Fatal(rec.Code)
		}
		if rec := request(handler, "GET", path, "", "", "on"); rec.Code != 401 {
			t.Fatal(rec.Code)
		}
	}
	rec = request(handler, "POST", path+"/messages", `{"text":"First question"}`, "Alice", "on")
	if rec.Code != 200 || rec.Header().Get("Content-Type") != "text/event-stream" {
		t.Fatal(rec.Code, rec.Body.String())
	}
	var events []string
	for _, line := range strings.Split(rec.Body.String(), "\n") {
		if strings.HasPrefix(line, "event: ") {
			events = append(events, strings.TrimPrefix(line, "event: "))
		}
	}
	if !reflect.DeepEqual(events, []string{EventMessageStarted, EventToken, EventDone}) {
		t.Fatal(events)
	}
	saved, err := s.Thread(context.Background(), "tenant", "alice", thread.ID)
	if err != nil || saved.Title != "First question" {
		t.Fatal(saved, err)
	}
	if len(*published) != 2 || (*published)[1].OwnerLogin != "alice" {
		t.Fatal(*published)
	}
	rec = request(handler, "GET", path, "", "Alice", "on")
	if rec.Code != 200 || !strings.Contains(rec.Body.String(), "Hello") || !strings.Contains(rec.Body.String(), `"inputTokens":2`) {
		t.Fatal(rec.Code, rec.Body.String())
	}
	rec = request(handler, "PATCH", path, `{"title":"Updated","archived":true}`, "Alice", "on")
	if rec.Code != 204 {
		t.Fatal(rec.Code, rec.Body.String())
	}
	rec = request(handler, "POST", path+"/messages", `{"text":"No"}`, "Alice", "on")
	if rec.Code != 409 {
		t.Fatal(rec.Code)
	}
	rec = request(handler, "POST", "/api/ai-chat/threads", `{"workspace":"../private"}`, "Alice", "on")
	if rec.Code != 400 {
		t.Fatal(rec.Code)
	}
}

func TestAPIConcurrencyAndCancel(t *testing.T) {
	started := make(chan struct{}, 4)
	p := fakeProvider{stream: func(ctx context.Context, _ llm.Request, _ func(string) error) (llm.Response, error) {
		started <- struct{}{}
		<-ctx.Done()
		return llm.Response{}, ctx.Err()
	}}
	handler, s, _ := apiFixture(t, p)
	threads := []Thread{}
	for i := 0; i < 4; i++ {
		threads = append(threads, testThread(t, s))
	}
	var wg sync.WaitGroup
	for i := 0; i < 3; i++ {
		wg.Add(1)
		go func(id string) {
			defer wg.Done()
			rec := request(handler, "POST", "/api/ai-chat/threads/"+id+"/messages", `{"text":"question"}`, "octocat", "on")
			if rec.Code != 200 {
				t.Errorf("stream=%d", rec.Code)
			}
		}(threads[i].ID)
		<-started
	}
	for _, thread := range []Thread{threads[0], threads[3]} {
		rec := request(handler, "POST", "/api/ai-chat/threads/"+thread.ID+"/messages", `{"text":"question"}`, "octocat", "on")
		if rec.Code != 429 {
			t.Errorf("busy=%d", rec.Code)
		}
	}
	for _, thread := range threads[:3] {
		rec := request(handler, "POST", "/api/ai-chat/threads/"+thread.ID+"/cancel", "", "octocat", "on")
		if rec.Code != 204 {
			t.Errorf("cancel=%d", rec.Code)
		}
	}
	wg.Wait()
}

func TestRetryHistorySurvivesFailures(t *testing.T) {
	s := testStore(t)
	thread := testThread(t, s)
	for i := 0; i < 45; i++ {
		m, err := s.BeginTurn(context.Background(), thread, "original question", "model", i > 0)
		if err != nil {
			t.Fatal(err)
		}
		m.Status = "error"
		if err := s.FinishMessage(context.Background(), m); err != nil {
			t.Fatal(err)
		}
	}
	history, err := s.History(context.Background(), thread.ID)
	if err != nil || len(history) != 1 || history[0].Content != "original question" {
		t.Fatalf("history=%+v err=%v", history, err)
	}
}

func TestRunnerErrorAndDeadlineSaveStatus(t *testing.T) {
	for _, tc := range []struct {
		name, code string
		deadline   bool
	}{{"provider", "turn_failed", false}, {"deadline", "timeout", true}} {
		t.Run(tc.name, func(t *testing.T) {
			s := testStore(t)
			thread := testThread(t, s)
			m, err := s.BeginTurn(context.Background(), thread, "question", "model", false)
			if err != nil {
				t.Fatal(err)
			}
			ctx := context.Background()
			if tc.deadline {
				var cancel context.CancelFunc
				ctx, cancel = context.WithDeadline(ctx, time.Now().Add(-time.Second))
				defer cancel()
			}
			provider := fakeProvider{stream: func(_ context.Context, _ llm.Request, emit func(string) error) (llm.Response, error) {
				_ = emit("partial")
				return llm.Response{InputTokens: 3, OutputTokens: 1}, errors.New("sensitive provider error")
			}}
			var events []string
			err = (&Runner{Store: s}).Run(ctx, thread, m, &config.Config{}, provider, func(event string, payload any) error {
				events = append(events, event)
				if event == EventError && payload.(map[string]string)["code"] != tc.code {
					t.Errorf("error=%+v", payload)
				}
				if strings.Contains(fmt.Sprint(payload), "sensitive") {
					t.Error("provider error leaked")
				}
				return nil
			})
			if err == nil || events[0] != EventMessageStarted || events[len(events)-2] != EventError || events[len(events)-1] != EventDone {
				t.Fatalf("events=%v err=%v", events, err)
			}
			messages, _ := s.Messages(context.Background(), thread.ID, 0)
			if messages[1].Status != "error" {
				t.Fatal(messages)
			}
		})
	}
}

func TestToolFailureIsRecordedWithoutProviderError(t *testing.T) {
	s := testStore(t)
	thread := testThread(t, s)
	m, err := s.BeginTurn(context.Background(), thread, "question", "model", false)
	if err != nil {
		t.Fatal(err)
	}
	registry, err := tools.NewReadRegistry(fakeRead{run: func(context.Context, json.RawMessage) (tools.Result, error) {
		return tools.Result{}, errors.New("secret-provider-response")
	}})
	if err != nil {
		t.Fatal(err)
	}
	runner := &Runner{Store: s, Reads: registry}
	result, err := runner.runTool(context.Background(), m.ID, 1, llm.ToolCall{ID: "call", Name: "read_test", Arguments: `{}`}, func(string, any) error { return nil })
	if err != nil || !result.IsError || strings.Contains(result.Content, "secret") {
		t.Fatal(result, err)
	}
	var stored string
	if err := s.DB.QueryRow(`SELECT error FROM ai_chat_tool_runs`).Scan(&stored); err != nil {
		t.Fatal(err)
	}
	if stored != "Read tool failed" {
		t.Fatal(stored)
	}
}

func TestThreadPatchPreservesUnspecifiedFields(t *testing.T) {
	for _, archiveFirst := range []bool{false, true} {
		t.Run(fmt.Sprint(archiveFirst), func(t *testing.T) {
			s := testStore(t)
			thread := testThread(t, s)
			ctx := context.Background()
			title, archived := "Renamed", true
			rename := func() error { return s.UpdateThread(ctx, thread, &title, nil) }
			archive := func() error { return s.UpdateThread(ctx, thread, nil, &archived) }
			first, second := rename, archive
			if archiveFirst {
				first, second = archive, rename
			}
			// Both requests loaded the same snapshot before either update.
			if err := first(); err != nil {
				t.Fatal(err)
			}
			if err := second(); err != nil {
				t.Fatal(err)
			}
			saved, err := s.Thread(ctx, thread.TenantID, thread.OwnerLogin, thread.ID)
			if err != nil || saved.Title != title || saved.ArchivedAt == nil {
				t.Fatalf("thread=%+v err=%v", saved, err)
			}
			archived = false
			if err := s.UpdateThread(ctx, thread, nil, &archived); err != nil {
				t.Fatal(err)
			}
			saved, err = s.Thread(ctx, thread.TenantID, thread.OwnerLogin, thread.ID)
			if err != nil || saved.Title != title || saved.ArchivedAt != nil {
				t.Fatalf("unarchived=%+v err=%v", saved, err)
			}
		})
	}
}

func TestInterruptedTurnRecovery(t *testing.T) {
	for _, reload := range []bool{false, true} {
		t.Run(fmt.Sprint(reload), func(t *testing.T) {
			p := fakeProvider{stream: func(_ context.Context, req llm.Request, emit func(string) error) (llm.Response, error) {
				if len(req.Messages) != 1 || req.Messages[0].Content != "original" {
					t.Errorf("retry history=%+v", req.Messages)
				}
				return llm.Response{Text: "recovered"}, emit("recovered")
			}}
			handler, s, _ := apiFixture(t, p)
			thread := testThread(t, s)
			ctx := context.Background()
			m, err := s.BeginTurn(ctx, thread, "original", "model", false)
			if err != nil {
				t.Fatal(err)
			}
			m.Content, m.InputTokens = "partial", 12
			if err := s.FinishMessage(ctx, m); err != nil {
				t.Fatal(err)
			}
			// Migration-only DB openers must not interrupt a serving process.
			if err := Migrate(context.Background(), s.DB); err != nil {
				t.Fatal(err)
			}
			messages, err := s.Messages(ctx, thread.ID, 0)
			if err != nil || messages[1].Status != "streaming" {
				t.Fatalf("%+v %v", messages, err)
			}
			path := "/api/ai-chat/threads/" + thread.ID
			if reload {
				rec := request(handler, "GET", path, "", "octocat", "on")
				var body struct {
					Messages []Message `json:"messages"`
				}
				if rec.Code != 200 || json.Unmarshal(rec.Body.Bytes(), &body) != nil || len(body.Messages) != 2 || body.Messages[1].Status != "error" {
					t.Fatalf("reload=%d %s", rec.Code, rec.Body.String())
				}
			}
			rec := request(handler, "POST", path+"/messages", `{"retry":true}`, "octocat", "on")
			if rec.Code != 200 {
				t.Fatalf("retry=%d %s", rec.Code, rec.Body.String())
			}
			messages, err = s.Messages(ctx, thread.ID, 0)
			if err != nil || len(messages) != 3 || messages[1].Status != "error" || messages[1].Content != "partial" || messages[1].InputTokens != 12 || messages[2].Status != "completed" {
				t.Fatalf("messages=%+v err=%v", messages, err)
			}
		})
	}
}

func TestRecoveryPreservesActiveTurn(t *testing.T) {
	s := testStore(t)
	thread := testThread(t, s)
	r := &Runner{Store: s}
	ctx, release, err := r.Acquire(context.Background(), thread)
	if err != nil {
		t.Fatal(err)
	}
	defer release()
	if _, err := s.BeginTurn(ctx, thread, "original", "model", false); err != nil {
		t.Fatal(err)
	}
	if err := r.RecoverInterrupted(ctx, thread); err != nil {
		t.Fatal(err)
	}
	messages, err := s.Messages(ctx, thread.ID, 0)
	if err != nil || messages[1].Status != "streaming" {
		t.Fatalf("%+v %v", messages, err)
	}
	release()
	if err := r.RecoverInterrupted(context.Background(), thread); err != nil {
		t.Fatal(err)
	}
	messages, err = s.Messages(context.Background(), thread.ID, 0)
	if err != nil || messages[1].Status != "error" {
		t.Fatalf("%+v %v", messages, err)
	}
}

func TestRunnerCompletesTruncatedResponse(t *testing.T) {
	s := testStore(t)
	thread := testThread(t, s)
	m, err := s.BeginTurn(context.Background(), thread, "question", "model", false)
	if err != nil {
		t.Fatal(err)
	}
	p := fakeProvider{stream: func(_ context.Context, _ llm.Request, emit func(string) error) (llm.Response, error) {
		return llm.Response{Text: "partial answer", Truncated: true, InputTokens: 10, OutputTokens: 20,
			ToolCalls: []llm.ToolCall{{ID: "partial", Name: "read_test", Arguments: "{"}}}, emit("partial answer")
	}}
	r := &Runner{Store: s}
	err = r.Run(context.Background(), thread, m, &config.Config{}, p, func(event string, _ any) error {
		if event == EventError || event == EventToolStarted {
			t.Errorf("unexpected event: %s", event)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	messages, err := s.Messages(context.Background(), thread.ID, 0)
	if err != nil || messages[1].Status != "completed" || messages[1].Content != "partial answer" || messages[1].InputTokens != 10 || messages[1].OutputTokens != 20 {
		t.Fatalf("messages=%+v err=%v", messages, err)
	}
}
