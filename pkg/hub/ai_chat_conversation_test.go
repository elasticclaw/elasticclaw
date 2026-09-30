package hub

import (
	"context"
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
