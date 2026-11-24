package aichat

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/elasticclaw/elasticclaw/pkg/hub/aichat/llm"
	"github.com/elasticclaw/elasticclaw/pkg/hub/aichat/tools"
)

func TestArtifactKitAPI(t *testing.T) {
	var deps Deps
	handler, _, _ := apiFixture(t, fakeProvider{}, func(d *Deps) {
		copyKit(t, d.ManagedDir("workspace"))
		d.ArtifactKey = func() string { return "key" }
		d.FeatureStage = func(context.Context, string) string { return "beta" }
		deps = *d
	})
	rec := request(handler, "GET", "/api/ai-chat/sources?workspace=workspace", "", "alice", "on")
	var sources SourcesResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &sources); err != nil || sources.ArtifactKit == nil {
		t.Fatal(rec.Body.String(), err)
	}
	if sources.ArtifactKit.Templates[0].PreviewURL != "" {
		t.Fatal(sources.ArtifactKit)
	}
	rec = request(handler, "GET", "/api/ai-chat/artifact-kit?workspace=workspace", "", "alice", "on")
	var info ArtifactKitInfo
	if err := json.Unmarshal(rec.Body.Bytes(), &info); err != nil || rec.Code != 200 || rec.Header().Get("Cache-Control") != "no-store" || len(info.Templates) != 2 || info.Templates[0].PreviewURL == "" {
		t.Fatal(rec.Body.String(), err)
	}
	signed := info.Templates[0].PreviewURL
	raw := RawArtifactRoutes(deps)
	preview := httptest.NewRecorder()
	for _, h := range []string{"Access-Control-Allow-Origin", "Access-Control-Allow-Headers", "Access-Control-Allow-Methods"} {
		preview.Header().Set(h, "*")
	}
	raw.ServeHTTP(preview, httptest.NewRequest("GET", signed, nil))
	if preview.Code != 200 || !strings.Contains(preview.Body.String(), "<style data-ds-injected>") {
		t.Fatal(preview.Code, preview.Body.String())
	}
	expected := httptest.NewRecorder()
	writeArtifactHTML(expected, []byte("body"), "inline", "one-pager.html")
	if !reflect.DeepEqual(preview.Header(), expected.Header()) {
		t.Fatal(preview.Header(), expected.Header())
	}
	if err := validateArtifact(preview.Body.String()); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name, workspace, id, sig string
		exp                      int64
		off                      bool
	}{
		{name: "bad signature", workspace: "workspace", id: "one-pager", sig: "00"},
		{name: "expired", workspace: "workspace", id: "one-pager", exp: time.Now().Add(-time.Minute).Unix()},
		{name: "far future", workspace: "workspace", id: "one-pager", exp: time.Now().Add(time.Hour).Unix()},
		{name: "unknown workspace", workspace: "unknown", id: "one-pager"},
		{name: "unknown template", workspace: "workspace", id: "missing"},
		{name: "bad id", workspace: "workspace", id: "BAD"},
		{name: "feature off", workspace: "workspace", id: "one-pager", off: true},
		{name: "wrong domain", workspace: "workspace", id: "one-pager", sig: artifactSignature("key", "tenant", "one-pager", 1, time.Now().Add(time.Minute).Unix(), "0")},
	} {
		t.Run(tc.name, func(t *testing.T) {
			exp := tc.exp
			if exp == 0 {
				exp = time.Now().Add(time.Minute).Unix()
			}
			sig := tc.sig
			if sig == "" {
				sig = templatePreviewSignature("key", "tenant", tc.workspace, tc.id, exp)
			}
			q := url.Values{"workspace": {tc.workspace}, "tenant": {"tenant"}, "exp": {strconv.FormatInt(exp, 10)}, "sig": {sig}}
			d := deps
			if tc.off {
				d.FeatureStage = func(context.Context, string) string { return "off" }
			}
			rr := httptest.NewRecorder()
			RawArtifactRoutes(d).ServeHTTP(rr, httptest.NewRequest("GET", "/artifact-templates/"+tc.id+"/preview?"+q.Encode(), nil))
			if rr.Code != 404 {
				t.Fatal(rr.Code, rr.Body.String())
			}
		})
	}
	for _, tc := range []struct {
		workspace, login, feature string
		status                    int
	}{{"unknown", "alice", "on", 404}, {"workspace", "", "on", 401}, {"workspace", "alice", "off", 404}} {
		rr := request(handler, "GET", "/api/ai-chat/artifact-kit?workspace="+tc.workspace, "", tc.login, tc.feature)
		if rr.Code != tc.status {
			t.Fatal(rr.Code, rr.Body.String())
		}
	}
	if err := os.Remove(filepath.Join(deps.ManagedDir("workspace"), "ai_chat.yaml")); err != nil {
		t.Fatal(err)
	}
	if rr := request(handler, "GET", "/api/ai-chat/artifact-kit?workspace=workspace", "", "alice", "on"); rr.Code != 404 || strings.TrimSpace(rr.Body.String()) != "Not configured for this workspace" {
		t.Fatal(rr.Code, rr.Body.String())
	}
}
func TestArtifactKitNoSigningKey(t *testing.T) {
	handler, _, _ := apiFixture(t, fakeProvider{}, func(d *Deps) { copyKit(t, d.ManagedDir("workspace")) })
	rec := request(handler, "GET", "/api/ai-chat/artifact-kit?workspace=workspace", "", "alice", "on")
	var info ArtifactKitInfo
	if err := json.Unmarshal(rec.Body.Bytes(), &info); err != nil || rec.Code != 200 || len(info.Templates) != 2 || info.Templates[0].PreviewURL != "" {
		t.Fatal(rec.Body.String(), err)
	}
}
func TestArtifactTemplateMessageAndActionAPI(t *testing.T) {
	calls := 0
	p := fakeProvider{stream: func(_ context.Context, req llm.Request, token func(string) error) (llm.Response, error) {
		calls++
		if len(req.Tools) > 0 {
			return llm.Response{ToolCalls: []llm.ToolCall{{ID: "call", Name: "start_artifact", Arguments: `{"title":"Report","brief":"Build","template":"status-report"}`}}}, nil
		}
		return llm.Response{}, token(testHTML)
	}}
	var managed string
	handler, s, _ := apiFixture(t, p, func(d *Deps) {
		managed = d.ManagedDir("workspace")
		copyKit(t, managed)
		d.ArtifactKey = func() string { return "key" }
	})
	rec := request(handler, "POST", "/api/ai-chat/threads", `{"workspace":"workspace","mode":"ask_the_data"}`, "alice", "on")
	var thread Thread
	if err := json.Unmarshal(rec.Body.Bytes(), &thread); err != nil {
		t.Fatal(err)
	}
	path := "/api/ai-chat/threads/" + thread.ID
	for _, tc := range []struct {
		suffix, body string
		status       int
	}{
		{"/actions", `{"kind":"ticket","template":"free"}`, 400},
	} {
		rr := request(handler, "POST", path+tc.suffix, tc.body, "alice", "on")
		if rr.Code != tc.status {
			t.Fatal(rr.Code, rr.Body.String())
		}
	}
	rec = request(handler, "POST", path+"/messages", `{"text":"Build","artifactTemplate":" One-Pager "}`, "alice", "on")
	if rec.Code != 200 {
		t.Fatal(rec.Code, rec.Body.String())
	}
	card := latestArtifactCard(t, s, thread)
	if calls != 2 || card.Template != "one-pager" || card.TemplateName != "One-pager" {
		t.Fatal(card, calls)
	}
	rec = request(handler, "POST", path+"/actions", fmt.Sprintf(`{"kind":"artifact","artifactId":%q,"title":"Report","template":"status-report"}`, card.ArtifactID), "alice", "on")
	if rec.Code != 200 {
		t.Fatal(rec.Code, rec.Body.String())
	}
	card = latestArtifactCard(t, s, thread)
	if card.Template != "status-report" || card.Version != 2 {
		t.Fatal(card)
	}
	rec = request(handler, "GET", "/api/ai-chat/artifacts/"+card.Slug, "", "alice", "on")
	var metadata Artifact
	if err := json.Unmarshal(rec.Body.Bytes(), &metadata); err != nil || metadata.TemplateID != "status-report" || metadata.TemplateName != "Status report" {
		t.Fatal(rec.Body.String(), err)
	}
	if err := os.Remove(filepath.Join(managed, "ai_chat", "templates", "status-report.html")); err != nil {
		t.Fatal(err)
	}
	rec = request(handler, "GET", "/api/ai-chat/artifacts/"+card.Slug, "", "alice", "on")
	if err := json.Unmarshal(rec.Body.Bytes(), &metadata); err != nil || metadata.TemplateName != "status-report" {
		t.Fatal(rec.Body.String(), err)
	}
	// A retry accepts the same pin field and starts a fresh template-aware turn.
	failed, err := s.BeginTurn(context.Background(), thread, "Build again", "fake", false)
	if err != nil {
		t.Fatal(err)
	}
	failed.Status = "error"
	if err = s.FinishMessage(context.Background(), failed); err != nil {
		t.Fatal(err)
	}
	rec = request(handler, "POST", path+"/messages", `{"retry":true,"artifactTemplate":"free"}`, "alice", "on")
	if rec.Code != 200 {
		t.Fatal(rec.Code, rec.Body.String())
	}
	if card := latestArtifactCard(t, s, thread); card.Template != "" {
		t.Fatal(card)
	}
}

func TestArtifactTemplateInterviewAnswers(t *testing.T) {
	p := fakeProvider{stream: func(_ context.Context, req llm.Request, token func(string) error) (llm.Response, error) {
		if len(req.Tools) > 0 {
			return llm.Response{ToolCalls: []llm.ToolCall{{ID: "call", Name: "start_artifact", Arguments: `{"title":"Report","brief":"Build","template":"status-report"}`}}}, nil
		}
		return llm.Response{}, token(testHTML)
	}}
	handler, s, _ := apiFixture(t, p, func(d *Deps) { copyKit(t, d.ManagedDir("workspace")) })
	thread := testThread(t, s)
	ctx := context.Background()
	m, err := s.BeginTurn(ctx, thread, "explore", "fake", false)
	if err != nil {
		t.Fatal(err)
	}
	interview, err := tools.ParseInterview(json.RawMessage(testInterviewArgs))
	if err != nil {
		t.Fatal(err)
	}
	interview.QuestionSetID = "set-templates"
	m.Interview, _ = json.Marshal(interview)
	m.Status = "completed"
	if err = s.FinishMessage(ctx, m); err != nil {
		t.Fatal(err)
	}
	answers := InterviewAnswers{QuestionSetID: interview.QuestionSetID, Steps: []InterviewStepAnswers{{Skipped: true}, {Skipped: true}}}
	raw, _ := json.Marshal(map[string]any{"answers": answers, "artifactTemplate": "one-pager"})
	rec := request(handler, "POST", "/api/ai-chat/threads/"+thread.ID+"/messages", string(raw), "Octocat", "on")
	if rec.Code != 200 {
		t.Fatal(rec.Code, rec.Body.String())
	}
	if card := latestArtifactCard(t, s, thread); card.Template != "one-pager" {
		t.Fatal(card)
	}
}

func TestArtifactTemplatePreviewReloadsFiles(t *testing.T) {
	dir := t.TempDir()
	copyKit(t, dir)
	deps := Deps{ManagedDir: func(string) string { return dir }, Workspaces: func() ([]string, error) { return []string{"workspace"}, nil }, ArtifactKey: func() string { return "key" }, FeatureStage: func(context.Context, string) string { return "on" }}
	a := &api{deps: deps}
	signed := a.signedTemplatePreviewURL("tenant", "workspace", "one-pager")
	// Previews are scoped to the registry and do not need the bearer token or YAML.
	handler := RawArtifactRoutes(deps)
	serve := func() *httptest.ResponseRecorder {
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, httptest.NewRequest("GET", signed, nil))
		return rec
	}
	if rec := serve(); rec.Code != 200 {
		t.Fatal(rec.Code)
	}
	if err := os.WriteFile(filepath.Join(dir, "ai_chat", "design-system.html"), []byte("invalid"), 0600); err != nil {
		t.Fatal(err)
	}
	if rec := serve(); rec.Code != 200 || strings.Contains(rec.Body.String(), "data-ds-injected") {
		t.Fatal(rec.Code, rec.Body.String())
	}
	if err := os.WriteFile(filepath.Join(dir, "ai_chat", "templates", "one-pager.html"), []byte("invalid"), 0600); err != nil {
		t.Fatal(err)
	}
	if rec := serve(); rec.Code != 404 {
		t.Fatal(rec.Code, rec.Body.String())
	}
}
