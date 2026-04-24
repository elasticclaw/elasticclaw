package hub

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/elasticclaw/elasticclaw/pkg/config"
	"github.com/elasticclaw/elasticclaw/pkg/types"
	"gopkg.in/yaml.v3"
)

// ─── Request / response types ─────────────────────────────────────────────────

type aiConfigRequest struct {
	Message string          `json:"message"`
	History []aiChatMessage `json:"history"`
}

type aiChatMessage struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

type aiConfigResponse struct {
	Reply        string   `json:"reply"`
	ProposedYAML string   `json:"proposed_yaml,omitempty"`
	Placeholders []string `json:"placeholders,omitempty"`
}

type aiConfigApplyRequest struct {
	ProposedYAML string            `json:"proposed_yaml"`
	Secrets      map[string]string `json:"secrets"`
}

type aiConfigApplyResponse struct {
	Success    bool   `json:"success"`
	BackupPath string `json:"backup_path"`
}

type aiConfigRevertRequest struct {
	BackupPath string `json:"backup_path"`
}

type aiConfigBackupResponse struct {
	BackupPath string `json:"backup_path"`
	BackupTime string `json:"backup_time,omitempty"`
}

// ─── Route handlers ───────────────────────────────────────────────────────────

func (s *Server) handleAIConfig(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	var req aiConfigRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "invalid request", http.StatusBadRequest)
		return
	}
	if req.Message == "" {
		http.Error(w, "message required", http.StatusBadRequest)
		return
	}

	// Load and sanitize current hub config
	diskCfg, err := config.LoadHubConfig()
	if err != nil {
		http.Error(w, "failed to load hub config: "+err.Error(), http.StatusInternalServerError)
		return
	}
	sanitized, err := sanitizeHubConfig(diskCfg)
	if err != nil {
		http.Error(w, "failed to sanitize config: "+err.Error(), http.StatusInternalServerError)
		return
	}

	// Select LLM key
	s.mu.RLock()
	llmKeys := s.hubCfg.LLMKeys
	defaultModel := s.hubCfg.DefaultModel
	s.mu.RUnlock()

	reply, err := callLLMForConfig(sanitized, req.Message, req.History, llmKeys, defaultModel)
	if err != nil {
		http.Error(w, "LLM error: "+err.Error(), http.StatusInternalServerError)
		return
	}

	resp := aiConfigResponse{Reply: reply}

	// Extract YAML block if present
	if yaml := extractYAMLBlock(reply); yaml != "" {
		resp.ProposedYAML = yaml
		resp.Placeholders = extractPlaceholders(yaml)
	}

	jsonOK(w, resp)
}

func (s *Server) handleAIConfigApply(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	var req aiConfigApplyRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "invalid request", http.StatusBadRequest)
		return
	}
	if req.ProposedYAML == "" {
		http.Error(w, "proposed_yaml required", http.StatusBadRequest)
		return
	}

	// Substitute placeholders
	finalYAML := substitutePlaceholders(req.ProposedYAML, req.Secrets)

	// Restore masked secrets (*** from sanitized config) from disk before parsing.
	diskCfg, err := config.LoadHubConfig()
	if err != nil {
		http.Error(w, "failed to load hub config: "+err.Error(), http.StatusInternalServerError)
		return
	}
	finalYAML, err = restoreMaskedSecretsFromDisk(finalYAML, diskCfg)
	if err != nil {
		http.Error(w, "failed to restore masked secrets: "+err.Error(), http.StatusInternalServerError)
		return
	}

	// Parse and validate
	var newCfg types.HubConfig
	if err := yaml.Unmarshal([]byte(finalYAML), &newCfg); err != nil {
		http.Error(w, "invalid yaml: "+err.Error(), http.StatusBadRequest)
		return
	}
	if err := validateHubConfig(&newCfg); err != nil {
		http.Error(w, "validation failed: "+err.Error(), http.StatusBadRequest)
		return
	}

	// Backup current config
	backupPath, err := backupHubConfig()
	if err != nil {
		http.Error(w, "backup failed: "+err.Error(), http.StatusInternalServerError)
		return
	}

	// Save new config
	if err := config.SaveHubConfigNoSecretMerge(&newCfg); err != nil {
		http.Error(w, "save failed: "+err.Error(), http.StatusInternalServerError)
		return
	}

	// Hot-reload
	s.mu.Lock()
	s.hubCfg = &newCfg
	s.mu.Unlock()

	jsonOK(w, aiConfigApplyResponse{Success: true, BackupPath: backupPath})
}

func (s *Server) handleAIConfigRevert(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	var req aiConfigRevertRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "invalid request", http.StatusBadRequest)
		return
	}
	if req.BackupPath == "" {
		http.Error(w, "backup_path required", http.StatusBadRequest)
		return
	}

	// Validate path to prevent traversal
	hubYAMLPath, err := config.ActiveHubConfigPath()
	if err != nil {
		http.Error(w, "cannot determine hub config path: "+err.Error(), http.StatusInternalServerError)
		return
	}
	cleanHubYAMLPath := filepath.Clean(hubYAMLPath)
	cleanBackupPath := filepath.Clean(req.BackupPath)
	expectedPrefix := cleanHubYAMLPath + ".bak."
	if !strings.HasPrefix(cleanBackupPath, expectedPrefix) {
		http.Error(w, "invalid backup path", http.StatusBadRequest)
		return
	}
	suffix := strings.TrimPrefix(cleanBackupPath, expectedPrefix)
	if !backupTimestampSuffixRe.MatchString(suffix) {
		http.Error(w, "invalid backup path", http.StatusBadRequest)
		return
	}

	data, err := os.ReadFile(cleanBackupPath)
	if err != nil {
		http.Error(w, "cannot read backup: "+err.Error(), http.StatusInternalServerError)
		return
	}

	var restoredCfg types.HubConfig
	if err := yaml.Unmarshal(data, &restoredCfg); err != nil {
		http.Error(w, "backup file is corrupt: "+err.Error(), http.StatusInternalServerError)
		return
	}

	if err := config.SaveHubConfigNoSecretMerge(&restoredCfg); err != nil {
		http.Error(w, "save failed: "+err.Error(), http.StatusInternalServerError)
		return
	}

	// Hot-reload
	s.mu.Lock()
	s.hubCfg = &restoredCfg
	s.mu.Unlock()

	jsonOK(w, map[string]bool{"success": true})
}

func (s *Server) handleAIConfigBackup(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	hubYAMLPath, err := config.ActiveHubConfigPath()
	if err != nil {
		jsonOK(w, aiConfigBackupResponse{})
		return
	}

	dir := filepath.Dir(hubYAMLPath)
	base := filepath.Base(hubYAMLPath)
	entries, err := os.ReadDir(dir)
	if err != nil {
		jsonOK(w, aiConfigBackupResponse{})
		return
	}

	prefix := base + ".bak."
	var backups []string
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), prefix) {
			backups = append(backups, filepath.Join(dir, e.Name()))
		}
	}
	if len(backups) == 0 {
		jsonOK(w, aiConfigBackupResponse{})
		return
	}

	sort.Strings(backups)
	latest := backups[len(backups)-1]

	info, err := os.Stat(latest)
	var backupTime string
	if err == nil {
		backupTime = info.ModTime().UTC().Format(time.RFC3339)
	}

	jsonOK(w, aiConfigBackupResponse{BackupPath: latest, BackupTime: backupTime})
}

// ─── LLM call ────────────────────────────────────────────────────────────────

const aiConfigSystemPromptTemplate = `You are a configuration assistant for ElasticClaw hub. You help users configure their hub.yaml.

The current hub.yaml (with secrets masked as ***) is:
---
%s
---

Hub.yaml schema overview:
- url: public URL of the hub
- token: web UI password (shared secret)
- claw_token: token claws use to connect (required)
- default_model: default LLM model (e.g. "anthropic/claude-sonnet-4-6")
- providers: daytona, replicated, vercel, local sandbox providers
- github: list of GitHub App configs (app_id, private_key_pem, url)
- integrations: linear, shortcut workspace tokens and webhook secrets
- llm_keys: [{name, provider, api_key, default: bool}]
- ssh_public_keys: list of SSH public keys allowed for claw access
- secrets: named secrets map (used by factory webhook_secret_ref)
- auth: github_oauth config and access control (view/interact tag requirements)

When proposing config changes:
- Return the COMPLETE updated hub.yaml as a YAML code block (` + "```yaml ... ```" + `)
- For any secret values the user provides, include them directly
- For any secret values not yet provided, use __SECRET_NAME__ as placeholder (e.g. __LINEAR_TOKEN__, __GITHUB_CLIENT_SECRET__)
- Preserve all existing config that the user didn't ask to change
- Never remove claw_token or url
- Explain what you changed in plain English before the YAML block`

func callLLMForConfig(sanitizedYAML, message string, history []aiChatMessage, llmKeys types.LLMKeysList, defaultModel string) (string, error) {
	systemPrompt := fmt.Sprintf(aiConfigSystemPromptTemplate, sanitizedYAML)

	// Build message list
	var msgs []aiChatMessage
	msgs = append(msgs, history...)
	msgs = append(msgs, aiChatMessage{Role: "user", Content: message})

	// Select provider
	var anthropicKey, openaiKey string
	for _, k := range llmKeys {
		if k.Provider == "anthropic" && anthropicKey == "" {
			anthropicKey = k.APIKey
		}
		if k.Provider == "openai" && openaiKey == "" {
			openaiKey = k.APIKey
		}
	}

	if anthropicKey != "" {
		return callAnthropic(anthropicKey, systemPrompt, msgs)
	}
	if openaiKey != "" {
		return callOpenAI(openaiKey, systemPrompt, msgs)
	}
	if len(llmKeys) == 0 {
		return "", fmt.Errorf("no LLM keys configured")
	}
	defaultProvider := strings.TrimSpace(strings.SplitN(defaultModel, "/", 2)[0])
	availableProviders := uniqueProviders(llmKeys)
	if defaultProvider != "" {
		return "", fmt.Errorf("no supported LLM key configured for default_model provider %q (supported providers: anthropic, openai; configured: %s)", defaultProvider, strings.Join(availableProviders, ", "))
	}
	return "", fmt.Errorf("no supported LLM key configured (supported providers: anthropic, openai; configured: %s)", strings.Join(availableProviders, ", "))
}

func callAnthropic(apiKey, systemPrompt string, msgs []aiChatMessage) (string, error) {
	type anthropicMsg struct {
		Role    string `json:"role"`
		Content string `json:"content"`
	}
	type anthropicReq struct {
		Model     string         `json:"model"`
		MaxTokens int            `json:"max_tokens"`
		System    string         `json:"system"`
		Messages  []anthropicMsg `json:"messages"`
	}
	type anthropicContent struct {
		Type string `json:"type"`
		Text string `json:"text"`
	}
	type anthropicResp struct {
		Content []anthropicContent `json:"content"`
		Error   *struct {
			Message string `json:"message"`
		} `json:"error"`
	}

	anthropicMsgs := make([]anthropicMsg, len(msgs))
	for i, m := range msgs {
		anthropicMsgs[i] = anthropicMsg{Role: m.Role, Content: m.Content}
	}

	body, _ := json.Marshal(anthropicReq{
		Model:     "claude-sonnet-4-6",
		MaxTokens: 4096,
		System:    systemPrompt,
		Messages:  anthropicMsgs,
	})

	req, err := http.NewRequest("POST", "https://api.anthropic.com/v1/messages", bytes.NewReader(body))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("x-api-key", apiKey)
	req.Header.Set("anthropic-version", "2023-06-01")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()

	data, _ := io.ReadAll(resp.Body)
	var ar anthropicResp
	if err := json.Unmarshal(data, &ar); err != nil {
		return "", fmt.Errorf("invalid Anthropic response: %w", err)
	}
	if ar.Error != nil {
		return "", fmt.Errorf("Anthropic error: %s", ar.Error.Message)
	}
	for _, c := range ar.Content {
		if c.Type == "text" {
			return c.Text, nil
		}
	}
	return "", fmt.Errorf("no text content in Anthropic response")
}

func callOpenAI(apiKey, systemPrompt string, msgs []aiChatMessage) (string, error) {
	type openAIMsg struct {
		Role    string `json:"role"`
		Content string `json:"content"`
	}
	type openAIReq struct {
		Model    string      `json:"model"`
		Messages []openAIMsg `json:"messages"`
	}
	type openAIChoice struct {
		Message openAIMsg `json:"message"`
	}
	type openAIResp struct {
		Choices []openAIChoice `json:"choices"`
		Error   *struct {
			Message string `json:"message"`
		} `json:"error"`
	}

	openAIMsgs := []openAIMsg{{Role: "system", Content: systemPrompt}}
	for _, m := range msgs {
		openAIMsgs = append(openAIMsgs, openAIMsg{Role: m.Role, Content: m.Content})
	}

	body, _ := json.Marshal(openAIReq{
		Model:    "gpt-4o",
		Messages: openAIMsgs,
	})

	req, err := http.NewRequest("POST", "https://api.openai.com/v1/chat/completions", bytes.NewReader(body))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+apiKey)

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()

	data, _ := io.ReadAll(resp.Body)
	var or openAIResp
	if err := json.Unmarshal(data, &or); err != nil {
		return "", fmt.Errorf("invalid OpenAI response: %w", err)
	}
	if or.Error != nil {
		return "", fmt.Errorf("OpenAI error: %s", or.Error.Message)
	}
	if len(or.Choices) == 0 {
		return "", fmt.Errorf("no choices in OpenAI response")
	}
	return or.Choices[0].Message.Content, nil
}

// ─── Config helpers ───────────────────────────────────────────────────────────

// sanitizeHubConfig marshals to YAML and masks known secret fields.
func sanitizeHubConfig(cfg *types.HubConfig) (string, error) {
	data, err := yaml.Marshal(cfg)
	if err != nil {
		return "", err
	}
	var node yaml.Node
	if err := yaml.Unmarshal(data, &node); err != nil {
		return "", err
	}
	maskSensitiveYAMLValues(&node)
	sanitized, err := yaml.Marshal(&node)
	if err != nil {
		return "", err
	}
	return string(sanitized), nil
}

var backupTimestampSuffixRe = regexp.MustCompile(`^\d+$`)

var sensitiveYAMLFields = map[string]struct{}{
	"token":           {},
	"claw_token":      {},
	"relay_secret":    {},
	"ui_password":     {},
	"api_key":         {},
	"access_token":    {},
	"private_key_pem": {},
	"client_secret":   {},
	"webhook_secret":  {},
}

func maskSensitiveYAMLValues(node *yaml.Node) {
	switch node.Kind {
	case yaml.DocumentNode, yaml.SequenceNode:
		for _, child := range node.Content {
			maskSensitiveYAMLValues(child)
		}
	case yaml.MappingNode:
		for i := 0; i+1 < len(node.Content); i += 2 {
			keyNode := node.Content[i]
			valNode := node.Content[i+1]

			if _, ok := sensitiveYAMLFields[keyNode.Value]; ok {
				maskYAMLNodeValue(valNode)
				continue
			}
			if keyNode.Value == "secrets" && valNode.Kind == yaml.MappingNode {
				for j := 1; j < len(valNode.Content); j += 2 {
					maskYAMLNodeValue(valNode.Content[j])
				}
				continue
			}

			maskSensitiveYAMLValues(valNode)
		}
	}
}

func maskYAMLNodeValue(node *yaml.Node) {
	node.Kind = yaml.ScalarNode
	node.Style = 0
	node.Tag = "!!str"
	node.Value = "***"
	node.Anchor = ""
	node.Alias = nil
	node.Content = nil
}

// extractYAMLBlock extracts the content of the first ```yaml ... ``` block.
var yamlBlockRe = regexp.MustCompile("(?s)```yaml\n(.*?)```")

func extractYAMLBlock(s string) string {
	m := yamlBlockRe.FindStringSubmatch(s)
	if len(m) < 2 {
		return ""
	}
	return strings.TrimSpace(m[1])
}

// extractPlaceholders finds all __PLACEHOLDER__ patterns in the YAML.
var placeholderRe = regexp.MustCompile(`__([A-Z0-9_]+)__`)

func extractPlaceholders(s string) []string {
	matches := placeholderRe.FindAllStringSubmatch(s, -1)
	seen := map[string]bool{}
	var result []string
	for _, m := range matches {
		if !seen[m[1]] {
			seen[m[1]] = true
			result = append(result, m[1])
		}
	}
	return result
}

// substitutePlaceholders replaces __NAME__ with provided secret values.
func substitutePlaceholders(yamlStr string, secrets map[string]string) string {
	return placeholderRe.ReplaceAllStringFunc(yamlStr, func(match string) string {
		m := placeholderRe.FindStringSubmatch(match)
		if len(m) < 2 {
			return match
		}
		if val, ok := secrets[m[1]]; ok {
			return val
		}
		return match
	})
}

func restoreMaskedSecretsFromDisk(yamlStr string, diskCfg *types.HubConfig) (string, error) {
	if diskCfg == nil {
		return yamlStr, nil
	}

	var proposedNode yaml.Node
	if err := yaml.Unmarshal([]byte(yamlStr), &proposedNode); err != nil {
		return "", err
	}

	diskYAML, err := yaml.Marshal(diskCfg)
	if err != nil {
		return "", err
	}
	var diskNode yaml.Node
	if err := yaml.Unmarshal(diskYAML, &diskNode); err != nil {
		return "", err
	}

	mergeMaskedSecretsNode(&proposedNode, &diskNode)
	restored, err := yaml.Marshal(&proposedNode)
	if err != nil {
		return "", err
	}
	return string(restored), nil
}

func mergeMaskedSecretsNode(proposed, disk *yaml.Node) {
	if proposed == nil {
		return
	}
	switch proposed.Kind {
	case yaml.DocumentNode:
		if len(proposed.Content) == 0 {
			return
		}
		var diskRoot *yaml.Node
		if disk != nil && len(disk.Content) > 0 {
			diskRoot = disk.Content[0]
		}
		mergeMaskedSecretsNode(proposed.Content[0], diskRoot)
	case yaml.SequenceNode:
		for i := range proposed.Content {
			var diskChild *yaml.Node
			if disk != nil && disk.Kind == yaml.SequenceNode && i < len(disk.Content) {
				diskChild = disk.Content[i]
			}
			mergeMaskedSecretsNode(proposed.Content[i], diskChild)
		}
	case yaml.MappingNode:
		diskVals := map[string]*yaml.Node{}
		if disk != nil && disk.Kind == yaml.MappingNode {
			for i := 0; i+1 < len(disk.Content); i += 2 {
				diskVals[disk.Content[i].Value] = disk.Content[i+1]
			}
		}

		for i := 0; i+1 < len(proposed.Content); i += 2 {
			keyNode := proposed.Content[i]
			valNode := proposed.Content[i+1]
			diskVal := diskVals[keyNode.Value]

			if _, ok := sensitiveYAMLFields[keyNode.Value]; ok && isMaskedValue(valNode) && diskVal != nil {
				proposed.Content[i+1] = cloneYAMLNode(diskVal)
				continue
			}

			if keyNode.Value == "secrets" && valNode.Kind == yaml.MappingNode {
				mergeMaskedSecretMap(valNode, diskVal)
				continue
			}

			mergeMaskedSecretsNode(valNode, diskVal)
		}
	}
}

func mergeMaskedSecretMap(proposedSecrets, diskSecrets *yaml.Node) {
	if proposedSecrets == nil || proposedSecrets.Kind != yaml.MappingNode {
		return
	}
	if diskSecrets == nil || diskSecrets.Kind != yaml.MappingNode {
		return
	}

	diskVals := map[string]*yaml.Node{}
	for i := 0; i+1 < len(diskSecrets.Content); i += 2 {
		diskVals[diskSecrets.Content[i].Value] = diskSecrets.Content[i+1]
	}

	for i := 0; i+1 < len(proposedSecrets.Content); i += 2 {
		key := proposedSecrets.Content[i].Value
		val := proposedSecrets.Content[i+1]
		if isMaskedValue(val) {
			if diskVal := diskVals[key]; diskVal != nil {
				proposedSecrets.Content[i+1] = cloneYAMLNode(diskVal)
			}
		}
	}
}

func isMaskedValue(node *yaml.Node) bool {
	return node != nil && node.Kind == yaml.ScalarNode && node.Value == "***"
}

func cloneYAMLNode(n *yaml.Node) *yaml.Node {
	if n == nil {
		return nil
	}
	cloned := *n
	if len(n.Content) > 0 {
		cloned.Content = make([]*yaml.Node, len(n.Content))
		for i := range n.Content {
			cloned.Content[i] = cloneYAMLNode(n.Content[i])
		}
	}
	// Alias nodes are not expected in hub config YAML; keep nil to avoid cycles.
	cloned.Alias = nil
	return &cloned
}

func uniqueProviders(llmKeys types.LLMKeysList) []string {
	seen := map[string]struct{}{}
	var providers []string
	for _, k := range llmKeys {
		p := strings.TrimSpace(k.Provider)
		if p == "" {
			continue
		}
		if _, ok := seen[p]; ok {
			continue
		}
		seen[p] = struct{}{}
		providers = append(providers, p)
	}
	sort.Strings(providers)
	if len(providers) == 0 {
		return []string{"none"}
	}
	return providers
}

// validateHubConfig checks that the config has required fields.
func validateHubConfig(cfg *types.HubConfig) error {
	// Marshal/unmarshal roundtrip
	data, err := yaml.Marshal(cfg)
	if err != nil {
		return fmt.Errorf("marshal failed: %w", err)
	}
	var roundtrip types.HubConfig
	if err := yaml.Unmarshal(data, &roundtrip); err != nil {
		return fmt.Errorf("unmarshal roundtrip failed: %w", err)
	}

	if cfg.ClawToken == "" {
		return fmt.Errorf("claw_token is required")
	}
	if cfg.URL == "" {
		return fmt.Errorf("url is required")
	}
	for i, k := range cfg.LLMKeys {
		if k.Name == "" {
			return fmt.Errorf("llm_keys[%d]: name is required", i)
		}
		if k.Provider == "" {
			return fmt.Errorf("llm_keys[%d] (%s): provider is required", i, k.Name)
		}
	}
	return nil
}

// backupHubConfig copies hub.yaml to hub.yaml.bak.{timestamp} and returns the backup path.
func backupHubConfig() (string, error) {
	src, err := config.ActiveHubConfigPath()
	if err != nil {
		return "", err
	}
	data, err := os.ReadFile(src)
	if err != nil {
		return "", fmt.Errorf("read hub config: %w", err)
	}
	dst := fmt.Sprintf("%s.bak.%d", src, time.Now().Unix())
	if err := os.WriteFile(dst, data, 0600); err != nil {
		return "", fmt.Errorf("write backup: %w", err)
	}
	return dst, nil
}
