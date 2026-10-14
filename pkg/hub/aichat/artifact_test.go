package aichat

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/elasticclaw/elasticclaw/pkg/hub/aichat/config"
	"github.com/elasticclaw/elasticclaw/pkg/hub/aichat/llm"
	"github.com/elasticclaw/elasticclaw/pkg/hub/aichat/tools"
)

const testHTML = "<!doctype html><html><head><title>Meal swap</title></head><body>Prototype</body></html>"

func TestArtifactValidation(t *testing.T) {
	for _, tc := range []struct {
		name, body string
		valid      bool
	}{
		{"inline", `<style>body{color:red}</style><script>let count=1</script>`, true},
		{"libraries", `<script src="https://cdnjs.cloudflare.com/a.js"></script><script src="https://cdn.jsdelivr.net/a.js"></script>`, true},
		{"fonts", `<link href="https://fonts.googleapis.com/a"><style>@font-face{src:url(https://fonts.gstatic.com/a)}</style>`, true},
		{"upper scheme", `<script src="HTTPS://CDN.JSDELIVR.NET/a.js"></script>`, true},
		{"data", `<img src="data:image/png;base64,abc"><a href="#details">Details</a>`, true},
		{"http", `<img src="http://cdn.jsdelivr.net/a">`, false},
		{"host", `<img src="https://evil.test/a">`, false},
		{"protocol relative", `<img src="//evil.test/a">`, false},
		{"relative", `<img src="/api/secret">`, false},
		{"javascript", `<a href="JaVaScRiPt:alert(1)">Go</a>`, false},
		{"encoded scheme", `<a href="java&#10;script:alert(1)">Go</a>`, false},
		{"srcset", `<img srcset="data:image/png;base64,abc 1x, https://evil.test/a 2x">`, false},
		{"srcset no descriptor", `<img srcset="https://cdn.jsdelivr.net/a,https://evil.test/b">`, false},
		{"srcset allowed", `<img srcset="data:image/png;base64,abc 1x, data:image/png;base64,def 2x">`, true},
		{"css url", `<style>p{background:url('https://evil.test/a')}</style>`, false},
		{"css import", `<style>@import "https://evil.test/a";</style>`, false},
		{"css import url", `<style>@import url(//evil.test/a);</style>`, false},
		{"css escaped", `<style>p{background:u\72l(https://evil.test/a)}</style>`, false},
		{"css comments", `<style>@im/**/port "https://evil.test/a";</style>`, false},
		{"style attr", `<div style="background: url(https://evil.test/a)"></div>`, false},
		{"refresh", `<meta http-equiv="REFRESH" content="0; URL='https://evil.test/a'">`, false},
		{"refresh whitespace", `<meta http-equiv="refresh" content=" 0; url=https://evil.test/">`, false},
		{"refresh without url", `<meta http-equiv="refresh" content="0; https://evil.test/">`, false},
		{"base", `<base href="https://evil.test/a">`, false},
		{"form", `<form action="https://evil.test/a"><button formaction="https://evil.test/b">Go</button></form>`, false},
		{"poster data", `<video poster="https://evil.test/a"></video><object data="https://evil.test/b"></object>`, false},
		{"userinfo", `<script src="https://evil.test@cdn.jsdelivr.net/a"></script>`, false},
		{"port", `<script src="https://cdn.jsdelivr.net:8443/a"></script>`, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := validateArtifact("<!doctype html><html>" + tc.body + "</html>")
			if (err == nil) != tc.valid {
				t.Fatal(err)
			}
		})
	}
	for _, body := range []string{"", "<div>fragment</div>", "<!doctype html><html>cut", strings.Repeat("x", maxArtifactBytes+1)} {
		if validateArtifact(body) == nil {
			t.Fatal("accepted invalid document")
		}
	}
	if got := stripHTMLFence("```html\n" + testHTML + "\n```"); got != testHTML {
		t.Fatal(got)
	}
}

func TestArtifactStorageAndConcurrentVersions(t *testing.T) {
	ctx := context.Background()
	s := testStore(t)
	thread := testThread(t, s)
	dir := t.TempDir()
	m, _ := s.beginAction(ctx, thread, "fake", "")
	a, err := s.publishArtifact(ctx, dir, thread, &m, tools.ArtifactRequest{Title: "Meal swap / prototype"}, testHTML)
	if err != nil {
		t.Fatal(err)
	}
	if !artifactSlug.MatchString(a.Slug) || !strings.HasPrefix(a.Slug, "meal-swap-prototype-") {
		t.Fatal(a.Slug)
	}
	info, err := os.Stat(a.Path)
	if err != nil || info.Mode().Perm() != 0600 {
		t.Fatal(info, err)
	}
	parent, _ := os.Stat(filepath.Dir(a.Path))
	if parent.Mode().Perm() != 0700 {
		t.Fatal(parent.Mode())
	}
	sum := sha256.Sum256([]byte(testHTML))
	saved, err := s.Artifact(ctx, thread.TenantID, a.Slug, 0)
	if err != nil || saved.Bytes != int64(len(testHTML)) || saved.SHA256 != hex.EncodeToString(sum[:]) {
		t.Fatal(saved, err)
	}
	var wg sync.WaitGroup
	for i := 0; i < 5; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			copy := m
			_, e := s.publishArtifact(ctx, dir, thread, &copy, tools.ArtifactRequest{ArtifactID: a.ID, Title: a.Title}, testHTML)
			if e != nil {
				t.Error(e)
			}
		}()
	}
	wg.Wait()
	saved, err = s.Artifact(ctx, thread.TenantID, a.Slug, 0)
	if err != nil || saved.Version != 6 {
		t.Fatal(saved, err)
	}
	entries, _ := os.ReadDir(filepath.Dir(a.Path))
	if len(entries) != 6 {
		t.Fatal(entries)
	}
	for _, tenant := range []string{"../outside", "..", "a/b", "a\\b", ""} {
		if _, e := artifactPath(dir, tenant, a.Slug, 1); e == nil {
			t.Fatal(tenant)
		}
	}
	wrong := thread
	wrong.OwnerLogin = "other"
	if _, e := s.ownedArtifact(ctx, wrong, a.ID); e == nil {
		t.Fatal("other owner")
	}
	wrong = thread
	wrong.ID = "other"
	if _, e := s.ownedArtifact(ctx, wrong, a.ID); e == nil {
		t.Fatal("other thread")
	}
	cancelled, cancel := context.WithCancel(ctx)
	cancel()
	copy := m
	if _, e := s.publishArtifact(cancelled, dir, thread, &copy, tools.ArtifactRequest{ArtifactID: a.ID, Title: a.Title}, testHTML); e == nil {
		t.Fatal("published after cancellation")
	}
	entries, _ = os.ReadDir(filepath.Dir(a.Path))
	if len(entries) != 6 {
		t.Fatal(entries)
	}
}

func TestArtifactSignedServing(t *testing.T) {
	ctx := context.Background()
	s := testStore(t)
	thread := testThread(t, s)
	dir := t.TempDir()
	m, _ := s.beginAction(ctx, thread, "fake", "")
	v, err := s.publishArtifact(ctx, dir, thread, &m, tools.ArtifactRequest{Title: "Meal swap"}, testHTML)
	if err != nil {
		t.Fatal(err)
	}
	enabled := true
	deps := Deps{DB: s.DB, ArtifactsDir: dir, ArtifactKey: func() string { return "session-secret" }, FeatureStage: func(context.Context, string) string {
		if enabled {
			return "on"
		}
		return "off"
	}, TenantID: func(r *http.Request) string { return r.Header.Get("Tenant") }, CallerLogin: func(r *http.Request) string { return r.Header.Get("Login") }, WebAuth: func(next http.HandlerFunc) http.HandlerFunc {
		return func(w http.ResponseWriter, r *http.Request) {
			if r.Header.Get("Login") == "" {
				http.Error(w, "unauthorized", 401)
				return
			}
			next(w, r)
		}
	}, WithFeature: func(_ string, next http.HandlerFunc) http.HandlerFunc {
		return func(w http.ResponseWriter, r *http.Request) {
			if !enabled {
				http.NotFound(w, r)
				return
			}
			next(w, r)
		}
	}, PublicURL: func() string { return "https://factory.test" }}
	api := &api{deps: deps, store: s, runner: &Runner{PublicURL: deps.PublicURL}}
	handler := RawArtifactRoutes(deps)
	for _, download := range []string{"0", "1"} {
		signed := api.signedArtifactURL(v, download)
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, httptest.NewRequest("GET", signed, nil))
		disposition := "inline"
		if download == "1" {
			disposition = "attachment"
		}
		want := map[string]string{"Content-Security-Policy": "default-src 'none'; script-src 'unsafe-inline' https://cdnjs.cloudflare.com https://cdn.jsdelivr.net; style-src 'unsafe-inline' https://fonts.googleapis.com; font-src data: https://fonts.gstatic.com; img-src data:; connect-src 'none'; frame-ancestors 'self'; sandbox allow-scripts", "X-Content-Type-Options": "nosniff", "Referrer-Policy": "no-referrer", "Content-Disposition": fmt.Sprintf(`%s; filename="%s-v1.html"`, disposition, v.Slug), "Cache-Control": "no-store"}
		if rec.Code != 200 || rec.Body.String() != testHTML {
			t.Fatal(rec.Code, rec.Body.String())
		}
		for key, value := range want {
			if rec.Header().Get(key) != value {
				t.Fatal(key, rec.Header())
			}
		}
		for _, mutation := range []struct{ key, value string }{{"v", "2"}, {"tenant", "other"}, {"sig", ""}, {"exp", "1"}, {"download", map[string]string{"0": "1", "1": "0"}[download]}} {
			u, _ := url.Parse(signed)
			q := u.Query()
			q.Set(mutation.key, mutation.value)
			u.RawQuery = q.Encode()
			rr := httptest.NewRecorder()
			handler.ServeHTTP(rr, httptest.NewRequest("GET", u.String(), nil))
			if rr.Code != 404 {
				t.Fatal(mutation, rr.Code)
			}
		}
		u, _ := url.Parse(signed)
		u.Path = strings.Replace(u.Path, v.Slug, "other-abcdef", 1)
		rr := httptest.NewRecorder()
		handler.ServeHTTP(rr, httptest.NewRequest("GET", u.String(), nil))
		if rr.Code != 404 {
			t.Fatal(rr.Code)
		}
	}
	for _, tenant := range []string{"tenant", "other"} {
		req := httptest.NewRequest("GET", "/api/ai-chat/artifacts/"+v.Slug, nil)
		req.Header.Set("Tenant", tenant)
		req.Header.Set("Login", "teammate")
		rec := httptest.NewRecorder()
		Routes(deps).ServeHTTP(rec, req)
		want := 200
		if tenant == "other" {
			want = 404
		}
		if rec.Code != want {
			t.Fatal(tenant, rec.Code, rec.Body.String())
		}
		if want == 200 && !strings.Contains(rec.Body.String(), "https://factory.test/artifacts/") {
			t.Fatal(rec.Body.String())
		}
	}
	enabled = false
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, httptest.NewRequest("GET", api.signedArtifactURL(v, "0"), nil))
	if rec.Code != 404 {
		t.Fatal(rec.Code)
	}
	enabled = true
	s.DB.Exec(`UPDATE ai_chat_artifacts SET deleted_at=1 WHERE id=?`, v.ID)
	rec = httptest.NewRecorder()
	handler.ServeHTTP(rec, httptest.NewRequest("GET", api.signedArtifactURL(v, "0"), nil))
	if rec.Code != 404 {
		t.Fatal(rec.Code)
	}
	req := httptest.NewRequest("GET", "/api/ai-chat/artifacts/"+v.Slug, nil)
	req.Header.Set("Tenant", "tenant")
	req.Header.Set("Login", "teammate")
	rec = httptest.NewRecorder()
	Routes(deps).ServeHTTP(rec, req)
	if rec.Code != 404 {
		t.Fatal(rec.Code)
	}
	for _, path := range []string{"/artifacts/" + v.Slug, "/artifacts/" + v.Slug + "/v/2/"} {
		if p, ok := ArtifactStaticPath(path); !ok || p != "artifacts/index.html" {
			t.Fatal(path)
		}
	}
	for _, path := range []string{"/artifacts/../secret", "/artifacts/" + v.Slug + "/raw", "/artifacts/" + v.Slug + "/v/0"} {
		if _, ok := ArtifactStaticPath(path); ok {
			t.Fatal(path)
		}
	}
}

func TestArtifactGeneration(t *testing.T) {
	for _, tc := range []struct {
		name                 string
		outputs              []string
		truncated            bool
		wantState, wantError string
	}{
		{"success", []string{testHTML}, false, "created", ""},
		{"fenced", []string{"```html\n" + testHTML + "\n```"}, false, "created", ""},
		{"retry", []string{`<!doctype html><html><img src="https://evil.test/a"></html>`, testHTML}, false, "created", ""},
		{"retry fails", []string{`<!doctype html><html><img src="https://evil.test/a"></html>`, `<!doctype html><html><img src="https://evil.test/b"></html>`}, false, "failed", "outside the allowlist: evil.test"},
		{"too big", []string{"<!doctype html><html>" + strings.Repeat("x", maxArtifactBytes) + "</html>"}, false, "failed", "over the 2 MB limit"},
		{"truncated", []string{testHTML}, true, "failed", "The page was cut off before it finished."},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			s := testStore(t)
			thread := testThread(t, s)
			m, _ := s.beginAction(ctx, thread, "fake", "")
			r := &Runner{Store: s, ArtifactsDir: t.TempDir()}
			calls := 0
			p := fakeProvider{stream: func(_ context.Context, req llm.Request, token func(string) error) (llm.Response, error) {
				if req.MaxTokens != 32000 || len(req.Tools) != 0 {
					t.Fatal(req)
				}
				if calls >= len(tc.outputs) {
					t.Fatal("too many retries")
				}
				out := tc.outputs[calls]
				calls++
				if calls == 2 && !strings.Contains(req.Messages[len(req.Messages)-1].Content, "evil.test") {
					t.Fatal(req.Messages)
				}
				err := token(out)
				return llm.Response{Text: out, Truncated: tc.truncated, InputTokens: 10, OutputTokens: 20}, err
			}}
			var events strings.Builder
			emit := func(event string, payload any) error {
				raw, _ := json.Marshal(payload)
				events.WriteString(event + string(raw))
				return nil
			}
			if err := r.RunArtifact(ctx, thread, m, &config.Config{}, p, tools.ArtifactRequest{Title: "Meal swap", Brief: "A prototype"}, emit); err != nil {
				t.Fatal(err)
			}
			messages, _ := s.Messages(ctx, thread.ID, 0)
			last := messages[len(messages)-1]
			var blocks []tools.Block
			json.Unmarshal(last.Blocks, &blocks)
			if len(blocks) != 1 || blocks[0].State != tc.wantState {
				t.Fatal(string(last.Blocks))
			}
			var card ArtifactCard
			json.Unmarshal(blocks[0].Card, &card)
			if !strings.Contains(card.Error, tc.wantError) || calls != len(tc.outputs) {
				t.Fatal(card, calls)
			}
			if strings.Contains(events.String(), "<!doctype") || strings.Contains(events.String(), "Prototype") {
				t.Fatal("HTML leaked to SSE")
			}
			var count int
			s.DB.QueryRow(`SELECT count(*) FROM ai_chat_artifacts`).Scan(&count)
			if (count == 1) != (tc.wantState == "created") {
				t.Fatal(count)
			}
			if tc.wantState == "created" && !strings.Contains(historyContent(last), "artifactId: "+card.ArtifactID) {
				t.Fatal(historyContent(last))
			}
			if tc.wantState == "failed" && !strings.Contains(historyContent(last), "[Artifact failed:") {
				t.Fatal(historyContent(last))
			}
		})
	}
}

func TestArtifactCancelRecoveryAndSecrets(t *testing.T) {
	s := testStore(t)
	thread := testThread(t, s)
	ctx := context.Background()
	m, _ := s.beginAction(ctx, thread, "fake", "")
	r := &Runner{Store: s, ArtifactsDir: t.TempDir(), PrepareReads: func(_ context.Context, _ string, c *config.Config) (*tools.ReadRegistry, *config.Config, []string) {
		return nil, c, []string{"SECRET-SENTINEL"}
	}}
	p := fakeProvider{stream: func(_ context.Context, req llm.Request, token func(string) error) (llm.Response, error) {
		raw, _ := json.Marshal(req)
		if strings.Contains(string(raw), "SECRET-SENTINEL") {
			t.Fatal("prompt secret")
		}
		out := strings.Replace(testHTML, "Prototype", "SECRET-SENTINEL", 1)
		token(out[:30])
		token(out[30:])
		return llm.Response{}, nil
	}}
	var events strings.Builder
	if err := r.RunArtifact(ctx, thread, m, &config.Config{}, p, tools.ArtifactRequest{Title: "SECRET-SENTINEL", Brief: "SECRET-SENTINEL"}, func(_ string, v any) error { raw, _ := json.Marshal(v); events.Write(raw); return nil }); err != nil {
		t.Fatal(err)
	}
	var slug, run string
	s.DB.QueryRow(`SELECT slug FROM ai_chat_artifacts`).Scan(&slug)
	v, _ := s.Artifact(ctx, thread.TenantID, slug, 0)
	body, _ := readArtifact(r.ArtifactsDir, v)
	s.DB.QueryRow(`SELECT args_json||summary||COALESCE(result_json,'')||COALESCE(error,'') FROM ai_chat_tool_runs WHERE tool='artifact'`).Scan(&run)
	if strings.Contains(events.String()+string(body)+run, "SECRET-SENTINEL") {
		t.Fatal("secret leaked")
	}
	m, _ = s.beginAction(ctx, thread, "fake", "")
	turn, cancel := context.WithCancel(ctx)
	p.stream = func(ctx context.Context, _ llm.Request, token func(string) error) (llm.Response, error) {
		token("<!doctype html>")
		cancel()
		return llm.Response{}, ctx.Err()
	}
	if err := r.RunArtifact(turn, thread, m, &config.Config{}, p, tools.ArtifactRequest{Title: "Stopped", Brief: "Stop"}, func(string, any) error { return nil }); err != nil {
		t.Fatal(err)
	}
	messages, _ := s.Messages(ctx, thread.ID, 0)
	if !strings.Contains(string(messages[len(messages)-1].Blocks), "Stopped. Nothing was published.") {
		t.Fatal(messages)
	}
	var count int
	s.DB.QueryRow(`SELECT count(*) FROM ai_chat_artifacts`).Scan(&count)
	if count != 1 {
		t.Fatal(count)
	}
	m, _ = s.beginAction(ctx, thread, "fake", "")
	s.DB.Exec(`UPDATE ai_chat_tool_runs SET args_json=? WHERE message_id=?`, `{"title":"Interrupted","brief":"Try again"}`, m.ID)
	if err := s.RecoverInterrupted(ctx, thread.ID); err != nil {
		t.Fatal(err)
	}
	messages, _ = s.Messages(ctx, thread.ID, 0)
	if !strings.Contains(string(messages[len(messages)-1].Blocks), "The generation was interrupted.") {
		t.Fatal(messages)
	}
}

func TestStartArtifactEndsBatchAndAvailability(t *testing.T) {
	for _, available := range []bool{false, true} {
		t.Run(fmt.Sprint(available), func(t *testing.T) {
			ctx := context.Background()
			s := testStore(t)
			thread := testThread(t, s)
			m, _ := s.BeginTurn(ctx, thread, "Create a prototype", "fake", false)
			r := &Runner{Store: s, ArtifactsDir: t.TempDir()}
			if available {
				r.AvailableActions = func(*config.Config) []string { return []string{"artifact"} }
			}
			calls := 0
			p := fakeProvider{stream: func(_ context.Context, req llm.Request, token func(string) error) (llm.Response, error) {
				calls++
				if calls == 1 {
					found := false
					for _, tool := range req.Tools {
						if tool.Name == tools.StartArtifact {
							found = true
						}
					}
					if found != available {
						t.Fatal(found)
					}
					return llm.Response{ToolCalls: []llm.ToolCall{{ID: "artifact", Name: tools.StartArtifact, Arguments: `{"title":"Meal swap","brief":"Build it"}`}, {ID: "later", Name: tools.EmitBlock, Arguments: testBlockArgs}}}, nil
				}
				if available {
					if len(req.Tools) > 0 {
						t.Fatal("did not enter generation")
					}
					token(testHTML)
				}
				return llm.Response{}, nil
			}}
			if err := r.Run(ctx, thread, m, &config.Config{}, p, func(string, any) error { return nil }); err != nil {
				t.Fatal(err)
			}
			var count int
			s.DB.QueryRow(`SELECT count(*) FROM ai_chat_artifacts`).Scan(&count)
			if (count == 1) != available {
				t.Fatal(count)
			}
			if available {
				s.DB.QueryRow(`SELECT count(*) FROM ai_chat_tool_runs WHERE tool='emit_block'`).Scan(&count)
				if count != 0 {
					t.Fatal("executed later call")
				}
			}
		})
	}
}

func TestArtifactActionsAPI(t *testing.T) {
	p := fakeProvider{stream: func(_ context.Context, _ llm.Request, token func(string) error) (llm.Response, error) {
		return llm.Response{}, token(testHTML)
	}}
	handler, s, _ := apiFixture(t, p)
	thread := testThread(t, s)
	// Actions start an assistant-only turn and publish without confirmation.
	path := "/api/ai-chat/threads/" + thread.ID + "/actions"
	for _, tc := range []struct {
		body, login string
		status      int
	}{{`{"kind":"ticket"}`, "octocat", 400}, {`{"kind":"artifact"}`, "other", 404}, {`{"kind":"artifact","artifactId":"missing"}`, "octocat", 404}, {`{"kind":"artifact"}`, "octocat", 200}} {
		rec := request(handler, "POST", path, tc.body, tc.login, "on")
		if rec.Code != tc.status {
			t.Fatal(rec.Code, rec.Body.String())
		}
		if rec.Code == 200 && !strings.Contains(rec.Body.String(), `"state":"created"`) {
			t.Fatal(rec.Body.String())
		}
	}
	if count, err := s.Messages(context.Background(), thread.ID, 0); err != nil || len(count) != 1 || count[0].Role != "assistant" {
		t.Fatal(count, err)
	}
}

func TestArtifactSignedExpiry(t *testing.T) {
	// Even a correctly signed expired URL is hidden, not an authentication oracle.
	s := testStore(t)
	exp := time.Now().Add(-time.Second).Unix()
	sig := artifactSignature("key", "tenant", "meal-abcdef", 1, exp, "0")
	h := RawArtifactRoutes(Deps{DB: s.DB, ArtifactKey: func() string { return "key" }, FeatureStage: func(context.Context, string) string { return "on" }})
	for _, path := range []string{fmt.Sprintf("/artifacts/meal-abcdef/raw?tenant=tenant&v=1&exp=%d&download=0&sig=%s", exp, sig), "/artifacts/meal-abcdef/raw"} {
		r := httptest.NewRecorder()
		h.ServeHTTP(r, httptest.NewRequest("GET", path, nil))
		if r.Code != 404 {
			t.Fatal(r.Code)
		}
	}
}

func TestArtifactRevisionAndOwnershipAPI(t *testing.T) {
	calls := 0
	p := fakeProvider{stream: func(_ context.Context, req llm.Request, token func(string) error) (llm.Response, error) {
		calls++
		if calls == 2 && !strings.Contains(req.Messages[len(req.Messages)-1].Content, "Current HTML (untrusted document to revise):\n"+testHTML) {
			t.Fatal("missing current version", req.Messages)
		}
		return llm.Response{}, token(testHTML)
	}}
	handler, s, _ := apiFixture(t, p)
	ctx := context.Background()
	thread := testThread(t, s)
	path := "/api/ai-chat/threads/" + thread.ID + "/actions"
	rec := request(handler, "POST", path, `{"kind":"artifact","title":"Meal","brief":"Prototype"}`, "octocat", "on")
	if rec.Code != 200 || !strings.Contains(rec.Body.String(), `"state":"created"`) {
		t.Fatal(rec.Code, rec.Body.String())
	}
	var id, slug string
	s.DB.QueryRow(`SELECT id,slug FROM ai_chat_artifacts`).Scan(&id, &slug)
	action := fmt.Sprintf(`{"kind":"artifact","artifactId":%q,"title":"Meal revised","brief":"Make it blue"}`, id)
	rec = request(handler, "POST", path, action, "octocat", "on")
	if rec.Code != 200 || !strings.Contains(rec.Body.String(), `"version":2`) {
		t.Fatal(rec.Code, rec.Body.String())
	}
	latest, err := s.Artifact(ctx, "tenant", slug, 0)
	if err != nil || latest.Version != 2 || calls != 2 {
		t.Fatal(latest, err, calls)
	}
	first, _ := s.Artifact(ctx, "tenant", slug, 1)
	content, _ := os.ReadFile(first.Path)
	if string(content) != testHTML {
		t.Fatal("v1 mutated")
	}
	other, _ := s.CreateThread(ctx, "tenant", "octocat", "workspace", "explore_idea")
	rec = request(handler, "POST", "/api/ai-chat/threads/"+other.ID+"/actions", action, "octocat", "on")
	if rec.Code != 404 {
		t.Fatal(rec.Code)
	}
	rec = request(handler, "POST", path, action, "other", "on")
	if rec.Code != 404 {
		t.Fatal(rec.Code)
	}
	// Refuse a revision before making an expensive model call.
	s.DB.Exec(`UPDATE ai_chat_artifact_versions SET bytes=? WHERE artifact_id=? AND version=2`, 512*1024+1, id)
	rec = request(handler, "POST", path, action, "octocat", "on")
	if !strings.Contains(rec.Body.String(), "over 512 KB") || calls != 2 {
		t.Fatal(rec.Body.String(), calls)
	}
}

func TestArtifactCancelDuringPreparation(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	thread := testThread(t, s)
	m, _ := s.beginAction(ctx, thread, "fake", "")
	turn, cancel := context.WithCancel(ctx)
	r := &Runner{Store: s, ArtifactsDir: t.TempDir(), PrepareReads: func(_ context.Context, _ string, cfg *config.Config) (*tools.ReadRegistry, *config.Config, []string) {
		cancel()
		return nil, cfg, nil
	}}
	p := fakeProvider{stream: func(context.Context, llm.Request, func(string) error) (llm.Response, error) {
		t.Fatal("called model after cancel")
		return llm.Response{}, nil
	}}
	if err := r.RunArtifact(turn, thread, m, &config.Config{}, p, tools.ArtifactRequest{Title: "Meal"}, func(string, any) error { return nil }); err != nil {
		t.Fatal(err)
	}
	messages, _ := s.Messages(ctx, thread.ID, 0)
	if !strings.Contains(string(messages[0].Blocks), "Stopped. Nothing was published.") {
		t.Fatal(messages)
	}
	// An action interrupted before preparation also gets a card on reload.
	m, _ = s.beginAction(ctx, thread, "fake", "")
	if err := r.RecoverInterrupted(ctx, thread); err != nil {
		t.Fatal(err)
	}
	messages, _ = s.Messages(ctx, thread.ID, 0)
	if !strings.Contains(string(messages[len(messages)-1].Blocks), `"title":"HTML artifact"`) {
		t.Fatal(messages)
	}
}

func TestArtifactCancelAPI(t *testing.T) {
	entered := make(chan struct{})
	p := fakeProvider{stream: func(ctx context.Context, _ llm.Request, token func(string) error) (llm.Response, error) {
		token("<!doctype html>")
		close(entered)
		<-ctx.Done()
		return llm.Response{}, ctx.Err()
	}}
	handler, s, _ := apiFixture(t, p)
	thread := testThread(t, s)
	path := "/api/ai-chat/threads/" + thread.ID
	finished := make(chan *httptest.ResponseRecorder, 1)
	go func() { finished <- request(handler, "POST", path+"/actions", `{"kind":"artifact"}`, "octocat", "on") }()
	select {
	case <-entered:
	case <-time.After(5 * time.Second):
		t.Fatal("generation never started")
	}
	busy := request(handler, "POST", path+"/actions", `{"kind":"artifact"}`, "octocat", "on")
	if busy.Code != 429 {
		t.Fatal(busy.Code)
	}
	stop := request(handler, "POST", path+"/cancel", ``, "octocat", "on")
	if stop.Code != 204 {
		t.Fatal(stop.Code)
	}
	select {
	case response := <-finished:
		if !strings.Contains(response.Body.String(), "Stopped. Nothing was published.") {
			t.Fatal(response.Body.String())
		}
	case <-time.After(5 * time.Second):
		t.Fatal("generation did not stop")
	}
	var count int
	s.DB.QueryRow(`SELECT count(*) FROM ai_chat_artifacts`).Scan(&count)
	if count != 0 {
		t.Fatal(count)
	}
}

func TestArtifactEncodedSecrets(t *testing.T) {
	const secret = "SECRET-SENTINEL"
	body := `<!doctype html><html><head><title>SECRET&#45;SENTINEL</title></head><body data-value="SECRET&#45;SENTINEL">SECRET&#45;SENTINEL<script>if (1 < 2) { document.body.dataset.ok = 'yes' }</script></body></html>`
	redacted := redactArtifactHTML(body, []string{secret})
	if strings.Contains(redacted, "SENTINEL") || !strings.Contains(redacted, "if (1 < 2)") {
		t.Fatal(redacted)
	}
	if title := artifactDocumentTitle(redacted); title != "[redacted]" {
		t.Fatal(title)
	}
	if err := validateArtifact(redacted); err != nil {
		t.Fatal(err)
	}
}

func TestArtifactSignatureBindsAllParameters(t *testing.T) {
	baseline := artifactSignature("key", "tenant", "meal-abcdef", 1, 100, "0")
	for _, changed := range []string{
		artifactSignature("other", "tenant", "meal-abcdef", 1, 100, "0"),
		artifactSignature("key", "other", "meal-abcdef", 1, 100, "0"),
		artifactSignature("key", "tenant", "other-abcdef", 1, 100, "0"),
		artifactSignature("key", "tenant", "meal-abcdef", 2, 100, "0"),
		artifactSignature("key", "tenant", "meal-abcdef", 1, 101, "0"),
		artifactSignature("key", "tenant", "meal-abcdef", 1, 100, "1"),
	} {
		if baseline == changed {
			t.Fatal("signature did not bind a parameter")
		}
	}
}

func TestInterruptedArtifactRevisionKeepsTarget(t *testing.T) {
	ctx := context.Background()
	s := testStore(t)
	thread := testThread(t, s)
	m, _ := s.beginAction(ctx, thread, "fake", "")
	a, err := s.publishArtifact(ctx, t.TempDir(), thread, &m, tools.ArtifactRequest{Title: "Meal"}, testHTML)
	if err != nil {
		t.Fatal(err)
	}
	m.Status = "completed"
	if err = s.FinishMessage(ctx, m); err != nil {
		t.Fatal(err)
	}
	m, err = s.beginAction(ctx, thread, "fake", a.ID)
	if err != nil {
		t.Fatal(err)
	}
	if err = s.RecoverInterrupted(ctx, thread.ID); err != nil {
		t.Fatal(err)
	}
	messages, _ := s.Messages(ctx, thread.ID, 0)
	if !strings.Contains(string(messages[len(messages)-1].Blocks), a.ID) {
		t.Fatal("lost revision target")
	}
}
