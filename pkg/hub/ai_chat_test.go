package hub

import (
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"testing"

	"github.com/elasticclaw/elasticclaw/pkg/hub/aichat"
)

func TestAIChatFeatureGateAndMe(t *testing.T) {
	t.Setenv("ELASTICCLAW_HUB_CONFIG", filepath.Join(t.TempDir(), "hub.yaml"))
	s, db := newFeatureFlagTestServer(t)
	flag, ok := findFeatureFlag("ai-chat")
	if !ok || flag.DefaultStage != FeatureStageOff {
		t.Fatalf("AI Chat must be registered default off: %#v", flag)
	}
	if _, err := db.Exec(`INSERT INTO hub_beta_testers(login, added_at) VALUES('tester', 1)`); err != nil {
		t.Fatal(err)
	}
	for _, stage := range []FeatureStage{FeatureStageOff, FeatureStageBeta, FeatureStageOn} {
		t.Run(string(stage), func(t *testing.T) {
			if _, err := db.Exec(`INSERT INTO hub_feature_flags(key, stage, updated_at) VALUES('ai-chat', ?, 1) ON CONFLICT(key) DO UPDATE SET stage=excluded.stage`, stage); err != nil {
				t.Fatal(err)
			}
			for _, user := range []struct {
				name, token string
				tester      bool
			}{
				{"tester", featureFlagSession(t, "TeStEr"), true},
				{"member", featureFlagSession(t, "member"), false},
				{"admin", featureFlagSession(t, "admin"), false},
				{"password", "hub-token", false},
			} {
				t.Run(user.name, func(t *testing.T) {
					enabled := stage == FeatureStageOn || stage == FeatureStageBeta && user.tester
					want := http.StatusNotFound
					if enabled {
						want = http.StatusOK
					}
					rec := featureFlagRequest(t, s, "GET", "/api/ai-chat/sources", "", user.token)
					if rec.Code != want {
						t.Fatalf("sources status = %d, want %d: %s", rec.Code, want, rec.Body.String())
					}
					if !enabled {
						rec = featureFlagRequest(t, s, "POST", "/api/ai-chat/sources", "", user.token)
						if rec.Code != http.StatusNotFound {
							t.Fatalf("disabled POST status = %d", rec.Code)
						}
					}
					rec = featureFlagRequest(t, s, "GET", "/api/auth/me", "", user.token)
					if rec.Code != http.StatusOK {
						t.Fatalf("me status = %d", rec.Code)
					}
					var me struct {
						Features []string `json:"features"`
					}
					decodeFeatureFlagResponse(t, rec, &me)
					if slices.Contains(me.Features, "ai-chat") != enabled {
						t.Fatalf("me features = %v, want enabled %v", me.Features, enabled)
					}
				})
			}
			rec := featureFlagRequest(t, s, "GET", "/api/ai-chat/sources", "", "")
			if rec.Code != http.StatusUnauthorized {
				t.Fatalf("unauthenticated status = %d", rec.Code)
			}
			rec = featureFlagRequest(t, s, "GET", "/api/ai-chat/future-route", "", "")
			if rec.Code != http.StatusUnauthorized {
				t.Fatalf("namespace auth status = %d", rec.Code)
			}
		})
	}
}

func TestAIChatSourcesUsesWorkspaceManagedDirectory(t *testing.T) {
	t.Setenv("ELASTICCLAW_HUB_CONFIG", filepath.Join(t.TempDir(), "hub.yaml"))
	s, db := newFeatureFlagTestServer(t)
	if _, err := db.Exec(`INSERT INTO hub_feature_flags(key, stage, updated_at) VALUES('ai-chat', 'on', 1)`); err != nil {
		t.Fatal(err)
	}
	dir := workspaceManagedDir("product")
	if err := os.MkdirAll(dir, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(workspacesDir(), "product", "elasticclaw-config.yaml"), []byte("name: product\nrepositories: [example/product]\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "ai_chat.yaml"), []byte("about: Product assistant\n"), 0600); err != nil {
		t.Fatal(err)
	}
	rec := featureFlagRequest(t, s, "GET", "/api/ai-chat/sources", "", "hub-token")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", rec.Code, rec.Body.String())
	}
	var response aichat.SourcesResponse
	decodeFeatureFlagResponse(t, rec, &response)
	if !response.Configured || response.Workspace != "product" || len(response.Workspaces) != 1 {
		t.Fatalf("sources = %#v", response)
	}
	var count int
	if err := db.QueryRow(`SELECT COUNT(*) FROM sqlite_master WHERE type='table' AND name LIKE 'ai_chat_%'`).Scan(&count); err != nil || count != 6 {
		t.Fatalf("openDB tables = %d, %v", count, err)
	}
}

func TestAIChatSourcesIgnoresInvalidWorkflow(t *testing.T) {
	t.Setenv("ELASTICCLAW_HUB_CONFIG", filepath.Join(t.TempDir(), "hub.yaml"))
	s, db := newFeatureFlagTestServer(t)
	if _, err := db.Exec(`INSERT INTO hub_feature_flags(key, stage, updated_at) VALUES('ai-chat', 'on', 1)`); err != nil {
		t.Fatal(err)
	}
	for name, body := range map[string]string{
		"product/elasticclaw-config.yaml":           "name: product\nrepositories: [example/product]\n",
		"product/workflows/broken.yaml":             "steps: [",
		"product/.elasticclaw-managed/ai_chat.yaml": "about: Product assistant\n",
		".hidden/.elasticclaw-managed/ai_chat.yaml": "about: Hidden\n",
		"not-a-directory":                           "ignored",
	} {
		path := filepath.Join(workspacesDir(), name)
		if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(body), 0600); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := loadExternalWorkspace("product"); err == nil {
		t.Fatal("expected invalid workflow to fail the full workspace loader")
	}
	workspaces, err := loadExternalWorkspaces()
	if err != nil || len(workspaces) != 0 {
		t.Fatalf("full workspace listing = %#v, %v", workspaces, err)
	}
	rec := featureFlagRequest(t, s, "GET", "/api/ai-chat/sources", "", "hub-token")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", rec.Code, rec.Body.String())
	}
	var response aichat.SourcesResponse
	decodeFeatureFlagResponse(t, rec, &response)
	if !response.Configured || response.Workspace != "product" || !slices.Equal(response.Workspaces, []string{"product"}) {
		t.Fatalf("sources = %#v", response)
	}
}
