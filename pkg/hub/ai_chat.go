package hub

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/elasticclaw/elasticclaw/pkg/hub/aichat"
	"github.com/elasticclaw/elasticclaw/pkg/hub/aichat/config"
	"github.com/elasticclaw/elasticclaw/pkg/hub/aichat/llm"
	"github.com/elasticclaw/elasticclaw/pkg/types"
	"nhooyr.io/websocket/wsjson"
)

func (s *Server) aiChatDeps() aichat.Deps {
	return aichat.Deps{
		DB:          s.db,
		GitHubToken: s.tokenForRepo,
		GitHubAPI:   s.githubBaseURL,
		HTTPClient:  http.DefaultClient,
		Secret: func(workspace, name string) (string, bool) {
			secrets, _ := loadWorkspaceSecrets(workspace)
			if value, ok := secrets[name]; ok {
				return value, value != ""
			}
			s.mu.RLock()
			defer s.mu.RUnlock()
			value, ok := s.hubCfg.Secrets[name]
			return value, ok && value != ""
		},
		LinearToken: func(workspace string) (string, bool) {
			cfg, err := config.Load(workspaceManagedDir(workspace))
			if err != nil || cfg.IssueTracker == nil {
				return "", false
			}
			tracker, ok := findWorkspaceIssueTracker(workspace, "linear", cfg.IssueTracker.Name)
			return tracker.Token, ok && tracker.Token != ""
		},
		WebAuth:     s.withWebAuth,
		WithFeature: s.withFeature,
		CallerLogin: func(r *http.Request) string { return aichat.OwnerLogin(githubLoginFromContext(r.Context())) },
		TenantID: func(r *http.Request) string {
			if tenant := tenantFromCtx(r); tenant != "" {
				return tenant
			}
			tenant, _ := s.githubTenantIDContext(r.Context())
			return tenant
		},
		Workspaces: listExternalWorkspaceNames,
		ManagedDir: workspaceManagedDir,
		LLM:        s.aiChatProvider,
		Publish: func(t aichat.Thread) {
			// Reuse the hub's tenant recipient selection, then restrict to the owner.
			event := types.WSMessage{Type: "ai_chat_thread_updated", Payload: map[string]string{"threadId": t.ID}}
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			for _, uc := range s.broadcastRecipients(t.TenantID, event) {
				if aichat.OwnerLogin(uc.githubLogin) != t.OwnerLogin {
					continue
				}
				if uc.send != nil {
					_ = uc.send(ctx, event)
				} else if uc.conn != nil {
					_ = wsjson.Write(ctx, uc.conn, event)
				}
			}
		},
	}
}

func (s *Server) aiChatProvider(name string) (llm.Provider, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	keys := s.hubCfg.LLMKeys
	if name != "" {
		var selected types.LLMKeysList
		for _, key := range keys {
			if key != nil && key.Name == name {
				selected = append(selected, key)
				break
			}
		}
		if len(selected) == 0 {
			return nil, fmt.Errorf("unknown LLM key")
		}
		keys = selected
	}
	// Chat's HTTP clients cannot resolve auth profiles into credentials.
	var usable types.LLMKeysList
	for _, key := range keys {
		if key != nil && (key.APIKey != "" || key.Provider == "ollama") {
			usable = append(usable, key)
		}
	}
	choice, err := selectAIConfigProvider(usable, s.hubCfg.DefaultModel)
	if err != nil {
		return nil, err
	}
	client := llm.Client{HTTP: http.DefaultClient, APIKey: choice.Key.APIKey, ModelName: choice.Model}
	if choice.Anthropic {
		client.Endpoint = "https://api.anthropic.com/v1/messages"
		client.ModelName = "claude-sonnet-4-6"
		if model := aiConfigModelForKey(choice.Key, s.hubCfg.DefaultModel); model != "" && modelMatchesProvider("anthropic", model) {
			client.ModelName = stripProviderPrefix(model)
		}
		return &llm.Anthropic{Client: client}, nil
	}
	client.Endpoint = strings.TrimRight(choice.Provider.BaseURL, "/") + "/chat/completions"
	return &llm.OpenAI{Client: client}, nil
}
