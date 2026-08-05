package hub

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/elasticclaw/elasticclaw/pkg/types"
)

const routineDraftSystemPrompt = `You draft scheduled ElasticClaw agent routines.
Return exactly one JSON object and no markdown or commentary:
{"name":"kebab-case-name","task":"clear agent instruction","schedule":"five-field cron","timezone":"IANA timezone","overlapPolicy":"skip or parallel","timeout":"Go duration"}

Rules:
- Infer a practical schedule from the request. If it is ambiguous, use 0 9 * * 1-5.
- Use the requested timezone; otherwise use the supplied browser timezone.
- The task must be self-contained, concrete, and mention verification or reporting when appropriate.
- Use "skip" unless concurrent runs are explicitly useful.
- Use a realistic timeout such as 30m, 1h, or 2h.
- Never include secrets, credentials, markdown, or extra JSON fields.`

var routineNamePattern = regexp.MustCompile(`^[a-z0-9][a-z0-9-]*$`)

var routineDraftCommandContext = exec.CommandContext

const (
	routineDraftCodexTimeout = 2 * time.Minute
	routineDraftMaxAuthFile  = 2 * 1024 * 1024
	routineDraftMaxAuthTotal = 10 * 1024 * 1024
	routineDraftMaxCLIOutput = 8 * 1024
)

type routineDraftRequest struct {
	Description string `json:"description"`
	Timezone    string `json:"timezone"`
}

type routineDraftResponse struct {
	Name          string `json:"name"`
	Task          string `json:"task"`
	Schedule      string `json:"schedule"`
	Timezone      string `json:"timezone"`
	OverlapPolicy string `json:"overlapPolicy"`
	Timeout       string `json:"timeout"`
}

func (s *Server) handleRoutineDraft(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	workspaceName := strings.TrimSpace(r.PathValue("workspace"))
	if workspaceName == "" {
		http.Error(w, "workspace name required", http.StatusBadRequest)
		return
	}
	workspace, err := loadExternalWorkspace(workspaceName)
	if err != nil {
		http.Error(w, "workspace not found", http.StatusNotFound)
		return
	}

	var req routineDraftRequest
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 32*1024)).Decode(&req); err != nil {
		http.Error(w, "invalid request", http.StatusBadRequest)
		return
	}
	req.Description = strings.TrimSpace(req.Description)
	if req.Description == "" {
		http.Error(w, "description required", http.StatusBadRequest)
		return
	}
	if len(req.Description) > 4000 {
		http.Error(w, "description must be 4000 characters or fewer", http.StatusBadRequest)
		return
	}
	req.Timezone = strings.TrimSpace(req.Timezone)
	if req.Timezone == "" {
		req.Timezone = "UTC"
	}
	if _, err := time.LoadLocation(req.Timezone); err != nil {
		req.Timezone = "UTC"
	}

	repositories := make([]string, 0, len(workspace.Repositories))
	for _, repository := range workspace.Repositories {
		if repository.Repo != "" {
			repositories = append(repositories, repository.Repo)
		}
	}
	userPrompt := fmt.Sprintf(
		"Workspace: %s\nRepositories: %s\nBrowser timezone: %s\n\nRoutine request:\n%s",
		workspace.Name,
		strings.Join(repositories, ", "),
		req.Timezone,
		req.Description,
	)

	s.mu.RLock()
	llmKeys := cloneLLMKeys(s.hubCfg.LLMKeys)
	defaultModel := s.hubCfg.DefaultModel
	modelAuthProfiles := cloneModelAuthProfiles(s.hubCfg.ModelAuthProfiles)
	s.mu.RUnlock()

	raw, err := callLLMForRoutineDraft(r.Context(), userPrompt, llmKeys, defaultModel, modelAuthProfiles)
	if err != nil {
		http.Error(w, "unable to draft routine: "+err.Error(), http.StatusBadGateway)
		return
	}
	draft, err := parseRoutineDraft(raw, req.Timezone)
	if err != nil {
		http.Error(w, "the configured model returned an invalid routine: "+err.Error(), http.StatusBadGateway)
		return
	}
	jsonOK(w, draft)
}

func callLLMForRoutineDraft(
	ctx context.Context,
	prompt string,
	llmKeys types.LLMKeysList,
	defaultModel string,
	modelAuthProfiles []*types.ModelAuthProfileConfig,
) (string, error) {
	choice, err := selectAIConfigProvider(llmKeys, defaultModel)
	if err != nil {
		return "", err
	}
	messages := []aiChatMessage{{Role: "user", Content: prompt}}
	if choice.Anthropic {
		return callAnthropicModel(ctx, choice.Key.APIKey, "claude-sonnet-4-6", routineDraftSystemPrompt, messages, 1200)
	}
	if choice.Key.APIKey == "" && choice.Key.Provider == "codex" && choice.Key.AuthProfile != "" {
		profile := findModelAuthProfile(modelAuthProfiles, choice.Key.Provider, choice.Key.AuthProfile)
		if profile == nil || profile.AuthState == "" {
			return "", fmt.Errorf("Codex auth profile %q is not configured", choice.Key.AuthProfile)
		}
		return callCodexCLIForRoutineDraft(ctx, prompt, choice.Model, profile.AuthState)
	}
	if choice.Key.APIKey == "" && choice.Key.Provider != "ollama" {
		return "", fmt.Errorf("the selected %s model requires an API key or supported CLI auth profile", choice.Key.Provider)
	}
	return callOpenAICompatible(ctx, choice.Provider, choice.Key.APIKey, choice.Model, routineDraftSystemPrompt, messages)
}

func cloneModelAuthProfiles(profiles []*types.ModelAuthProfileConfig) []*types.ModelAuthProfileConfig {
	cloned := make([]*types.ModelAuthProfileConfig, len(profiles))
	for i, profile := range profiles {
		if profile == nil {
			continue
		}
		copy := *profile
		cloned[i] = &copy
	}
	return cloned
}

func findModelAuthProfile(profiles []*types.ModelAuthProfileConfig, provider, name string) *types.ModelAuthProfileConfig {
	for _, profile := range profiles {
		if profile != nil && profile.Provider == provider && profile.Name == name {
			return profile
		}
	}
	return nil
}

func callCodexCLIForRoutineDraft(ctx context.Context, prompt, model, authState string) (string, error) {
	authRoot, err := os.MkdirTemp("", "elasticclaw-routine-draft-*")
	if err != nil {
		return "", fmt.Errorf("create temporary Codex home: %w", err)
	}
	defer os.RemoveAll(authRoot)
	if err := os.Chmod(authRoot, 0o700); err != nil {
		return "", fmt.Errorf("secure temporary Codex home: %w", err)
	}
	if err := restoreCLIAuthBundle(authRoot, authState); err != nil {
		return "", fmt.Errorf("restore Codex auth profile: %w", err)
	}

	schemaPath := filepath.Join(authRoot, "routine-draft.schema.json")
	if err := os.WriteFile(schemaPath, []byte(routineDraftJSONSchema), 0o600); err != nil {
		return "", fmt.Errorf("write routine draft schema: %w", err)
	}
	outputPath := filepath.Join(authRoot, "routine-draft.json")
	workspacePath := filepath.Join(authRoot, "workspace")
	if err := os.Mkdir(workspacePath, 0o700); err != nil {
		return "", fmt.Errorf("create temporary Codex workspace: %w", err)
	}

	runCtx, cancel := context.WithTimeout(ctx, routineDraftCodexTimeout)
	defer cancel()
	args := []string{
		"exec",
		"--ephemeral",
		"--ignore-user-config",
		"--ignore-rules",
		"--sandbox", "read-only",
		"--skip-git-repo-check",
		"--color", "never",
		"--output-schema", schemaPath,
		"--output-last-message", outputPath,
		"--cd", workspacePath,
	}
	if strings.TrimSpace(model) != "" {
		args = append(args, "--model", model)
	}
	args = append(args, "-")

	cmd := routineDraftCommandContext(runCtx, "codex", args...)
	cmd.Env = routineDraftCodexEnv(authRoot)
	cmd.Stdin = strings.NewReader(routineDraftSystemPrompt + "\n\n" + prompt)
	var diagnostic cappedBuffer
	diagnostic.max = routineDraftMaxCLIOutput
	cmd.Stdout = &diagnostic
	cmd.Stderr = &diagnostic
	if err := cmd.Run(); err != nil {
		if runCtx.Err() != nil {
			return "", fmt.Errorf("Codex routine draft timed out: %w", runCtx.Err())
		}
		detail := strings.TrimSpace(diagnostic.String())
		if detail == "" {
			return "", fmt.Errorf("run Codex routine draft: %w", err)
		}
		return "", fmt.Errorf("run Codex routine draft: %w: %s", err, detail)
	}

	raw, err := os.ReadFile(outputPath)
	if err != nil {
		return "", fmt.Errorf("read Codex routine draft: %w", err)
	}
	if strings.TrimSpace(string(raw)) == "" {
		return "", fmt.Errorf("Codex returned an empty routine draft")
	}
	return string(raw), nil
}

const routineDraftJSONSchema = `{
  "type": "object",
  "additionalProperties": false,
  "required": ["name", "task", "schedule", "timezone", "overlapPolicy", "timeout"],
  "properties": {
    "name": {"type": "string"},
    "task": {"type": "string"},
    "schedule": {"type": "string"},
    "timezone": {"type": "string"},
    "overlapPolicy": {"type": "string", "enum": ["skip", "parallel"]},
    "timeout": {"type": "string"}
  }
}`

func restoreCLIAuthBundle(root, authState string) error {
	outer, err := base64.StdEncoding.DecodeString(authState)
	if err != nil {
		return fmt.Errorf("decode auth state: %w", err)
	}
	if len(outer) > routineDraftMaxAuthTotal {
		return fmt.Errorf("auth bundle exceeds 10MiB")
	}
	var bundle cliAuthBundle
	if err := json.Unmarshal(outer, &bundle); err != nil {
		return fmt.Errorf("decode auth bundle: %w", err)
	}
	var total int
	for rel, encoded := range bundle.Files {
		clean := filepath.Clean(filepath.FromSlash(rel))
		if clean == "." || clean == ".." || filepath.IsAbs(clean) || strings.HasPrefix(clean, ".."+string(os.PathSeparator)) {
			return fmt.Errorf("auth bundle contains unsafe path %q", rel)
		}
		data, err := base64.StdEncoding.DecodeString(encoded)
		if err != nil {
			return fmt.Errorf("decode auth file %q: %w", rel, err)
		}
		if len(data) > routineDraftMaxAuthFile {
			return fmt.Errorf("auth file %q exceeds 2MiB", rel)
		}
		total += len(data)
		if total > routineDraftMaxAuthTotal {
			return fmt.Errorf("auth bundle exceeds 10MiB")
		}
		path := filepath.Join(root, clean)
		if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
			return fmt.Errorf("create auth directory for %q: %w", rel, err)
		}
		if err := os.WriteFile(path, data, 0o600); err != nil {
			return fmt.Errorf("restore auth file %q: %w", rel, err)
		}
	}
	return nil
}

func routineDraftCodexEnv(authRoot string) []string {
	env := make([]string, 0, len(os.Environ())+3)
	for _, entry := range os.Environ() {
		key, _, _ := strings.Cut(entry, "=")
		switch key {
		case "HOME", "CODEX_HOME", "OPENAI_API_KEY", "CODEX_API_KEY":
			continue
		default:
			env = append(env, entry)
		}
	}
	return append(env,
		"HOME="+authRoot,
		"CODEX_HOME="+filepath.Join(authRoot, ".codex"),
		"NO_COLOR=1",
	)
}

type cappedBuffer struct {
	buf bytes.Buffer
	max int
}

func (b *cappedBuffer) Write(p []byte) (int, error) {
	originalLen := len(p)
	if b.max <= 0 || b.buf.Len() >= b.max {
		return originalLen, nil
	}
	remaining := b.max - b.buf.Len()
	if len(p) > remaining {
		p = p[:remaining]
	}
	_, _ = b.buf.Write(p)
	return originalLen, nil
}

func (b *cappedBuffer) String() string {
	return b.buf.String()
}

var _ io.Writer = (*cappedBuffer)(nil)

func parseRoutineDraft(raw, fallbackTimezone string) (*routineDraftResponse, error) {
	raw = strings.TrimSpace(raw)
	if strings.HasPrefix(raw, "```") {
		raw = strings.TrimPrefix(raw, "```json")
		raw = strings.TrimPrefix(raw, "```")
		raw = strings.TrimSuffix(strings.TrimSpace(raw), "```")
	}
	start, end := strings.Index(raw, "{"), strings.LastIndex(raw, "}")
	if start < 0 || end < start {
		return nil, fmt.Errorf("response did not contain JSON")
	}

	var draft routineDraftResponse
	if err := json.Unmarshal([]byte(raw[start:end+1]), &draft); err != nil {
		return nil, fmt.Errorf("invalid JSON: %w", err)
	}
	draft.Name = strings.TrimSpace(draft.Name)
	draft.Task = strings.TrimSpace(draft.Task)
	draft.Schedule = strings.TrimSpace(draft.Schedule)
	draft.Timezone = strings.TrimSpace(draft.Timezone)
	draft.OverlapPolicy = strings.TrimSpace(draft.OverlapPolicy)
	draft.Timeout = strings.TrimSpace(draft.Timeout)

	if !routineNamePattern.MatchString(draft.Name) {
		return nil, fmt.Errorf("name must use lowercase letters, numbers, and hyphens")
	}
	if draft.Task == "" {
		return nil, fmt.Errorf("task is required")
	}
	if _, err := types.ParseCronSchedule(draft.Schedule); err != nil {
		return nil, fmt.Errorf("invalid cron schedule: %w", err)
	}
	if draft.Timezone == "" {
		draft.Timezone = fallbackTimezone
	}
	if _, err := time.LoadLocation(draft.Timezone); err != nil {
		return nil, fmt.Errorf("invalid timezone %q", draft.Timezone)
	}
	if draft.OverlapPolicy == "" {
		draft.OverlapPolicy = "skip"
	}
	if draft.OverlapPolicy != "skip" && draft.OverlapPolicy != "parallel" {
		return nil, fmt.Errorf("overlap policy must be skip or parallel")
	}
	if draft.Timeout == "" {
		draft.Timeout = "2h"
	}
	if _, err := time.ParseDuration(draft.Timeout); err != nil {
		return nil, fmt.Errorf("invalid timeout: %w", err)
	}
	return &draft, nil
}
