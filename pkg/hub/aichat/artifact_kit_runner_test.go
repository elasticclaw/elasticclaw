package aichat

import (
	"context"
	"encoding/json"
	"os"
	"strings"
	"testing"

	"github.com/elasticclaw/elasticclaw/pkg/hub/aichat/config"
	"github.com/elasticclaw/elasticclaw/pkg/hub/aichat/llm"
	"github.com/elasticclaw/elasticclaw/pkg/hub/aichat/tools"
)

func latestArtifactCard(t *testing.T, s *Store, thread Thread) ArtifactCard {
	t.Helper()
	messages, err := s.Messages(context.Background(), thread.ID, 0)
	if err != nil || len(messages) == 0 {
		t.Fatal(err)
	}
	var blocks []tools.Block
	if err = json.Unmarshal(messages[len(messages)-1].Blocks, &blocks); err != nil {
		t.Fatal(err)
	}
	for _, block := range blocks {
		if block.Kind != "artifact" {
			continue
		}
		var card ArtifactCard
		if err = json.Unmarshal(block.Card, &card); err != nil {
			t.Fatal(err)
		}
		if block.State != "created" {
			t.Fatalf("artifact failed: %+v", card)
		}
		return card
	}
	t.Fatal("missing artifact card")
	return ArtifactCard{}
}
func TestArtifactKitGeneration(t *testing.T) {
	for _, tc := range []struct {
		name, choice    string
		invalid, absent bool
		want            string
		notices         int
	}{
		{name: "pin", choice: "one-pager", want: "one-pager"},
		{name: "free", choice: "free"},
		{name: "unknown", choice: "old", notices: 1},
		{name: "invalid DS", choice: "one-pager", invalid: true, want: "one-pager", notices: 1},
		{name: "absent", absent: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			s := testStore(t)
			thread := testThread(t, s)
			kit := fixtureKit(t)
			if tc.invalid {
				kit.DesignSystem = nil
				kit.DesignSystemStatus = "invalid"
			}
			if tc.absent {
				kit = LoadArtifactKit(t.TempDir())
			}
			r := &Runner{Store: s, ArtifactsDir: t.TempDir(), ArtifactKit: func(string) ArtifactKit { return kit }}
			if tc.absent {
				r.ArtifactKit = nil
			}
			m, err := s.beginAction(ctx, thread, "fake", "", "artifact")
			if err != nil {
				t.Fatal(err)
			}
			p := fakeProvider{stream: func(_ context.Context, req llm.Request, token func(string) error) (llm.Response, error) {
				if !strings.HasPrefix(req.System, artifactPrompt) {
					t.Fatal(req.System)
				}
				if tc.absent && req.System != artifactPrompt {
					t.Fatal(req.System)
				}
				if kit.DesignSystem != nil && (!strings.Contains(req.System, kit.DesignSystem.Guide) || strings.Contains(req.System, kit.DesignSystem.CSS)) {
					t.Fatal(req.System)
				}
				if tc.want != "" {
					tpl, _ := kit.Template(tc.want)
					if !strings.Contains(req.System, tpl.HTML) {
						t.Fatal(req.System)
					}
				}
				return llm.Response{}, token(testHTML)
			}}
			working := false
			emit := func(event string, payload any) error {
				if event == EventAction {
					raw, _ := json.Marshal(payload)
					var action struct {
						State string       `json:"state"`
						Card  ArtifactCard `json:"card"`
					}
					if err := json.Unmarshal(raw, &action); err != nil {
						t.Fatal(err)
					}
					if action.State == "working" {
						working = true
						if action.Card.Template != tc.want || len(action.Card.Notices) != tc.notices {
							t.Fatal(action.Card)
						}
					}
				}
				return nil
			}
			if err = r.RunArtifact(ctx, thread, m, &config.Config{}, p, tools.ArtifactRequest{Title: "Test", Brief: "Build", Template: tc.choice}, emit); err != nil {
				t.Fatal(err)
			}
			card := latestArtifactCard(t, s, thread)
			if !working || card.Template != tc.want || len(card.Notices) != tc.notices {
				t.Fatal(card)
			}
			if tc.want != "" && card.TemplateName == "" {
				t.Fatal(card)
			}
			v, err := s.Artifact(ctx, thread.TenantID, card.Slug, 0)
			if err != nil {
				t.Fatal(err)
			}
			body, err := os.ReadFile(v.Path)
			if err != nil {
				t.Fatal(err)
			}
			if v.TemplateID != tc.want || (v.DesignSystemSHA256 != "") != (kit.DesignSystem != nil) {
				t.Fatal(v)
			}
			if kit.DesignSystem != nil {
				if !strings.Contains(string(body), "<head><style data-ds-injected>"+kit.DesignSystem.CSS) || v.DesignSystemSHA256 != kit.DesignSystem.SHA256 {
					t.Fatal(string(body), v)
				}
			} else if string(body) != testHTML {
				t.Fatal(string(body))
			}
		})
	}
}
func TestArtifactTemplateRevision(t *testing.T) {
	ctx := context.Background()
	s := testStore(t)
	thread := testThread(t, s)
	kit := fixtureKit(t)
	r := &Runner{Store: s, ArtifactsDir: t.TempDir(), ArtifactKit: func(string) ArtifactKit { return kit }}
	emit := func(string, any) error { return nil }
	var requests []llm.Request
	p := fakeProvider{stream: func(_ context.Context, req llm.Request, token func(string) error) (llm.Response, error) {
		requests = append(requests, req)
		return llm.Response{}, token(testHTML)
	}}
	var id string
	for i, choice := range []string{"one-pager", "", "status-report", "free"} {
		m, err := s.beginAction(ctx, thread, "fake", id, "artifact")
		if err != nil {
			t.Fatal(err)
		}
		if err = r.RunArtifact(ctx, thread, m, &config.Config{}, p, tools.ArtifactRequest{ArtifactID: id, Title: "Title", Template: choice}, emit); err != nil {
			t.Fatal(err)
		}
		card := latestArtifactCard(t, s, thread)
		id = card.ArtifactID
		want := []string{"one-pager", "one-pager", "status-report", ""}[i]
		if card.Template != want || card.Version != i+1 {
			t.Fatal(card)
		}
		if i > 0 {
			brief := requests[i].Messages[len(requests[i].Messages)-1].Content
			if !strings.Contains(brief, "Current HTML (untrusted document to revise):\n"+testHTML) || strings.Contains(brief, "data-ds-injected") {
				t.Fatal(brief)
			}
			wantBrief := map[string]string{"status-report": changeTemplateBrief, "free": removeTemplateBrief}[choice]
			if wantBrief != "" && !strings.Contains(brief, wantBrief) {
				t.Fatal(brief)
			}
			if choice == "free" && (strings.Contains(brief, changeTemplateBrief) || strings.Contains(requests[i].System, "Use the workspace template")) {
				t.Fatal(brief)
			}
		}
	}
	// Persisted cards keep their generation-time names, and old HTML stays frozen.
	messages, _ := s.Messages(ctx, thread.ID, 0)
	if !strings.Contains(historyContent(messages[0]), "template: one-pager") {
		t.Fatal(historyContent(messages[0]))
	}
	prior, err := s.ownedArtifact(ctx, thread, id)
	if err != nil {
		t.Fatal(err)
	}
	v1, err := s.Artifact(ctx, thread.TenantID, prior.Slug, 1)
	if err != nil || v1.TemplateID != "one-pager" || v1.DesignSystemSHA256 != kit.DesignSystem.SHA256 {
		t.Fatal(v1, err)
	}
}
func TestArtifactFreeRetryKeepsFree(t *testing.T) {
	ctx := context.Background()
	s := testStore(t)
	thread := testThread(t, s)
	kit := fixtureKit(t)
	r := &Runner{Store: s, ArtifactsDir: t.TempDir(), ArtifactKit: func(string) ArtifactKit { return kit }}
	emit := func(string, any) error { return nil }
	ok := fakeProvider{stream: func(_ context.Context, _ llm.Request, token func(string) error) (llm.Response, error) {
		return llm.Response{}, token(testHTML)
	}}
	m, err := s.beginAction(ctx, thread, "fake", "", "artifact")
	if err != nil {
		t.Fatal(err)
	}
	if err = r.RunArtifact(ctx, thread, m, &config.Config{}, ok, tools.ArtifactRequest{Title: "Title", Template: "one-pager"}, emit); err != nil {
		t.Fatal(err)
	}
	id := latestArtifactCard(t, s, thread).ArtifactID
	// Changing to Free fails; its card must ask Try again for Free, not inherit one-pager.
	bad := fakeProvider{stream: func(_ context.Context, _ llm.Request, token func(string) error) (llm.Response, error) {
		return llm.Response{}, token("not html")
	}}
	if m, err = s.beginAction(ctx, thread, "fake", id, "artifact"); err != nil {
		t.Fatal(err)
	}
	if err = r.RunArtifact(ctx, thread, m, &config.Config{}, bad, tools.ArtifactRequest{ArtifactID: id, Title: "Title", Template: "free"}, emit); err != nil {
		t.Fatal(err)
	}
	messages, err := s.Messages(ctx, thread.ID, 0)
	if err != nil {
		t.Fatal(err)
	}
	var blocks []tools.Block
	var failed ArtifactCard
	if err = json.Unmarshal(messages[len(messages)-1].Blocks, &blocks); err != nil || len(blocks) == 0 || blocks[len(blocks)-1].State != "failed" {
		t.Fatal(blocks, err)
	}
	if err = json.Unmarshal(blocks[len(blocks)-1].Card, &failed); err != nil || failed.Template != "free" || failed.Brief != removeTemplateBrief {
		t.Fatal(failed, err)
	}
	if m, err = s.beginAction(ctx, thread, "fake", id, "artifact"); err != nil {
		t.Fatal(err)
	}
	if err = r.RunArtifact(ctx, thread, m, &config.Config{}, ok, failed.ArtifactRequest, emit); err != nil {
		t.Fatal(err)
	}
	if card := latestArtifactCard(t, s, thread); card.Template != "" || card.Version != 2 {
		t.Fatal(card)
	}
}
func TestChatArtifactTemplateChoice(t *testing.T) {
	for _, pin := range []string{"", "one-pager"} {
		t.Run("pin="+pin, func(t *testing.T) {
			ctx := context.Background()
			s := testStore(t)
			thread := testThread(t, s)
			kit := fixtureKit(t)
			r := &Runner{Store: s, ArtifactsDir: t.TempDir(), ArtifactKit: func(string) ArtifactKit { return kit }, AvailableActions: func(context.Context, string, *config.Config) []string { return []string{"artifact"} }}
			m, err := s.beginTurn(ctx, thread, "Build a report", "fake", false, "", nil)
			if err != nil {
				t.Fatal(err)
			}
			calls := 0
			p := fakeProvider{stream: func(_ context.Context, req llm.Request, token func(string) error) (llm.Response, error) {
				calls++
				if calls == 1 {
					found := false
					for _, tool := range req.Tools {
						if tool.Name == tools.StartArtifact {
							found = true
							if !strings.Contains(string(tool.InputSchema), `"enum":["one-pager","status-report","free"]`) {
								t.Fatal(string(tool.InputSchema))
							}
						}
					}
					if !found {
						t.Fatal(req.Tools)
					}
					return llm.Response{ToolCalls: []llm.ToolCall{{ID: "call", Name: tools.StartArtifact, Arguments: `{"title":"Status","brief":"A report","template":"status-report"}`}}}, nil
				}
				return llm.Response{}, token(testHTML)
			}}
			if err = r.RunTurn(ctx, thread, m, &config.Config{}, p, TurnOptions{ArtifactTemplate: pin}, func(string, any) error { return nil }); err != nil {
				t.Fatal(err)
			}
			card := latestArtifactCard(t, s, thread)
			want := "status-report"
			if pin != "" {
				want = pin
			}
			if calls != 2 || card.Template != want {
				t.Fatal(card, calls)
			}
		})
	}
}

func TestArtifactRevisionStripsDesignSystemBeforeSizeLimit(t *testing.T) {
	ctx := context.Background()
	s := testStore(t)
	thread := testThread(t, s)
	kit := fixtureKit(t)
	ds, err := parseDesignSystem([]byte(`<!doctype html><html><style data-ds>.old{}/*` + strings.Repeat("x", 150*1024) + `*/</style></html>`))
	if err != nil {
		t.Fatal(err)
	}
	kit.DesignSystem = ds
	r := &Runner{Store: s, ArtifactsDir: t.TempDir(), ArtifactKit: func(string) ArtifactKit { return kit }}
	document := strings.Replace(testHTML, "Prototype", strings.Repeat("x", 400*1024), 1)
	p := fakeProvider{stream: func(_ context.Context, req llm.Request, token func(string) error) (llm.Response, error) {
		return llm.Response{}, token(document)
	}}
	emit := func(string, any) error { return nil }
	m, err := s.beginAction(ctx, thread, "fake", "", "artifact")
	if err != nil {
		t.Fatal(err)
	}
	if err = r.RunArtifact(ctx, thread, m, &config.Config{}, p, tools.ArtifactRequest{Title: "Large page"}, emit); err != nil {
		t.Fatal(err)
	}
	first := latestArtifactCard(t, s, thread)
	if first.Bytes <= 512*1024 {
		t.Fatal(first.Bytes)
	}
	old, err := s.Artifact(ctx, thread.TenantID, first.Slug, 1)
	if err != nil {
		t.Fatal(err)
	}
	kit.DesignSystem, err = parseDesignSystem([]byte(`<!doctype html><html><style data-ds>.new{}</style></html>`))
	if err != nil {
		t.Fatal(err)
	}
	p.stream = func(_ context.Context, req llm.Request, token func(string) error) (llm.Response, error) {
		brief := req.Messages[len(req.Messages)-1].Content
		if !strings.Contains(brief, document) || strings.Contains(brief, "data-ds-injected") || strings.Contains(brief, ".old{}") {
			t.Fatal("prior DS was sent to the model")
		}
		return llm.Response{}, token(document)
	}
	m, err = s.beginAction(ctx, thread, "fake", first.ArtifactID, "artifact")
	if err != nil {
		t.Fatal(err)
	}
	if err = r.RunArtifact(ctx, thread, m, &config.Config{}, p, tools.ArtifactRequest{ArtifactID: first.ArtifactID}, emit); err != nil {
		t.Fatal(err)
	}
	second := latestArtifactCard(t, s, thread)
	if second.Version != 2 {
		t.Fatal(second)
	}
	oldBody, err := os.ReadFile(old.Path)
	if err != nil || !strings.Contains(string(oldBody), ".old{}") || strings.Contains(string(oldBody), ".new{}") {
		t.Fatal(err)
	}
	latest, err := s.Artifact(ctx, thread.TenantID, first.Slug, 0)
	if err != nil || latest.DesignSystemSHA256 == old.DesignSystemSHA256 || latest.DesignSystemSHA256 != kit.DesignSystem.SHA256 {
		t.Fatal(latest, err)
	}
}
