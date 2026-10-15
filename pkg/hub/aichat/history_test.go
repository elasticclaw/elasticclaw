package aichat

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/elasticclaw/elasticclaw/pkg/hub/aichat/llm"
	"github.com/elasticclaw/elasticclaw/pkg/hub/aichat/tools"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
)

func TestHistoryListsAndDeletion(t *testing.T) {
	ctx := context.Background()
	var deps Deps
	h, s, published := apiFixture(t, nil, func(d *Deps) {
		d.ArtifactKey = func() string { return "test-key" }
		d.FeatureStage = func(context.Context, string) string { return "on" }
		d.TenantID = func(r *http.Request) string {
			if v := r.Header.Get("Tenant"); v != "" {
				return v
			}
			return "tenant"
		}
		deps = *d
	})
	thread := testThread(t, s)
	a := addTicketArtifact(t, s, thread, "Prototype")
	deleted := addTicketArtifact(t, s, thread, "Deleted")
	if _, err := s.DeleteArtifact(ctx, thread.TenantID, thread.OwnerLogin, deleted.Slug); err != nil {
		t.Fatal(err)
	}
	messages, err := s.Messages(ctx, thread.ID, 0)
	if err != nil {
		t.Fatal(err)
	}
	m := messages[0]
	for i := 0; i < 2; i++ {
		if _, err := s.DB.Exec(`INSERT INTO ai_chat_handoffs(id,thread_id,message_id,kind,provider,external_ref,url,created_at) VALUES(?,?,?,'ticket','linear',?,'https://linear.app',?)`, fmt.Sprint(i), thread.ID, m.ID, fmt.Sprintf("PRO-%d", i), i); err != nil {
			t.Fatal(err)
		}
	}
	other, _ := s.CreateThread(ctx, "tenant", "other", "workspace", "write_brief")
	addTicketArtifact(t, s, other, "Private")
	foreign, _ := s.CreateThread(ctx, "foreign", "octocat", "workspace", "ask_the_data")
	addTicketArtifact(t, s, foreign, "Other tenant")
	archived := testThread(t, s)
	yes := true
	if err := s.UpdateThread(ctx, archived, nil, &yes); err != nil {
		t.Fatal(err)
	}
	threads, err := s.Threads(ctx, "tenant", "OCTOCAT")
	if err != nil || len(threads) != 1 || threads[0].ID != thread.ID || threads[0].ArtifactCount != 1 || threads[0].Ticket != "PRO-1" {
		t.Fatalf("%+v %v", threads, err)
	}
	items, err := s.Artifacts(ctx, "tenant", "octocat")
	if err != nil || len(items) != 1 || items[0].ID != a.ID || items[0].ThreadID != thread.ID || items[0].URL != "/artifacts/"+a.Slug {
		t.Fatalf("%+v %v", items, err)
	}
	for _, route := range []string{"/api/ai-chat/threads", "/api/ai-chat/artifacts"} {
		rec := request(h, "GET", route, "", "octocat", "on")
		if rec.Code != 200 || strings.Contains(rec.Body.String(), "Other tenant") || strings.Contains(rec.Body.String(), "Private") {
			t.Fatal(rec.Code, rec.Body)
		}
	}
	path := "/api/ai-chat/artifacts/" + a.Slug
	for _, method := range []string{"GET", "DELETE"} {
		if rec := request(h, method, path, "", "other", "on"); rec.Code != 404 {
			t.Fatal(method, rec.Code)
		}
		req := httptest.NewRequest(method, path, nil)
		req.Header.Set("Login", "octocat")
		req.Header.Set("Feature", "on")
		req.Header.Set("Tenant", "foreign")
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		if rec.Code != 404 {
			t.Fatal(method, rec.Code)
		}
	}
	deps.ArtifactsDir = filepath.Dir(filepath.Dir(filepath.Dir(a.Path)))
	raw := (&api{deps: deps}).signedArtifactURL(a, "0")
	before := httptest.NewRecorder()
	RawArtifactRoutes(deps).ServeHTTP(before, httptest.NewRequest("GET", raw, nil))
	if before.Code != 200 {
		t.Fatal("raw must work before deletion", before.Code, before.Body)
	}
	if rec := request(h, "GET", path, "", "octocat", "on"); rec.Code != 200 {
		t.Fatal(rec.Code, rec.Body)
	}
	for i := 0; i < 2; i++ {
		if rec := request(h, "DELETE", path, "", "octocat", "on"); rec.Code != 204 {
			t.Fatal(rec.Code, rec.Body)
		}
	}
	if len(*published) != 2 || (*published)[0].ID != thread.ID {
		t.Fatal(*published)
	}
	for _, suffix := range []string{"", "?v=1"} {
		if rec := request(h, "GET", path+suffix, "", "octocat", "on"); rec.Code != 404 {
			t.Fatal(rec.Code)
		}
	}
	rec := httptest.NewRecorder()
	RawArtifactRoutes(deps).ServeHTTP(rec, httptest.NewRequest("GET", raw, nil))
	if rec.Code != 404 {
		t.Fatal(rec.Code)
	}
	if rec := request(h, "DELETE", "/api/ai-chat/artifacts/unknown", "", "octocat", "on"); rec.Code != 404 {
		t.Fatal(rec.Code)
	}
	for _, method := range []string{"GET", "DELETE"} {
		if rec := request(h, method, path, "", "octocat", "off"); rec.Code != 404 {
			t.Fatal(rec.Code)
		}
		if rec := request(h, method, path, "", "", "on"); rec.Code != 401 {
			t.Fatal(rec.Code)
		}
	}
	rec = request(h, "GET", "/api/ai-chat/threads/"+thread.ID, "", "octocat", "on")
	if rec.Code != 200 || !strings.Contains(rec.Body.String(), `"deleted":true`) {
		t.Fatal(rec.Code, rec.Body)
	}
	history, err := s.History(ctx, thread.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(historyContent(history[0]), "[Artifact Prototype deleted]") || strings.Contains(historyContent(history[0]), "artifactId:") {
		t.Fatal(historyContent(history[0]))
	}
	var blocks string
	if err := s.DB.QueryRow(`SELECT blocks_json FROM ai_chat_messages WHERE id=?`, m.ID).Scan(&blocks); err != nil || strings.Contains(blocks, `"deleted"`) {
		t.Fatal(blocks, err)
	}
	runner := &Runner{Store: s}
	if _, err := runner.ticketArtifact(ctx, thread, a.ID); err == nil {
		t.Fatal("accepted deleted artifact")
	}
	if artifact, err := runner.ticketArtifact(ctx, thread, ""); err != nil || artifact != nil {
		t.Fatal(artifact, err)
	}
	if rec := request(h, "POST", "/api/ai-chat/threads/"+thread.ID+"/actions", fmt.Sprintf(`{"kind":"ticket","artifactId":%q}`, a.ID), "octocat", "on"); rec.Code != 404 {
		t.Fatal(rec.Code, rec.Body)
	}
}

func TestHistoryOrderingAndLimits(t *testing.T) {
	ctx := context.Background()
	s := testStore(t)
	for i := 0; i < 105; i++ {
		thread := testThread(t, s)
		if _, err := s.DB.Exec(`UPDATE ai_chat_threads SET title=?,updated_at=? WHERE id=?`, fmt.Sprint(i), i, thread.ID); err != nil {
			t.Fatal(err)
		}
		if _, err := s.DB.Exec(`INSERT INTO ai_chat_artifacts(id,slug,tenant_id,workspace,owner_login,thread_id,title,current_version,created_at,updated_at) VALUES(?,?,'tenant','workspace','octocat',?,'Title',1,?,?)`, fmt.Sprint(i), fmt.Sprint(i), thread.ID, i, i); err != nil {
			t.Fatal(err)
		}
	}
	threads, err := s.Threads(ctx, "tenant", "octocat")
	if err != nil || len(threads) != 100 || threads[0].Title != "104" || threads[99].Title != "5" {
		t.Fatal(threads, err)
	}
	artifacts, err := s.Artifacts(ctx, "tenant", "octocat")
	if err != nil || len(artifacts) != 100 || artifacts[0].ID != "104" || artifacts[99].ID != "5" {
		t.Fatal(artifacts, err)
	}
	raw, _ := json.Marshal(threads[0])
	if strings.Contains(string(raw), "ownerLogin") || strings.Contains(string(raw), "tenant") {
		t.Fatal(string(raw))
	}
}

func TestHistoryMutationNotifications(t *testing.T) {
	for _, fail := range []bool{false, true} {
		t.Run(fmt.Sprintf("message-failure-%t", fail), func(t *testing.T) {
			p := fakeProvider{stream: func(context.Context, llm.Request, func(string) error) (llm.Response, error) {
				if fail {
					return llm.Response{}, errors.New("failed")
				}
				return llm.Response{Text: "Hello"}, nil
			}}
			h, _, published := apiFixture(t, p)
			rec := request(h, "POST", "/api/ai-chat/threads", `{"workspace":"workspace","mode":"explore_idea"}`, "octocat", "on")
			var thread Thread
			if rec.Code != 201 || json.Unmarshal(rec.Body.Bytes(), &thread) != nil {
				t.Fatal(rec.Code, rec.Body)
			}
			if len(*published) != 1 {
				t.Fatal(*published)
			}
			path := "/api/ai-chat/threads/" + thread.ID
			for i, body := range []string{`{"title":"Renamed"}`, `{"archived":true}`, `{"archived":false}`} {
				if rec := request(h, "PATCH", path, body, "octocat", "on"); rec.Code != 204 || len(*published) != i+2 {
					t.Fatal(rec.Code, *published)
				}
			}
			if rec := request(h, "POST", path+"/messages", `{"text":"Hello"}`, "octocat", "on"); rec.Code != 200 || len(*published) != 5 {
				t.Fatal(rec.Code, *published)
			}
			for _, v := range *published {
				if v.ID != thread.ID || v.OwnerLogin != "octocat" || v.TenantID != "tenant" {
					t.Fatal(v)
				}
			}
		})
	}
	t.Run("interview", func(t *testing.T) {
		calls := 0
		p := fakeProvider{stream: func(context.Context, llm.Request, func(string) error) (llm.Response, error) {
			calls++
			if calls == 1 {
				return llm.Response{ToolCalls: []llm.ToolCall{{ID: "interview", Name: tools.AskInterview, Arguments: testInterviewArgs}}}, nil
			}
			return llm.Response{Text: "Thanks"}, nil
		}}
		h, s, published := apiFixture(t, p)
		thread := testThread(t, s)
		path := "/api/ai-chat/threads/" + thread.ID + "/messages"
		if rec := request(h, "POST", path, `{"text":"Explore"}`, "octocat", "on"); rec.Code != 200 {
			t.Fatal(rec.Code, rec.Body)
		}
		messages, err := s.Messages(context.Background(), thread.ID, 0)
		if err != nil {
			t.Fatal(err)
		}
		var interview tools.Interview
		if err = json.Unmarshal(messages[len(messages)-1].Interview, &interview); err != nil {
			t.Fatal(err)
		}
		body, _ := json.Marshal(map[string]any{"answers": InterviewAnswers{QuestionSetID: interview.QuestionSetID, Steps: []InterviewStepAnswers{{Skipped: true}, {Skipped: true}}}})
		if rec := request(h, "POST", path, string(body), "octocat", "on"); rec.Code != 200 || len(*published) != 2 {
			t.Fatal(rec.Code, rec.Body, *published)
		}
	})
	for _, fail := range []bool{false, true} {
		t.Run(fmt.Sprintf("artifact-failure-%t", fail), func(t *testing.T) {
			p := fakeProvider{stream: func(context.Context, llm.Request, func(string) error) (llm.Response, error) {
				if fail {
					return llm.Response{}, errors.New("failed")
				}
				return llm.Response{Text: testHTML}, nil
			}}
			h, s, published := apiFixture(t, p)
			thread := testThread(t, s)
			rec := request(h, "POST", "/api/ai-chat/threads/"+thread.ID+"/actions", `{"kind":"artifact","title":"Prototype"}`, "octocat", "on")
			if rec.Code != 200 || len(*published) != 1 {
				t.Fatal(rec.Code, rec.Body, *published)
			}
		})
		t.Run(fmt.Sprintf("ticket-failure-%t", fail), func(t *testing.T) {
			published := []Thread{}
			h, s := ticketFixture(t, nil, ticketHTTP(t, func(w http.ResponseWriter, _ *http.Request, _ map[string]any) {
				if fail {
					w.WriteHeader(502)
				} else {
					ticketSuccess(w, "Draft")
				}
			}), func(d *Deps) { d.Publish = func(v Thread) { published = append(published, v) } })
			thread := testThread(t, s)
			rec := request(h, "POST", "/api/ai-chat/threads/"+thread.ID+"/actions", `{"kind":"ticket","title":"Draft","body":"Evidence"}`, "octocat", "on")
			if rec.Code != 200 || len(published) != 1 {
				t.Fatal(rec.Code, rec.Body, published)
			}
			if fail {
				m, _, _ := latestTicket(t, s, thread)
				rec = request(h, "POST", "/api/ai-chat/threads/"+thread.ID+"/ticket-attempts/"+m.ID+"/dismiss", "", "octocat", "on")
				if rec.Code != 204 || len(published) != 2 {
					t.Fatal(rec.Code, rec.Body, published)
				}
			}
		})
	}
}
