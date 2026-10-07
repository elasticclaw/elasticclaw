package aichat

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"testing"

	"github.com/elasticclaw/elasticclaw/pkg/hub/aichat/config"
	"github.com/elasticclaw/elasticclaw/pkg/hub/aichat/llm"
	"github.com/elasticclaw/elasticclaw/pkg/hub/aichat/tools"
)

// Exercise the complete boundary: provider responses, direct results, SSE,
// model tool replies, and all persisted tool-run text columns.
func TestEveryReadToolKeepsSecretsOutOfRuns(t *testing.T) {
	const secret = "sentinel-provider-secret"
	const sha = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	cfg := &config.Config{
		KnowledgeBase: &config.KnowledgeBase{Repo: "acme/docs", Entry: "README.md"},
		Repositories:  []string{"acme/app"},
		PostHog:       &config.PostHog{Host: "https://posthog.test", ProjectID: "42", APIKey: "PH_KEY"},
		Datadog:       &config.Datadog{Site: "datadoghq.com", Env: "prod", APIKey: "DD_KEY", AppKey: "DD_APP"},
		IssueTracker:  &config.IssueTracker{},
	}
	cfg.IssueTracker.DefaultFields.Team = "Product"
	cfg.Sources = []config.Source{{Kind: "knowledge_base", Name: "Knowledge base"}, {Kind: "repositories", Name: "Repositories"}, {Kind: "posthog", Name: "PostHog"}, {Kind: "datadog", Name: "Datadog"}, {Kind: "issue_tracker", Name: "Linear"}}
	args := map[string]string{
		"kb_list": `{}`, "kb_read": `{"path":"README.md"}`, "kb_search": `{"query":"Intro"}`,
		"repo_tree": `{"repo":"acme/app"}`, "repo_read": `{"repo":"acme/app","path":"README.md"}`, "repo_search": `{"repo":"acme/app","query":"handler"}`,
		"posthog_query": `{"query":"SELECT event FROM events"}`, "posthog_insights": `{}`,
		"datadog_metrics": `{"from":"2026-10-01T00:00:00Z","to":"2026-10-02T00:00:00Z","aggregation":"avg","metric":"system.cpu"}`,
		"datadog_logs":    `{"from":"2026-10-01T00:00:00Z","to":"2026-10-02T00:00:00Z"}`,
		"linear_search":   `{"query":"duplicate"}`, "linear_get": `{"id":"ABC-123"}`,
	}
	for _, authFailure := range []bool{false, true} {
		t.Run(fmt.Sprint("authFailure=", authFailure), func(t *testing.T) {
			deps := Deps{GitHubAPI: "https://github.test", GitHubToken: func(string) string { return secret }, Secret: func(string, string) (string, bool) { return secret, true }, LinearToken: func(string) (string, bool) { return secret, true }}
			deps.HTTPClient = sourceClient(func(w http.ResponseWriter, r *http.Request) {
				if authFailure {
					w.WriteHeader(401)
					fmt.Fprint(w, secret)
					return
				}
				var value any
				switch {
				case strings.Contains(r.URL.Path, "/commits/"):
					value = map[string]any{"sha": sha}
				case strings.Contains(r.URL.Path, "/git/trees/"):
					value = map[string]any{"tree": []any{map[string]any{"path": "README.md", "type": "blob", "mode": "100644", "size": 50}}}
				case strings.Contains(r.URL.Path, "/contents/"):
					value = map[string]any{"type": "file", "encoding": "base64", "content": base64.StdEncoding.EncodeToString([]byte("# Intro " + secret)), "size": 50}
				case r.URL.Path == "/search/code":
					value = map[string]any{"items": []any{map[string]any{"path": "README.md", "repository": map[string]any{"full_name": "acme/app"}, "text_matches": []any{map[string]any{"fragment": secret}}}}}
				case strings.HasPrefix(r.URL.Path, "/repos/"):
					value = map[string]any{"default_branch": "main"}
				case strings.HasSuffix(r.URL.Path, "/query/"):
					value = map[string]any{"columns": []string{"event"}, "results": [][]string{{secret}}}
				case strings.Contains(r.URL.Path, "/insights/"):
					value = map[string]any{"results": []any{map[string]any{"name": secret}}}
				case r.URL.Path == "/api/v1/query":
					value = map[string]any{"status": "ok", "series": []any{map[string]any{"metric": secret}}}
				case strings.HasSuffix(r.URL.Path, "/logs/events/search"):
					value = map[string]any{"data": []any{map[string]any{"message": secret}}}
				case r.URL.Path == "/graphql":
					var body struct{ Query string }
					_ = json.NewDecoder(r.Body).Decode(&body)
					issue := map[string]any{"identifier": "ABC-123", "title": secret}
					if strings.Contains(body.Query, "SearchIssues") {
						value = map[string]any{"data": map[string]any{"searchIssues": map[string]any{"nodes": []any{issue}}}}
					} else {
						value = map[string]any{"data": map[string]any{"issue": issue}}
					}
				default:
					t.Errorf("unexpected provider endpoint: %s", r.URL)
					w.WriteHeader(404)
					return
				}
				_ = json.NewEncoder(w).Encode(value)
			})
			var reads []tools.Tool
			for _, source := range cfg.Sources {
				reads = append(reads, deps.candidate("ws", cfg, source).tools...)
			}
			if len(reads) != 12 {
				t.Fatalf("tool coverage = %d", len(reads))
			}
			registry, err := tools.NewReadRegistry(reads...)
			if err != nil {
				t.Fatal(err)
			}
			store := testStore(t)
			thread := testThread(t, store)
			message, err := store.BeginTurn(context.Background(), thread, "question", "model", false)
			if err != nil {
				t.Fatal(err)
			}
			runner := Runner{Store: store}
			for i, tool := range reads {
				name := tool.Definition().Name
				t.Run(name, func(t *testing.T) {
					input := args[name]
					if input == "" {
						t.Fatal("missing tool fixture")
					}
					result, err := tool.Run(context.Background(), json.RawMessage(input))
					if strings.Contains(fmt.Sprint(result, err), secret) {
						t.Fatal("credential in direct result/error")
					}
					if !authFailure && (err != nil || result.RowCount != 1) {
						t.Fatalf("happy path: %+v %v", result, err)
					}
					if authFailure && err == nil {
						t.Fatal("expected auth error")
					}
					// A model can echo sensitive text in arguments; it must never reach args_json.
					recordedInput := strings.TrimSuffix(input, "}") + `,"credential_echo":"` + secret + `"}`
					if input == "{}" {
						recordedInput = `{"credential_echo":"` + secret + `"}`
					}
					reply, err := runner.runToolReads(context.Background(), message.ID, i+1, llm.ToolCall{ID: name, Name: name, Arguments: recordedInput}, func(_ string, data any) error {
						raw, _ := json.Marshal(data)
						if strings.Contains(string(raw), secret) {
							t.Fatal("credential in SSE")
						}
						return nil
					}, registry, []string{secret})
					if err != nil || strings.Contains(reply.Content, secret) {
						t.Fatalf("tool reply: %+v %v", reply, err)
					}
					var persisted string
					err = store.DB.QueryRow(`SELECT args_json || summary || result_json || COALESCE(error,'') FROM ai_chat_tool_runs WHERE message_id=? AND seq=?`, message.ID, i+1).Scan(&persisted)
					if err != nil || strings.Contains(persisted, secret) {
						t.Fatalf("persisted credential: %v", err)
					}
					// Record the successful provider response as well, not only rejected args.
					_, err = runner.runToolReads(context.Background(), message.ID, i+101, llm.ToolCall{ID: name, Name: name, Arguments: input}, func(string, any) error { return nil }, registry, []string{secret})
					if err != nil {
						t.Fatal(err)
					}
					err = store.DB.QueryRow(`SELECT args_json || summary || result_json || COALESCE(error,'') FROM ai_chat_tool_runs WHERE message_id=? AND seq=?`, message.ID, i+101).Scan(&persisted)
					if err != nil || strings.Contains(persisted, secret) {
						t.Fatalf("persisted provider credential: %v", err)
					}
				})
			}
		})
	}
}

func TestRunnerRedactsSplitTokensAndRejectsOriginalMalformedArgs(t *testing.T) {
	const secret = "sentinel-secret"
	store := testStore(t)
	thread := testThread(t, store)
	message, err := store.BeginTurn(context.Background(), thread, "question", "model", false)
	if err != nil {
		t.Fatal(err)
	}
	calls := 0
	tool := tools.ReadTool{Name: tools.Define("safe_read", "Read", map[string]any{}), Source: "Test", Execute: func(_ context.Context, raw json.RawMessage) (tools.Result, error) {
		var args struct{}
		if err := tools.DecodeArgs(raw, &args); err != nil {
			return tools.Result{}, err
		}
		calls++
		return tools.Result{}, nil
	}}
	registry, _ := tools.NewReadRegistry(tool)
	runner := Runner{Store: store, PrepareReads: func(_ context.Context, _ string, cfg *config.Config) (*tools.ReadRegistry, *config.Config, []string) {
		return registry, cfg, []string{secret}
	}}
	iteration := 0
	provider := fakeProvider{stream: func(_ context.Context, req llm.Request, emit func(string) error) (llm.Response, error) {
		iteration++
		if iteration == 1 {
			_ = emit("sentinel-")
			_ = emit("secret")
			return llm.Response{ToolCalls: []llm.ToolCall{{ID: "bad", Name: "safe_read", Arguments: `{} {}`}}}, nil
		}
		if !req.Messages[len(req.Messages)-1].IsError {
			t.Fatal("malformed args accepted")
		}
		return llm.Response{}, nil
	}}
	var tokens strings.Builder
	if err := runner.Run(context.Background(), thread, message, &config.Config{}, provider, func(event string, value any) error {
		if event == EventToken {
			tokens.WriteString(value.(map[string]string)["text"])
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if calls != 0 || tokens.String() != "[redacted]" {
		t.Fatalf("calls=%d tokens=%q", calls, tokens.String())
	}
	messages, err := store.Messages(context.Background(), thread.ID, 0)
	if err != nil || strings.Contains(fmt.Sprint(messages), secret) {
		t.Fatalf("persisted secret: %v", err)
	}
}

func TestConnectorValidationErrorsAreActionable(t *testing.T) {
	cfg := &config.Config{
		KnowledgeBase: &config.KnowledgeBase{Repo: "acme/docs", Entry: "README.md"},
		Repositories:  []string{"acme/app"},
		PostHog:       &config.PostHog{Host: "https://posthog.test", ProjectID: "42", APIKey: "PH"},
		Datadog:       &config.Datadog{Site: "datadoghq.com", Env: "prod", APIKey: "DD", AppKey: "APP"},
		IssueTracker:  &config.IssueTracker{},
	}
	cfg.IssueTracker.DefaultFields.Team = "Product"
	deps := Deps{GitHubToken: func(string) string { return "secret" }, Secret: func(string, string) (string, bool) { return "secret", true }, LinearToken: func(string) (string, bool) { return "secret", true }, HTTPClient: sourceClient(func(w http.ResponseWriter, r *http.Request) {
		t.Errorf("invalid arguments reached provider: %s", r.URL)
		w.WriteHeader(500)
	})}
	args := map[string]string{
		"kb_list": `{"dir":"../"}`, "kb_read": `{"path":"../README.md"}`, "kb_search": `{"query":""}`,
		"repo_tree": `{"repo":"other/private"}`, "repo_read": `{"repo":"acme/app","path":"../file"}`, "repo_search": `{"repo":"acme/app","query":"repo:other/private"}`,
		"posthog_query": `{"query":"DELETE FROM events"}`, "posthog_insights": `{"id":"../"}`,
		"datadog_metrics": `{"from":"invalid"}`, "datadog_logs": `{"from":"invalid"}`,
		"linear_search": `{"query":""}`, "linear_get": `{"id":"../"}`,
	}
	for _, kind := range []string{"knowledge_base", "repositories", "posthog", "datadog", "issue_tracker"} {
		for _, tool := range deps.candidate("ws", cfg, config.Source{Kind: kind}).tools {
			name := tool.Definition().Name
			for _, raw := range []string{args[name], `{"unknown":true}`} {
				_, err := tool.Run(context.Background(), json.RawMessage(raw))
				var argErr tools.ArgError
				if !errors.As(err, &argErr) || argErr.Error() == "" {
					t.Errorf("%s validation error: %v", name, err)
				}
			}
		}
	}
}

func TestRunnerExposesOnlyRedactedArgumentErrors(t *testing.T) {
	store := testStore(t)
	thread := testThread(t, store)
	message, err := store.BeginTurn(context.Background(), thread, "question", "model", false)
	if err != nil {
		t.Fatal(err)
	}
	runner := Runner{Store: store}
	for i, test := range []struct {
		err  error
		want string
	}{
		{fmt.Errorf("wrapped: %w", tools.ArgError("Invalid reference sentinel-secret")), "Invalid reference [redacted]"},
		{errors.New("provider sentinel-secret private details"), "Read tool failed"},
	} {
		tool := tools.ReadTool{Name: tools.Define("safe_read", "Read", map[string]any{}), Execute: func(context.Context, json.RawMessage) (tools.Result, error) { return tools.Result{}, test.err }}
		registry, _ := tools.NewReadRegistry(tool)
		reply, err := runner.runToolReads(context.Background(), message.ID, i+1, llm.ToolCall{ID: "call", Name: "safe_read", Arguments: `{}`}, func(event string, payload any) error {
			if event == EventToolFinished && payload.(map[string]any)["error"] != test.want {
				t.Errorf("SSE error: %v", payload)
			}
			return nil
		}, registry, []string{"sentinel-secret"})
		if err != nil || !reply.IsError || reply.Content != test.want {
			t.Fatalf("tool reply: %+v %v", reply, err)
		}
		var stored string
		if err := store.DB.QueryRow(`SELECT error FROM ai_chat_tool_runs WHERE message_id=? AND seq=?`, message.ID, i+1).Scan(&stored); err != nil || stored != test.want {
			t.Fatalf("stored error: %q %v", stored, err)
		}
	}
}
