package hub

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/elasticclaw/elasticclaw/pkg/types"
)

func TestResolveDefaultModelForKey(t *testing.T) {
	tests := []struct {
		name          string
		hubCfg        *types.HubConfig
		key           *types.LLMKeyConfig
		expectedModel string
	}{
		{
			name: "hub default matches key provider",
			hubCfg: &types.HubConfig{
				DefaultModel: "anthropic/claude-opus-4-5",
			},
			key: &types.LLMKeyConfig{
				Provider: "anthropic",
			},
			expectedModel: "anthropic/claude-opus-4-5",
		},
		{
			name: "hub default doesn't match - use provider default",
			hubCfg: &types.HubConfig{
				DefaultModel: "anthropic/claude-sonnet-4-6",
			},
			key: &types.LLMKeyConfig{
				Provider: "openai",
			},
			expectedModel: "openai/gpt-4o",
		},
		{
			name: "no hub default - use provider default",
			hubCfg: &types.HubConfig{
				DefaultModel: "",
			},
			key: &types.LLMKeyConfig{
				Provider: "fireworks",
			},
			expectedModel: "fireworks/accounts/fireworks/models/llama-v3p3-70b-instruct",
		},
		{
			name: "unknown provider - fall back to hub default",
			hubCfg: &types.HubConfig{
				DefaultModel: "anthropic/claude-sonnet-4-6",
			},
			key: &types.LLMKeyConfig{
				Provider: "unknown-provider",
			},
			expectedModel: "anthropic/claude-sonnet-4-6",
		},
		{
			name: "nil key - return hub default",
			hubCfg: &types.HubConfig{
				DefaultModel: "anthropic/claude-sonnet-4-6",
			},
			key:           nil,
			expectedModel: "anthropic/claude-sonnet-4-6",
		},
		{
			name: "groq provider",
			hubCfg: &types.HubConfig{
				DefaultModel: "anthropic/claude-sonnet-4-6",
			},
			key: &types.LLMKeyConfig{
				Provider: "groq",
			},
			expectedModel: "groq/llama-3.3-70b-versatile",
		},
		{
			name: "deepseek provider",
			hubCfg: &types.HubConfig{
				DefaultModel: "",
			},
			key: &types.LLMKeyConfig{
				Provider: "deepseek",
			},
			expectedModel: "deepseek/deepseek-chat",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := resolveDefaultModelForKey(tt.hubCfg, tt.key)
			if result != tt.expectedModel {
				t.Errorf("expected %s, got %s", tt.expectedModel, result)
			}
		})
	}
}

func TestGitHubAccessChecksReturnNotFoundForMissingClaw(t *testing.T) {
	s, _ := NewTestServerWithConfig(t, &types.HubConfig{
		Auth: &types.AuthConfig{
			Access: &types.AccessConfig{InteractRequiresTags: []string{"owner={user}"}},
		},
	}, "", "")

	for _, tt := range []struct {
		name   string
		method string
		path   string
		body   string
	}{
		{name: "patch claw", method: http.MethodPatch, path: "/api/claws/missing", body: `{"name":"new"}`},
		{name: "delete claw", method: http.MethodDelete, path: "/api/claws/missing"},
		{name: "post message", method: http.MethodPost, path: "/api/messages/missing", body: `{"content":"hello"}`},
	} {
		t.Run(tt.name, func(t *testing.T) {
			req := httptest.NewRequest(tt.method, tt.path, strings.NewReader(tt.body))
			req = req.WithContext(context.WithValue(req.Context(), ctxTenantKey{}, "test-tenant-id"))
			req = req.WithContext(context.WithValue(req.Context(), ctxGitHubLoginKey{}, "octocat"))
			rec := httptest.NewRecorder()

			switch tt.path {
			case "/api/claws/missing":
				req.SetPathValue("id", "missing")
				s.handleClawDetail(rec, req)
			default:
				s.handleMessages(rec, req)
			}

			if rec.Code != http.StatusNotFound {
				t.Fatalf("expected status %d, got %d", http.StatusNotFound, rec.Code)
			}
		})
	}
}

func TestCleanupExpiredOAuthCodes(t *testing.T) {
	s, _ := NewTestServerWithConfig(t, nil, "", "")
	now := time.Now()
	s.oauthCodes = map[string]pendingOAuthCode{
		"expired": {Token: "old", ExpiresAt: now.Add(-time.Second)},
		"valid":   {Token: "new", ExpiresAt: now.Add(time.Second)},
	}

	s.cleanupExpiredOAuthCodes(now)

	if _, ok := s.oauthCodes["expired"]; ok {
		t.Fatal("expected expired OAuth code to be removed")
	}
	if _, ok := s.oauthCodes["valid"]; !ok {
		t.Fatal("expected valid OAuth code to remain")
	}
}
