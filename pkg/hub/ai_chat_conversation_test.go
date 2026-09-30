package hub

import (
	"context"
	"net/http"
	"os"
	"path/filepath"
	"testing"

	"github.com/elasticclaw/elasticclaw/pkg/hub/aichat"
	"github.com/elasticclaw/elasticclaw/pkg/types"
)

func TestAIChatPublishIsOwnerOnly(t *testing.T) {
	recipients := []string{}
	server := &Server{users: map[string]*userConn{}}
	for _, tc := range []struct{ name, tenant, login string }{{"owner", "tenant", "ALICE"}, {"other", "tenant", "bob"}, {"password", "tenant", ""}, {"other tenant", "other", "alice"}} {
		name := tc.name
		server.users[name] = &userConn{tenantID: tc.tenant, githubLogin: tc.login, send: func(_ context.Context, message types.WSMessage) error {
			recipients = append(recipients, name)
			payload, ok := message.Payload.(map[string]string)
			if message.Type != "ai_chat_thread_updated" || !ok || payload["threadId"] != "thread" || len(payload) != 1 {
				t.Errorf("event=%+v", message)
			}
			return nil
		}}
	}
	publish := server.aiChatDeps().Publish
	publish(aichat.Thread{ID: "thread", TenantID: "tenant", OwnerLogin: "alice"})
	if len(recipients) != 1 || recipients[0] != "owner" {
		t.Fatal(recipients)
	}
	recipients = nil
	publish(aichat.Thread{ID: "thread", TenantID: "tenant", OwnerLogin: "admin"})
	if len(recipients) != 1 || recipients[0] != "password" {
		t.Fatal(recipients)
	}
}

func TestAIChatWebAuthTenantAndCredentials(t *testing.T) {
	t.Setenv("ELASTICCLAW_HUB_CONFIG", filepath.Join(t.TempDir(), "hub.yaml"))
	s, db := newFeatureFlagTestServer(t)
	if _, err := db.Exec(`INSERT INTO hub_feature_flags(key, stage, updated_at) VALUES('ai-chat', 'on', 1)`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`UPDATE tenants SET token='hub-token' WHERE id='test-tenant-id'`); err != nil {
		t.Fatal(err)
	}
	dir := workspaceManagedDir("product")
	if err := os.MkdirAll(dir, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "ai_chat.yaml"), []byte("about: Test\n"), 0600); err != nil {
		t.Fatal(err)
	}
	s.hubCfg.LLMKeys = types.LLMKeysList{&types.LLMKeyConfig{Name: "profile", Provider: "anthropic", AuthProfile: "subscription"}}
	for _, tc := range []struct{ name, token, login string }{
		{"password", "hub-token", ""},
		{"github", featureFlagSession(t, "ALICE"), "alice"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var events []types.WSMessage
			s.users = map[string]*userConn{"owner": {tenantID: "test-tenant-id", githubLogin: tc.login, send: func(_ context.Context, event types.WSMessage) error {
				events = append(events, event)
				return nil
			}}}
			rec := featureFlagRequest(t, s, "POST", "/api/ai-chat/threads", `{"workspace":"product"}`, tc.token)
			if rec.Code != http.StatusCreated {
				t.Fatalf("create: %d %s", rec.Code, rec.Body.String())
			}
			var thread aichat.Thread
			decodeFeatureFlagResponse(t, rec, &thread)
			var tenant, owner string
			if err := db.QueryRow(`SELECT tenant_id,owner_login FROM ai_chat_threads WHERE id=?`, thread.ID).Scan(&tenant, &owner); err != nil {
				t.Fatal(err)
			}
			if tenant != s.users["owner"].tenantID || owner != aichat.OwnerLogin(tc.login) {
				t.Fatalf("identity: tenant=%q owner=%q", tenant, owner)
			}
			if len(events) != 1 || events[0].Type != "ai_chat_thread_updated" || events[0].Payload.(map[string]string)["threadId"] != thread.ID {
				t.Fatalf("events: %+v", events)
			}
			rec = featureFlagRequest(t, s, "POST", "/api/ai-chat/threads/"+thread.ID+"/messages", `{"text":"question"}`, tc.token)
			if rec.Code != http.StatusServiceUnavailable {
				t.Fatalf("credentials: %d %s", rec.Code, rec.Body.String())
			}
			var count int
			if err := db.QueryRow(`SELECT COUNT(*) FROM ai_chat_messages WHERE thread_id=?`, thread.ID).Scan(&count); err != nil || count != 0 {
				t.Fatalf("persisted messages=%d, err=%v", count, err)
			}
		})
	}
}

func TestAIChatProviderSkipsUnresolvedProfiles(t *testing.T) {
	s := &Server{hubCfg: &types.HubConfig{DefaultModel: "anthropic/model", LLMKeys: types.LLMKeysList{
		&types.LLMKeyConfig{Name: "profile", Provider: "anthropic", AuthProfile: "subscription"},
		&types.LLMKeyConfig{Name: "key", Provider: "openai", APIKey: "key", DefaultModel: "openai/fallback"},
	}}}
	p, err := s.aiChatProvider("")
	if err != nil || p.Model() != "fallback" {
		t.Fatalf("provider=%v err=%v", p, err)
	}
	if _, err := s.aiChatProvider("profile"); err == nil {
		t.Fatal("named profile must fail instead of falling back")
	}
	s.hubCfg.LLMKeys[0].APIKey = "key"
	if _, err := s.aiChatProvider("profile"); err != nil {
		t.Fatalf("profile with an explicit API key: %v", err)
	}
}

func TestAIChatProviderUsesExistingSelectionAndNamedKey(t *testing.T) {
	server := &Server{hubCfg: &types.HubConfig{DefaultModel: "openai/default-model", LLMKeys: types.LLMKeysList{
		&types.LLMKeyConfig{Name: "anthropic", Provider: "anthropic", APIKey: "key", DefaultModel: "anthropic/custom-model"},
		&types.LLMKeyConfig{Name: "openai", Provider: "openai", APIKey: "key", DefaultModel: "openai/key-model"},
		&types.LLMKeyConfig{Name: "local", Provider: "ollama", DefaultModel: "ollama/local-model"},
	}}}
	for _, tc := range []struct{ name, model string }{{"", "key-model"}, {"anthropic", "custom-model"}, {"local", "local-model"}} {
		provider, err := server.aiChatProvider(tc.name)
		if err != nil {
			t.Fatal(err)
		}
		if provider.Model() != tc.model {
			t.Fatalf("%s: %s", tc.name, provider.Model())
		}
	}
	if _, err := server.aiChatProvider("missing"); err == nil {
		t.Fatal("unknown key fell back")
	}
}
