package hub

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"os"
	osexec "os/exec"
	"path/filepath"
	"strings"
	"text/template"
	"time"

	"github.com/elasticclaw/elasticclaw/pkg/hub/pipeline"
	"github.com/elasticclaw/elasticclaw/pkg/types"
)

// githubIssueDetails holds the fields we fetch for pipeline template rendering.
type githubIssueDetails struct {
	Identifier  string `json:"identifier"`
	Title       string `json:"title"`
	URL         string `json:"url"`
	Description string `json:"description"`
}

// fetchGitHubIssueDetails looks up an issue by owner/repo/number and returns
// the fields needed for pipeline template rendering.
func (s *Server) fetchGitHubIssueDetails(token, repo string, issueNumber int, baseURL string) (*githubIssueDetails, error) {
	if baseURL == "" {
		baseURL = "https://api.github.com"
	}
	url := fmt.Sprintf("%s/repos/%s/issues/%d", baseURL, repo, issueNumber)
	req, err := http.NewRequest("GET", url, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")

	resp, err := issueTrackerHTTPClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != 200 {
		return nil, fmt.Errorf("github API GET %s: %d %s", url, resp.StatusCode, string(body))
	}

	var result struct {
		Number int    `json:"number"`
		Title  string `json:"title"`
		Body   string `json:"body"`
		URL    string `json:"html_url"`
	}
	if err := json.Unmarshal(body, &result); err != nil {
		return nil, fmt.Errorf("parse github issue response: %w", err)
	}

	return &githubIssueDetails{
		Identifier:  fmt.Sprintf("#%d", result.Number),
		Title:       result.Title,
		URL:         result.URL,
		Description: result.Body,
	}, nil
}

// isRetryableGitHubError returns true for transient errors worth retrying:
// 5xx HTTP responses and network-level failures.
func isRetryableGitHubError(err error) bool {
	if err == nil {
		return false
	}
	msg := err.Error()
	// Parse and request-construction errors are permanent — don't retry.
	if strings.HasPrefix(msg, "parse github issue response") {
		return false
	}
	if !strings.Contains(msg, "github API GET") {
		// network-level error (no HTTP response at all)
		return true
	}
	for _, code := range []string{": 429 ", ": 500 ", ": 502 ", ": 503 ", ": 504 "} {
		if strings.Contains(msg, code) {
			return true
		}
	}
	return false
}

// fetchGitHubIssueDetailsWithRetry wraps fetchGitHubIssueDetails with exponential
// backoff for transient errors. It injects status messages into the claw so the
// user can see retries in the UI. On permanent failure it returns the last error
// without stopping or erroring the claw — the caller decides what to do.
func (s *Server) fetchGitHubIssueDetailsWithRetry(clawID, token, repo string, issueNumber int, baseURL string) (*githubIssueDetails, error) {
	const maxAttempts = 4
	backoff := time.Second
	var lastErr error
	for attempt := 1; attempt <= maxAttempts; attempt++ {
		details, err := s.fetchGitHubIssueDetails(token, repo, issueNumber, baseURL)
		if err == nil {
			return details, nil
		}
		lastErr = err
		if attempt == maxAttempts || !isRetryableGitHubError(err) {
			break
		}
		log.Printf("[pipeline] fetchGitHubIssueDetails attempt %d/%d failed for %s/%d: %v — retrying in %s", attempt, maxAttempts, repo, issueNumber, err, backoff)
		s.injectHubMessageByID(clawID, fmt.Sprintf("[hub] GitHub API temporarily unavailable — retry %d/%d in %s…", attempt, maxAttempts-1, backoff))
		time.Sleep(backoff)
		backoff *= 2
	}
	return nil, lastErr
}

// githubAPIAddLabel adds a label to a GitHub issue. Unlike
// githubAPIPostWithBase, this does not attempt to unmarshal the response body
// (POST /labels returns a JSON array of label objects, not a JSON object).
func githubAPIAddLabel(baseURL, repo string, issueNumber int, label, token string) error {
	if baseURL == "" {
		baseURL = "https://api.github.com"
	}
	path := fmt.Sprintf("repos/%s/issues/%d/labels", repo, issueNumber)
	body := map[string][]string{"labels": {label}}
	b, err := json.Marshal(body)
	if err != nil {
		return err
	}
	req, err := http.NewRequest("POST", baseURL+"/"+path, bytes.NewReader(b))
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")
	req.Header.Set("Content-Type", "application/json")
	resp, err := issueTrackerHTTPClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	respBody, _ := io.ReadAll(resp.Body)
	if resp.StatusCode >= 300 {
		return fmt.Errorf("github API POST %s: %d %s", path, resp.StatusCode, string(respBody))
	}
	return nil
}

// githubAPIDeleteLabel removes a label from a GitHub issue. Unlike
// githubAPIPostWithBase, this does not attempt to unmarshal the response body
// (DELETE returns an array of remaining labels, not a JSON object).
func githubAPIDeleteLabel(baseURL, repo string, issueNumber int, label, token string) error {
	if baseURL == "" {
		baseURL = "https://api.github.com"
	}
	path := fmt.Sprintf("repos/%s/issues/%d/labels/%s", repo, issueNumber, url.PathEscape(label))
	req, err := http.NewRequest("DELETE", baseURL+"/"+path, nil)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")
	resp, err := issueTrackerHTTPClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	respBody, _ := io.ReadAll(resp.Body)
	if resp.StatusCode == http.StatusNotFound && strings.Contains(string(respBody), "Label does not exist") {
		return nil
	}
	if resp.StatusCode >= 300 {
		return fmt.Errorf("github API DELETE %s: %d %s", path, resp.StatusCode, string(respBody))
	}
	return nil
}

// parsePipelineForFactory parses the PipelineYAML from a factory config.
// Returns nil (and logs a warning) if the YAML is empty or invalid.
func parsePipelineForFactory(factory *types.FactoryConfig) *pipeline.Pipeline {
	if factory == nil || factory.PipelineYAML == "" {
		return nil
	}
	p, err := pipeline.Parse([]byte(factory.PipelineYAML))
	if err != nil {
		log.Printf("[pipeline] factory %q: failed to parse pipeline_yaml: %v", factory.Name, err)
		log.Printf("[pipeline] factory %q: pipeline_yaml content:\n%s", factory.Name, factory.PipelineYAML)
		// NOTE: pipeline YAML may contain secrets in inject blocks. This log is
		// only emitted on parse failure (not routine operation) to aid debugging.
		// Audit your log aggregator retention policy if this is a concern.
		return nil
	}
	return p
}

type pipelineContext struct {
	Factory              *types.FactoryConfig
	Workspace            *types.WorkspaceConfig
	Workflow             *types.WorkflowConfig
	IssueID              string
	IssueLabels          []string
	IssueLabelsAvailable bool
}

func (ctx pipelineContext) Name() string {
	if ctx.Workflow != nil && ctx.Workspace != nil {
		return "workflow:" + ctx.Workspace.Name + "/" + ctx.Workflow.Name
	}
	if ctx.Factory != nil {
		return "factory:" + ctx.Factory.Name
	}
	return "pipeline"
}

func (ctx pipelineContext) Integration() string {
	if ctx.Workflow != nil {
		return ctx.Workflow.Integration
	}
	if ctx.Factory != nil {
		return ctx.Factory.Integration
	}
	return ""
}

func (ctx pipelineContext) TrackerName() string {
	if ctx.Workflow != nil {
		return ctx.Workflow.Workspace
	}
	if ctx.Factory != nil {
		return ctx.Factory.Workspace
	}
	return ""
}

func (ctx pipelineContext) PipelineYAML() string {
	if ctx.Workflow != nil {
		return ctx.Workflow.PipelineYAML
	}
	if ctx.Factory != nil {
		return ctx.Factory.PipelineYAML
	}
	return ""
}

func parsePipelineForContext(ctx pipelineContext) *pipeline.Pipeline {
	pipelineYAML := ctx.PipelineYAML()
	if pipelineYAML == "" {
		return nil
	}
	p, err := pipeline.Parse([]byte(pipelineYAML))
	if err != nil {
		log.Printf("[pipeline] %s: failed to parse pipeline yaml: %v", ctx.Name(), err)
		log.Printf("[pipeline] %s: pipeline yaml content:\n%s", ctx.Name(), pipelineYAML)
		return nil
	}
	return p
}

func (s *Server) warnPipelineRender(clawID, format string, args ...interface{}) {
	msg := fmt.Sprintf(format, args...)
	log.Printf("[pipeline] %s", msg)
	s.injectHubMessageByID(clawID, "[hub] Warning: "+msg)
}

func renderInjectWithData(clawID, injectMsg string, data interface{}) string {
	tmpl, err := template.New("inject").Parse(injectMsg)
	if err != nil {
		log.Printf("[pipeline] template PARSE FAILED for claw %s: %v", clawID[:8], err)
		return injectMsg
	}
	var buf bytes.Buffer
	if err := tmpl.Execute(&buf, data); err != nil {
		log.Printf("[pipeline] template EXECUTE FAILED for claw %s: %v", clawID[:8], err)
		return injectMsg
	}
	return buf.String()
}

// injectTemplateData wraps the given data with Outputs loaded from the DB so
// templates can reference {{ .Outputs.<name>.<key> }}.
func (s *Server) injectTemplateData(clawID string, baseData interface{}) interface{} {
	outputs := s.loadPipelineOutputs(clawID)
	if outputs == nil {
		return baseData
	}
	// Use a map so we can merge outputs with any base data shape.
	// Apply baseData first, then unconditionally set Outputs so pipeline
	// outputs can never be silently overwritten by caller data.
	result := make(map[string]interface{})
	if m, ok := baseData.(map[string]interface{}); ok {
		for k, v := range m {
			result[k] = v
		}
	} else {
		result["Data"] = baseData
	}
	result["Outputs"] = outputs
	return result
}

func fallbackGitHubIssueDetails(issueID string) *githubIssueDetails {
	details := &githubIssueDetails{Identifier: issueID}
	parts := strings.Split(issueID, "/")
	if len(parts) == 3 {
		details.Identifier = "#" + parts[2]
		details.URL = fmt.Sprintf("https://github.com/%s/%s/issues/%s", parts[0], parts[1], parts[2])
	}
	return details
}

func (s *Server) resolveLinearTokenForPipeline(ctx pipelineContext) string {
	if ctx.Workflow != nil && ctx.Workspace != nil {
		if tracker, ok := findWorkspaceIssueTracker(ctx.Workspace.Name, "linear", ctx.Workflow.Workspace); ok {
			return tracker.Token
		}
		return ""
	}
	if ctx.Factory != nil {
		return s.resolveLinearTokenForFactory(ctx.Factory)
	}
	return ""
}

func (s *Server) resolveShortcutTokenForPipeline(ctx pipelineContext) string {
	if ctx.Workflow != nil && ctx.Workspace != nil {
		if tracker, ok := findWorkspaceIssueTracker(ctx.Workspace.Name, "shortcut", ctx.Workflow.Workspace); ok {
			return tracker.Token
		}
		return ""
	}
	if ctx.Factory != nil {
		return s.resolveShortcutToken(ctx.Factory.Workspace)
	}
	return ""
}

func (s *Server) resolveGitHubIssuesTokenForPipeline(ctx pipelineContext) string {
	if ctx.Workflow != nil && ctx.Workspace != nil {
		if tracker, ok := findWorkspaceIssueTracker(ctx.Workspace.Name, "github-issues", ctx.Workflow.Workspace); ok {
			return tracker.Token
		}
		return ""
	}
	if ctx.Factory != nil {
		return s.resolveGitHubIssuesTokenForFactory(ctx.Factory)
	}
	return ""
}

func (s *Server) resolveJiraTrackerForPipeline(ctx pipelineContext) (workspaceIssueTracker, bool) {
	if ctx.Workflow != nil && ctx.Workspace != nil {
		return findWorkspaceIssueTracker(ctx.Workspace.Name, "jira", ctx.Workflow.Workspace)
	}
	if ctx.Factory != nil {
		return s.resolveJiraTrackerForFactory(ctx.Factory)
	}
	return workspaceIssueTracker{}, false
}

const defaultPipelineRunTimeout = 10 * time.Minute

type pipelineRunResult struct {
	ExitCode int
	Stdout   string
	Stderr   string
}

func (s *Server) executePipelineRunAction(clawID string, action pipeline.RunAction) (*pipelineRunResult, error) {
	command := strings.TrimSpace(action.Command)
	if command == "" {
		return nil, nil
	}
	if err := validateScriptCommand(command); err != nil {
		return nil, err
	}
	timeout := defaultPipelineRunTimeout
	if strings.TrimSpace(action.Timeout) != "" {
		parsed, err := time.ParseDuration(strings.TrimSpace(action.Timeout))
		if err != nil {
			return nil, fmt.Errorf("invalid run timeout %q: %w", action.Timeout, err)
		}
		timeout = parsed
	}
	return s.executePipelineCommand(clawID, command, timeout)
}

func (s *Server) executePipelineCommand(clawID, command string, timeout time.Duration) (*pipelineRunResult, error) {
	var providerName, providerID, sshHost, sshUser string
	var sshPort int
	if err := s.db.QueryRow(`
		SELECT COALESCE(provider,''), COALESCE(provider_id,''), COALESCE(ssh_host,''), COALESCE(ssh_port,0), COALESCE(ssh_user,'')
		FROM claws WHERE id=?
	`, clawID).Scan(&providerName, &providerID, &sshHost, &sshPort, &sshUser); err != nil {
		return nil, fmt.Errorf("load agent provider: %w", err)
	}
	if providerID == "" {
		return nil, fmt.Errorf("agent has no provider instance yet")
	}

	workspaceCommand, err := s.buildWorkspaceRunCommand(clawID, command)
	if err != nil {
		return nil, fmt.Errorf("build workspace run command: %w", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), timeout+30*time.Second)
	defer cancel()

	s.mu.RLock()
	provCfg, ok := s.hubCfg.Providers[providerName]
	s.mu.RUnlock()
	if !ok && providerName != "noop" {
		return nil, fmt.Errorf("provider %q is not configured", providerName)
	}

	switch providerName {
	case "daytona":
		p, err := newDaytonaProvider(provCfg)
		if err != nil {
			return nil, fmt.Errorf("daytona init: %w", err)
		}
		result, err := p.ExecWithTimeout(ctx, providerID, []string{workspaceCommand}, timeout)
		if result == nil {
			return nil, err
		}
		return &pipelineRunResult{ExitCode: result.ExitCode, Stdout: result.Stdout, Stderr: result.Stderr}, err
	case "exedev":
		p, err := newExedevProvider(provCfg)
		if err != nil {
			return nil, fmt.Errorf("exedev init: %w", err)
		}
		result, err := p.Exec(ctx, providerID, []string{"bash", "-lc", workspaceCommand})
		if result == nil {
			return nil, err
		}
		return &pipelineRunResult{ExitCode: result.ExitCode, Stdout: result.Stdout, Stderr: result.Stderr}, err
	case "lambda-microvms":
		p, err := newLambdaMicroVMsProvider(provCfg)
		if err != nil {
			return nil, fmt.Errorf("lambda microvms init: %w", err)
		}
		result, err := p.Exec(ctx, providerID, []string{"bash", "-lc", workspaceCommand})
		if result == nil {
			return nil, err
		}
		return &pipelineRunResult{ExitCode: result.ExitCode, Stdout: result.Stdout, Stderr: result.Stderr}, err
	case "docker":
		p, err := newDockerProvider(provCfg)
		if err != nil {
			return nil, fmt.Errorf("docker init: %w", err)
		}
		result, err := p.Exec(ctx, providerID, []string{"bash", "-lc", workspaceCommand})
		if result == nil {
			return nil, err
		}
		return &pipelineRunResult{ExitCode: result.ExitCode, Stdout: result.Stdout, Stderr: result.Stderr}, err
	case "replicated":
		if sshHost == "" || sshPort == 0 || sshUser == "" {
			return nil, fmt.Errorf("replicated agent has no SSH connection details")
		}
		return s.executeReplicatedPipelineRun(sshUser, fmt.Sprintf("%s:%d", sshHost, sshPort), workspaceCommand, timeout)
	case "noop":
		return &pipelineRunResult{ExitCode: 0, Stdout: "noop provider skipped workflow command"}, nil
	case "failing":
		// Test-only provider that always fails workflow run actions
		return &pipelineRunResult{ExitCode: 1, Stderr: "failing provider simulated run failure"}, fmt.Errorf("failing provider simulated run failure")
	case "testexec":
		// Test-only provider used by integration tests to exercise real run
		// commands without provisioning a sandbox.
		if os.Getenv("ELASTICCLAW_TESTEXEC_PROVIDER") == "" {
			return &pipelineRunResult{ExitCode: 1, Stderr: "testexec provider requires ELASTICCLAW_TESTEXEC_PROVIDER=1"}, fmt.Errorf("testexec provider disabled")
		}
		return executeLocalPipelineRun(ctx, workspaceCommand)
	default:
		return nil, fmt.Errorf("provider %q does not support workflow run actions", providerName)
	}
}

func executeLocalPipelineRun(ctx context.Context, command string) (*pipelineRunResult, error) {
	cmd := osexec.CommandContext(ctx, "bash", "-lc", command)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	err := cmd.Run()
	exitCode := 0
	if err != nil {
		exitCode = 1
		var exitErr *osexec.ExitError
		if errors.As(err, &exitErr) {
			exitCode = exitErr.ExitCode()
		}
	}
	return &pipelineRunResult{ExitCode: exitCode, Stdout: stdout.String(), Stderr: stderr.String()}, err
}

func (s *Server) executeReplicatedPipelineRun(user, host, command string, timeout time.Duration) (*pipelineRunResult, error) {
	output, err := s.sshRunWithTimeout(user, host, command, timeout)
	if err != nil {
		return &pipelineRunResult{ExitCode: 1, Stderr: err.Error(), Stdout: output}, err
	}
	return &pipelineRunResult{ExitCode: 0, Stdout: output}, nil
}

// persistPipelineOutput stores a run result in the DB so later stages can
// reference it via {{ .Outputs.<name>.<key> }} and it survives hub restarts.
func (s *Server) persistPipelineOutput(clawID, stageID, outputName string, result *pipelineRunResult) {
	if result == nil || outputName == "" {
		return
	}
	var parsedJSON string
	if parsed, ok := parsePipelineOutputJSON(result.Stdout); ok {
		b, _ := json.Marshal(parsed)
		parsedJSON = string(b)
	}
	if parsedJSON == "" {
		parsedJSON = "{}"
	}
	_, err := s.db.Exec(`
		INSERT INTO pipeline_outputs(claw_id, stage_id, output_name, exit_code, stdout, stderr, parsed_json, created_at)
		VALUES(?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(claw_id, output_name) DO UPDATE SET
			stage_id=excluded.stage_id,
			exit_code=excluded.exit_code,
			stdout=excluded.stdout,
			stderr=excluded.stderr,
			parsed_json=excluded.parsed_json,
			created_at=excluded.created_at`,
		clawID, stageID, outputName, result.ExitCode, result.Stdout, result.Stderr, parsedJSON, now())
	if err != nil {
		log.Printf("[pipeline] failed to persist output %q for claw %s: %v", outputName, clawID[:8], err)
	} else {
		log.Printf("[pipeline] persisted output %q for claw %s stage %s exit=%d", outputName, clawID[:8], stageID, result.ExitCode)
	}
}

func parsePipelineOutputJSON(stdout string) (map[string]interface{}, bool) {
	parse := func(s string) (map[string]interface{}, bool) {
		s = strings.TrimSpace(s)
		if s == "" {
			return nil, false
		}
		var parsed map[string]interface{}
		if err := json.Unmarshal([]byte(s), &parsed); err != nil {
			return nil, false
		}
		return parsed, true
	}
	if parsed, ok := parse(stdout); ok {
		return parsed, true
	}
	lines := strings.Split(stdout, "\n")
	for i := len(lines) - 1; i >= 0; i-- {
		if parsed, ok := parse(lines[i]); ok {
			return parsed, true
		}
	}
	return nil, false
}

// loadPipelineOutputs returns all persisted outputs for a claw as a map of
// output_name → parsed_json map. Used for template rendering.
func (s *Server) loadPipelineOutputs(clawID string) map[string]map[string]interface{} {
	rows, err := s.db.Query(`SELECT output_name, parsed_json FROM pipeline_outputs WHERE claw_id=?`, clawID)
	if err != nil {
		log.Printf("[pipeline] failed to load outputs for claw %s: %v", clawID[:8], err)
		return nil
	}
	defer rows.Close()
	outputs := make(map[string]map[string]interface{})
	for rows.Next() {
		var name, jsonStr string
		if err := rows.Scan(&name, &jsonStr); err != nil {
			continue
		}
		var parsed map[string]interface{}
		if err := json.Unmarshal([]byte(jsonStr), &parsed); err == nil {
			outputs[name] = parsed
		}
	}
	if err := rows.Err(); err != nil {
		log.Printf("[pipeline] rows error loading outputs for claw %s: %v", clawID[:8], err)
	}
	return outputs
}

// JudgeFinding is a single structured finding from a judge review.
type JudgeFinding struct {
	File     string `json:"file,omitempty"`
	Line     string `json:"line,omitempty"`
	Comment  string `json:"comment"`
	Severity string `json:"severity,omitempty"`
}

// JudgeResult is the structured output of a judge stage.
type JudgeResult struct {
	Verdict       string         `json:"verdict"`
	Summary       string         `json:"summary"`
	Findings      []JudgeFinding `json:"findings,omitempty"`
	RequiredFixes []string       `json:"required_fixes,omitempty"`
	RawJSON       string         `json:"-"`
}

// executeJudgeAction runs a model-backed review with constrained inputs.
func (s *Server) executeJudgeAction(clawID string, action pipeline.JudgeAction, ctx pipelineContext) (*JudgeResult, error) {
	// Build system prompt with instructions and output schema
	systemPrompt := action.Instructions + `

You must respond with a single JSON object in this exact schema:
{
  "verdict": "pass" or "fail",
  "summary": "brief summary of the review",
  "findings": [
    {"file": "path", "line": "1-10", "comment": "description", "severity": "low|medium|high|critical"}
  ],
  "required_fixes": ["fix 1", "fix 2"]
}
Do not include any markdown formatting, code fences, or explanatory text outside the JSON object.`

	// Collect bounded inputs
	var userContent strings.Builder
	for _, input := range action.Inputs {
		switch input {
		case pipeline.JudgeInputIssue:
			issueText := s.loadIssueTextForJudge(clawID, ctx)
			if issueText != "" {
				userContent.WriteString("## Issue Context\n\n")
				userContent.WriteString(issueText)
				userContent.WriteString("\n\n")
			}
		case pipeline.JudgeInputGitDiff:
			diff, err := s.executePipelineRunAction(clawID, pipeline.RunAction{
				Command: `cd "$HOME/.openclaw/workspace" && git diff --stat && echo "---" && git diff`,
				Timeout: "30s",
			})
			if err == nil && diff != nil {
				userContent.WriteString("## Git Diff\n\n```diff\n")
				userContent.WriteString(strings.TrimSpace(diff.Stdout))
				userContent.WriteString("\n```\n\n")
			}
		case pipeline.JudgeInputTestOutput:
			// Look for prior stage output named "test_output" or similar
			outputs := s.loadPipelineOutputs(clawID)
			if testOut, ok := outputs["test_output"]; ok {
				if stdout, ok := testOut["stdout"].(string); ok && stdout != "" {
					userContent.WriteString("## Test Output\n\n```\n")
					userContent.WriteString(stdout)
					userContent.WriteString("\n```\n\n")
				}
			} else if testOut, ok := outputs["tests"]; ok {
				if stdout, ok := testOut["stdout"].(string); ok && stdout != "" {
					userContent.WriteString("## Test Output\n\n```\n")
					userContent.WriteString(stdout)
					userContent.WriteString("\n```\n\n")
				}
			}
		case pipeline.JudgeInputFiles:
			// List changed files
			filesResult, err := s.executePipelineRunAction(clawID, pipeline.RunAction{
				Command: `cd "$HOME/.openclaw/workspace" && git diff --name-only`,
				Timeout: "10s",
			})
			if err == nil && filesResult != nil {
				userContent.WriteString("## Changed Files\n\n")
				userContent.WriteString(filesResult.Stdout)
				userContent.WriteString("\n\n")
			}
		}
	}

	if userContent.Len() == 0 {
		return nil, fmt.Errorf("no judge inputs could be collected")
	}

	// Call LLM
	model := action.Model
	if model == "" {
		model = s.hubCfg.DefaultModel
	}
	llmKeys := s.hubCfg.LLMKeys
	if len(llmKeys) == 0 {
		return nil, fmt.Errorf("no LLM keys configured for judge")
	}

	msgs := []aiChatMessage{{Role: "user", Content: userContent.String()}}
	ctx2, cancel := context.WithTimeout(context.Background(), judgeTimeout(action.Timeout))
	defer cancel()

	var rawResponse string
	err := streamLLMWithSystemPrompt(ctx2, systemPrompt, msgs, llmKeys, model, func(token string) {
		rawResponse += token
	})
	if err != nil {
		return nil, fmt.Errorf("judge LLM call failed: %w", err)
	}

	// Parse structured JSON response
	result, err := parseJudgeResponse(rawResponse)
	if err != nil {
		return nil, fmt.Errorf("judge response parse failed: %w (raw: %s)", err, truncateString(rawResponse, 500))
	}
	result.RawJSON = rawResponse
	return result, nil
}

func judgeTimeout(timeoutStr string) time.Duration {
	if timeoutStr != "" {
		if d, err := time.ParseDuration(timeoutStr); err == nil {
			return d
		}
	}
	return 2 * time.Minute
}

func parseJudgeResponse(raw string) (*JudgeResult, error) {
	// Extract JSON from possible markdown fences by finding the first '{' and
	// then matching the corresponding closing '}' using brace counting.
	jsonStr := raw
	if idx := strings.Index(raw, "{"); idx >= 0 {
		jsonStr = raw[idx:]
	}
	// Find the matching closing brace, not just the last one
	depth := 0
	endIdx := -1
	inString := false
	escapeNext := false
	for i, ch := range jsonStr {
		if escapeNext {
			escapeNext = false
			continue
		}
		if ch == '\\' && inString {
			escapeNext = true
			continue
		}
		if ch == '"' {
			inString = !inString
			continue
		}
		if inString {
			continue
		}
		if ch == '{' {
			depth++
		} else if ch == '}' {
			depth--
			if depth == 0 {
				endIdx = i
				break
			}
		}
	}
	if endIdx >= 0 {
		jsonStr = jsonStr[:endIdx+1]
	}
	jsonStr = strings.TrimSpace(jsonStr)

	var result JudgeResult
	if err := json.Unmarshal([]byte(jsonStr), &result); err != nil {
		return nil, err
	}
	if result.Verdict == "" {
		return nil, fmt.Errorf("missing verdict field")
	}
	// Normalize verdict
	result.Verdict = strings.ToLower(result.Verdict)
	if result.Verdict != "pass" && result.Verdict != "fail" {
		return nil, fmt.Errorf("invalid verdict: %q", result.Verdict)
	}
	return &result, nil
}

func (s *Server) loadIssueTextForJudge(clawID string, ctx pipelineContext) string {
	// Try to get issue details from the pipeline context
	issueID := ctx.IssueID
	if issueID == "" {
		return ""
	}
	if strings.HasPrefix(issueID, "sc-") {
		return "Shortcut story: " + issueID
	}
	if strings.Contains(issueID, "/") {
		// GitHub issue
		parts := strings.Split(issueID, "/")
		if len(parts) == 3 {
			ghToken := s.resolveGitHubIssuesTokenForPipeline(ctx)
			if ghToken != "" {
				repo := parts[0] + "/" + parts[1]
				var issueNum int
				if _, err := fmt.Sscanf(parts[2], "%d", &issueNum); err == nil {
					base := s.githubBaseURL
					if base == "" {
						base = "https://api.github.com"
					}
					details, err := s.fetchGitHubIssueDetails(ghToken, repo, issueNum, base)
					if err == nil && details != nil {
						return fmt.Sprintf("**%s**\n%s\n\n%s", details.Identifier, details.Title, details.Description)
					}
				}
			}
		}
		return "GitHub issue: " + issueID
	}
	// Linear issue
	linearToken := s.resolveLinearTokenForPipeline(ctx)
	if linearToken != "" {
		details, err := s.fetchLinearIssueDetails(linearToken, issueID)
		if err == nil && details != nil {
			return fmt.Sprintf("**%s: %s**\n%s\n\n%s", details.Identifier, details.Title, details.URL, details.Description)
		}
	}
	return "Linear issue: " + issueID
}

func truncateString(s string, maxLen int) string {
	if len(s) <= maxLen {
		return s
	}
	return s[:maxLen] + "..."
}

// validateScriptCommand checks that a workflow run command doesn't attempt path
// traversal outside the workspace scripts directory.
func validateScriptCommand(command string) error {
	command = strings.TrimSpace(command)
	if command == "" {
		return nil
	}
	// Reject any path traversal sequence — ".." in any path component could
	// escape the workspace. We tokenize the command and check each token that is
	// not a flag or a flag value.
	parts := strings.Fields(command)
	// Known flags that take a value — skip the next token after these.
	valueFlags := map[string]bool{"-m": true, "--module": true, "-f": true, "--file": true}
	skipNext := false
	for _, part := range parts {
		if skipNext {
			skipNext = false
			// Still reject traversal sequences even in flag values
			if strings.Contains(part, "..") {
				return fmt.Errorf("script command contains path traversal: %q", command)
			}
			continue
		}
		// Skip standalone flags (e.g. -f, --verbose), but still check inline values
		// like --output=../../.ssh/id_rsa for traversal sequences.
		if strings.HasPrefix(part, "-") {
			// Reject traversal even inside inline flag values (--output=../../evil)
			if strings.Contains(part, "..") {
				return fmt.Errorf("script command contains path traversal: %q", command)
			}
			if valueFlags[part] {
				skipNext = true
			}
			continue
		}
		// Reject any token containing ".." — this covers scripts/../..,
		// ../../.ssh/id_rsa, /etc/passwd/../shadow, etc.
		if strings.Contains(part, "..") {
			return fmt.Errorf("script command contains path traversal: %q", command)
		}
		// Reject absolute paths that clean to a different path (traversal via symlinks)
		if strings.HasPrefix(part, "/") {
			clean := filepath.Clean(part)
			if !strings.HasPrefix(clean, "/") {
				return fmt.Errorf("script command contains absolute path traversal: %q", command)
			}
		}
	}
	return nil
}

func formatPipelineRunFailure(action pipeline.RunAction, result *pipelineRunResult, err error) string {
	command := strings.TrimSpace(action.Command)
	if command == "" {
		command = "(empty command)"
	}
	details := ""
	if result != nil {
		details = strings.TrimSpace(result.Stdout + "\n" + result.Stderr)
	}
	if details == "" && err != nil {
		details = err.Error()
	}
	if details == "" {
		details = "no command output"
	}
	return fmt.Sprintf("Workflow command failed: `%s`\n\n%s", command, sanitizeBootstrapOutput(details))
}

// routedRequiredGateError means a required gate prevented subsequent on_enter
// actions, but its verdict has a declared gate_result route that will handle it.
// It must not terminate the source stage before that route can transition.
type routedRequiredGateError struct {
	stageID string
	verdict string
}

func (e *routedRequiredGateError) Error() string {
	return fmt.Sprintf("required gate %q %s", e.stageID, e.verdict)
}

// runOnEnter executes the on_enter actions for a given stage.
//
// - stage.OnEnter.Run: executes a command in the agent workspace
// - stage.OnEnter.Inject: injects a user message into the claw
// - stage.OnEnter.MoveIssue: moves the Linear/Shortcut issue to the named status
// - stage.OnEnter.CommentIssue: posts a rendered comment to the tracker issue
//
// issueID is the default issue from the trigger; it can be overridden by
// MoveIssue.IssueID or CommentIssue.IssueID (including template references
// like {{.Inputs.xxx}}).
func (s *Server) runOnEnter(clawID string, stage pipeline.Stage, ctx pipelineContext) (injectDelivered bool, err error) {
	issueID := ctx.IssueID
	// Captured for PR registration after any required gate has been evaluated.
	// Scanning immediately after the run would arm claw_prs before a failing
	// required gate could block the stage (and leave the watcher able to kill
	// the claw on merge/close while the workflow is still blocked).
	var runStdoutForPRScan string
	if strings.TrimSpace(stage.OnEnter.Run.Command) != "" {
		startMsg := fmt.Sprintf("[hub] ▶ Running workflow command for stage %q", stage.ID)
		if stage.OnEnter.Run.Timeout != "" {
			startMsg += fmt.Sprintf(" (timeout %s)", stage.OnEnter.Run.Timeout)
		}
		s.publishHubNotice(clawID, startMsg)
		log.Printf("[pipeline] running workflow command for claw %s stage %q: %s", clawID[:8], stage.ID, stage.OnEnter.Run.Command)
		result, err := s.executePipelineRunAction(clawID, stage.OnEnter.Run)
		// Persist output so later stages can reference it via {{ .Outputs.<name>.<key> }}
		if stage.OnEnter.Run.Output != "" && result != nil {
			s.persistPipelineOutput(clawID, stage.ID, stage.OnEnter.Run.Output, result)
		}
		if result != nil {
			runStdoutForPRScan = result.Stdout
		}
		if err != nil || (result != nil && result.ExitCode != 0) {
			msg := formatPipelineRunFailure(stage.OnEnter.Run, result, err)
			// If this stage has a gate, treat nonzero exit as a normal validation
			// failure so the gate can still evaluate the captured JSON output.
			if stage.Gate != nil && stage.OnEnter.Run.Output != "" && result != nil {
				log.Printf("[pipeline] %s; continuing because stage has a gate configured", msg)
				s.publishHubNotice(clawID, "[hub] Warning: "+msg)
			} else if stage.OnEnter.Run.ContinueOnError {
				log.Printf("[pipeline] %s; continuing because continue_on_error=true", msg)
				s.injectHubMessageByID(clawID, "[hub] Warning: "+msg)
			} else {
				log.Printf("[pipeline] %s", msg)
				s.injectHubMessageByID(clawID, "[hub] Error: "+msg)
				return false, fmt.Errorf("run action failed: %s", msg)
			}
		} else {
			log.Printf("[pipeline] workflow command completed for claw %s stage %q", clawID[:8], stage.ID)
			if result != nil {
				if result.Stdout != "" {
					log.Printf("[pipeline] workflow command stdout for claw %s:\n%s", clawID[:8], result.Stdout)
				}
				if result.Stderr != "" {
					log.Printf("[pipeline] workflow command stderr for claw %s:\n%s", clawID[:8], result.Stderr)
				}
				s.injectHubMessageByID(clawID, fmt.Sprintf("[hub] Workflow command completed for stage %q (exit %d)", stage.ID, result.ExitCode))
			}
		}
	}

	if dependencyUpdatesConfigured(stage.OnEnter.DependencyUpdates) {
		outputName := dependencyUpdatesOutputName(stage.OnEnter.DependencyUpdates)
		startMsg := fmt.Sprintf("[hub] ▶ Running dependency updates for stage %q", stage.ID)
		if len(stage.OnEnter.DependencyUpdates.Ecosystems) > 0 {
			startMsg += fmt.Sprintf(" (ecosystems: %s)", strings.Join(stage.OnEnter.DependencyUpdates.Ecosystems, ", "))
		}
		s.publishHubNotice(clawID, startMsg)
		log.Printf("[pipeline] running dependency updates for claw %s stage %q output %q", clawID[:8], stage.ID, outputName)
		result, err := s.executeDependencyUpdatesAction(clawID, stage.ID, stage.OnEnter.DependencyUpdates)
		if err != nil || (result != nil && result.ExitCode != 0) {
			msg := formatDependencyUpdateFailure(result)
			if err != nil && !strings.Contains(msg, err.Error()) {
				msg += ": " + err.Error()
			}
			// Log the full output so operators can debug even when the chat message is truncated.
			if result != nil {
				if result.Stdout != "" {
					log.Printf("[pipeline] dependency update stdout for claw %s:\n%s", clawID[:8], result.Stdout)
				}
				if result.Stderr != "" {
					log.Printf("[pipeline] dependency update stderr for claw %s:\n%s", clawID[:8], result.Stderr)
				}
			}
			if stage.OnEnter.DependencyUpdates.ContinueOnError {
				log.Printf("[pipeline] %s; continuing because continue_on_error=true", msg)
				s.injectHubMessageByID(clawID, "[hub] Warning: "+msg)
			} else {
				log.Printf("[pipeline] %s", msg)
				s.injectHubMessageByID(clawID, "[hub] Error: "+msg)
				return false, errors.New(msg)
			}
		} else {
			log.Printf("[pipeline] dependency updates completed for claw %s stage %q", clawID[:8], stage.ID)
			if result != nil {
				if result.Stdout != "" {
					log.Printf("[pipeline] dependency update stdout for claw %s:\n%s", clawID[:8], result.Stdout)
				}
				if result.Stderr != "" {
					log.Printf("[pipeline] dependency update stderr for claw %s:\n%s", clawID[:8], result.Stderr)
				}
				s.injectHubMessageByID(clawID, "[hub] "+formatDependencyUpdateSummary(result))
			}
		}
	}

	// Execute judge action if configured
	if stage.OnEnter.Judge.Instructions != "" {
		s.publishHubNotice(clawID, fmt.Sprintf("[hub] ▶ Running judge for stage %q", stage.ID))
		log.Printf("[pipeline] running judge for claw %s stage %q", clawID[:8], stage.ID)
		judgeResult, err := s.executeJudgeAction(clawID, stage.OnEnter.Judge, ctx)
		if err != nil {
			msg := fmt.Sprintf("Judge stage failed: %v", err)
			log.Printf("[pipeline] %s", msg)
			s.injectHubMessageByID(clawID, "[hub] Error: "+msg)
			if !stage.OnEnter.Judge.ContinueOnError {
				return false, fmt.Errorf("judge action failed: %s", msg)
			}
		} else {
			log.Printf("[pipeline] judge completed for claw %s stage %q: verdict=%s", clawID[:8], stage.ID, judgeResult.Verdict)
			// Persist judge output so later stages can reference it
			if stage.OnEnter.Judge.Output != "" {
				result := &pipelineRunResult{
					ExitCode: 0,
					Stdout:   judgeResult.RawJSON,
					Stderr:   "",
				}
				s.persistPipelineOutput(clawID, stage.ID, stage.OnEnter.Judge.Output, result)
			}
			// Inject judge result into claw chat
			if judgeResult.Verdict == "pass" {
				s.injectHubMessageByID(clawID, fmt.Sprintf("[hub] Review passed: %s", judgeResult.Summary))
			} else {
				findingsMsg := fmt.Sprintf("[hub] Review failed: %s\n\nRequired fixes:\n", judgeResult.Summary)
				for _, fix := range judgeResult.RequiredFixes {
					findingsMsg += fmt.Sprintf("- %s\n", fix)
				}
				if len(judgeResult.Findings) > 0 {
					findingsMsg += "\nFindings:\n"
					for _, f := range judgeResult.Findings {
						findingsMsg += fmt.Sprintf("- %s (%s): %s\n", f.File, f.Severity, f.Comment)
					}
				}
				s.injectHubMessageByID(clawID, findingsMsg)
			}
			// Auto-transition to next stage if a judge_verdict trigger matches
			s.safeGo("pipeline judge auto-transition", func() { s.autoTransitionAfterJudge(clawID, judgeResult.Verdict, ctx) })
			// Check required verdict
			if stage.OnEnter.Judge.Require.Verdict != "" &&
				!strings.EqualFold(judgeResult.Verdict, stage.OnEnter.Judge.Require.Verdict) {
				msg := fmt.Sprintf("Judge verdict %q does not match required %q", judgeResult.Verdict, stage.OnEnter.Judge.Require.Verdict)
				log.Printf("[pipeline] %s", msg)
				if !stage.OnEnter.Judge.ContinueOnError {
					s.injectHubMessageByID(clawID, "[hub] Error: "+msg)
					return false, fmt.Errorf("judge requirement failed: %s", msg)
				}
			}
		}
	}

	// Evaluate gate if configured
	if stage.Gate != nil {
		// Re-entering *this* plan_gate stage after it already passed must not
		// re-run validation forever. Scoped per stageID so a later plan_gate
		// in the same workflow still evaluates its own output (Greptile).
		if stage.PlanGate && s.hasSystemMarker(clawID, planGateAcceptedMarker(stage.ID)) {
			alreadyRouted := false
			if pl := parsePipelineForContext(ctx); pl != nil {
				alreadyRouted = pl.StageForGateResult(stage.ID, "pass") != nil
			}
			// Destination stage inject owns the next agent turn when routed.
			if alreadyRouted {
				s.publishHubNotice(clawID, planGateAlreadyAcceptedContent)
			} else {
				s.injectHubMessageByID(clawID, planGateAlreadyAcceptedContent)
			}
			s.safeGo("pipeline plan-gate already accepted", func() {
				s.autoTransitionAfterGate(clawID, stage.ID, "pass", ctx)
			})
			// injectDelivered only when we queued a model turn (no route).
			return !alreadyRouted, nil
		}

		gateResult := s.evaluateGate(clawID, stage.ID, stage.Gate)
		log.Printf("[pipeline] gate evaluated for claw %s stage %q: verdict=%s", clawID[:8], stage.ID, gateResult.Verdict)
		// Normalise "skipped" → "pass" when TreatSkippedAsPass is set so that
		// gate_result: { verdict: pass } stages are correctly reached.
		autoTransitionVerdict := gateResult.Verdict
		if autoTransitionVerdict == "skipped" && stage.Gate.TreatSkippedAsPass {
			autoTransitionVerdict = "pass"
		}
		gateResultHasRoute := false
		if pl := parsePipelineForContext(ctx); pl != nil {
			gateResultHasRoute = pl.StageForGateResult(stage.ID, autoTransitionVerdict) != nil
		}
		notifyGateResult := func(message string) {
			// Always surface gate outcomes in the transcript for human observers.
			// When a gate_result route will inject the next stage prompt, use a
			// notice (no extra model turn); otherwise inject so the agent sees it.
			if gateResultHasRoute {
				s.publishHubNotice(clawID, message)
				return
			}
			s.injectHubMessageByID(clawID, message)
		}
		// Report the gate result in chat with friendlier plan-gate wording.
		stageLabel := strings.TrimSpace(stage.Label)
		if stageLabel == "" {
			stageLabel = stage.ID
		}
		if gateResult.Verdict == "pass" {
			if stage.PlanGate {
				notifyGateResult(fmt.Sprintf("[hub] ✓ Plan approved (%s). Moving to implementation.", stageLabel))
			} else {
				notifyGateResult(fmt.Sprintf("[hub] ✓ Gate passed: %s", stageLabel))
			}
		} else if gateResult.Verdict == "skipped" && stage.Gate.TreatSkippedAsPass {
			notifyGateResult(fmt.Sprintf("[hub] Gate skipped (treated as pass): %s", stageLabel))
		} else if gateResult.Verdict == "error" {
			msg := fmt.Sprintf("[hub] Gate error (no condition matched): %s", stageLabel)
			notifyGateResult(msg)
		} else {
			msg := fmt.Sprintf("[hub] ✗ Gate failed: %s", stageLabel)
			if gateResult.MatchedPath != "" {
				msg += fmt.Sprintf("\n- path: %s\n- value: %s", gateResult.MatchedPath, gateResult.MatchedValue)
			}
			if gateResult.Reason != "" {
				msg += "\n- reason: " + gateResult.Reason
			}
			notifyGateResult(msg)
		}
		// Deterministic plan gates mark plan accepted so freeform never re-fires
		// if something races; freeform is already skipped when HasPlanGate().
		// Per-stage marker powers re-entry short-circuit without skipping later
		// plan_gate stages in the same workflow.
		if stage.PlanGate && autoTransitionVerdict == "pass" {
			if tenantID := s.tenantIDForClaw(clawID); tenantID != "" {
				_ = s.insertSystemMarker(clawID, tenantID, initialPlanAcceptedMarker)
				_ = s.insertSystemMarker(clawID, tenantID, planGateAcceptedMarker(stage.ID))
			}
			// Surface the approved plan in chat even when the agent only said
			// [PLAN_READY]. Prefer gate JSON fields; fall back to a short notice
			// when the validator only printed {"status":"ok"}.
			var summary string
			if stage.Gate != nil && stage.Gate.Output != "" {
				if outs := s.loadPipelineOutputs(clawID); outs != nil {
					summary = formatPlanGateSummary(outs[stage.Gate.Output])
				}
			}
			if summary == "" {
				summary = formatPlanGateSummary(nil)
			}
			s.publishHubNotice(clawID, summary)
			// When a gate_result route will inject the destination stage prompt,
			// do not also inject proceed — that queues a duplicate agent turn
			// before the implement instructions arrive (Greptile).
			if gateResultHasRoute {
				s.publishHubNotice(clawID, planGateProceedContent)
			} else {
				s.injectHubMessageByID(clawID, planGateProceedContent)
			}
		}
		// Auto-transition to next stage if a gate_result trigger matches.
		s.safeGo("pipeline gate auto-transition", func() {
			s.autoTransitionAfterGate(clawID, stage.ID, autoTransitionVerdict, ctx, gateResult.Reason)
		})
		// If gate is required and failed (or errored), block further on_enter actions
		if stage.Gate.Required && (gateResult.Verdict == "fail" || gateResult.Verdict == "error") {
			msg := fmt.Sprintf("Required gate %q %s — blocking further pipeline actions", stage.ID, gateResult.Verdict)
			log.Printf("[pipeline] %s", msg)
			if gateResultHasRoute {
				s.publishHubNotice(clawID, "[hub] Error: "+msg)
			} else {
				s.injectHubMessageByID(clawID, "[hub] Error: "+msg)
			}
			if pl := parsePipelineForContext(ctx); pl != nil && pl.StageForGateResult(stage.ID, gateResult.Verdict) != nil {
				return false, &routedRequiredGateError{stageID: stage.ID, verdict: gateResult.Verdict}
			}
			return false, fmt.Errorf("required gate %q %s", stage.ID, gateResult.Verdict)
		}
	}

	// Scripts like verify-github-pr-links emit PR URLs in JSON. Register them
	// only after a required gate (if any) has allowed the stage to continue, so
	// a failed gate cannot leave the PR watcher armed on a blocked claw.
	if strings.TrimSpace(runStdoutForPRScan) != "" {
		s.scanMessageForPRs(clawID, runStdoutForPRScan)
	}

	if stage.OnEnter.Inject != "" {
		injectMsg := stage.OnEnter.Inject
		manualInputs := s.loadManualTriggerInputs(clawID)

		// Build base template data from issue context, manual inputs, and persisted outputs.
		baseData := map[string]interface{}{}

		// Render {{.Issue.Identifier}}, {{.Issue.Title}}, {{.Issue.URL}} when this
		// claw is backed by a Linear or GitHub issue. Shortcut IDs (sc-...) are
		// intentionally excluded from the Issue namespace.
		if issueID != "" && !strings.HasPrefix(issueID, "sc-") {
			if !strings.Contains(issueID, "/") {
				// Linear issue
				log.Printf("[pipeline] attempting to render template for claw %s issue %s", clawID[:8], issueID)
				linearToken := s.resolveLinearTokenForPipeline(ctx)
				details := &linearIssueDetails{Identifier: issueID}
				if linearToken == "" {
					s.warnPipelineRender(clawID, "%s: no Linear issue tracker token configured; rendering inject with fallback issue context", ctx.Name())
				} else {
					var err error
					details, err = s.fetchLinearIssueDetails(linearToken, issueID)
					if err != nil {
						s.warnPipelineRender(clawID, "%s: failed to fetch Linear issue details for %s: %v", ctx.Name(), issueID, err)
						details = &linearIssueDetails{Identifier: issueID}
					}
					if details == nil {
						s.warnPipelineRender(clawID, "%s: Linear issue %s returned no details", ctx.Name(), issueID)
						details = &linearIssueDetails{Identifier: issueID}
					}
				}
				baseData["Issue"] = details
			} else {
				// GitHub issue — fetch details and render with same {{.Issue.*}} variables
				ghToken := s.resolveGitHubIssuesTokenForPipeline(ctx)
				details := fallbackGitHubIssueDetails(issueID)
				if ghToken != "" {
					parts := strings.Split(issueID, "/")
					if len(parts) == 3 {
						repo := parts[0] + "/" + parts[1]
						var issueNum int
						if _, err := fmt.Sscanf(parts[2], "%d", &issueNum); err == nil {
							base := s.githubBaseURL
							if base == "" {
								base = "https://api.github.com"
							}
							fetchedDetails, err := s.fetchGitHubIssueDetailsWithRetry(clawID, ghToken, repo, issueNum, base)
							if err != nil || fetchedDetails == nil {
								s.warnPipelineRender(clawID, "%s: failed to fetch GitHub issue details for %s: %v", ctx.Name(), issueID, err)
							} else {
								details = fetchedDetails
							}
						} else {
							s.warnPipelineRender(clawID, "%s: invalid GitHub issue number in %q: %v", ctx.Name(), issueID, err)
						}
					} else {
						s.warnPipelineRender(clawID, "%s: invalid GitHub issue ID format %q", ctx.Name(), issueID)
					}
					log.Printf("[pipeline] fetched GitHub issue %s: #%s title=%s", issueID, details.Identifier, details.Title)
				} else {
					s.warnPipelineRender(clawID, "%s: no GitHub Issues token configured; rendering inject with fallback issue context", ctx.Name())
				}
				baseData["Issue"] = details
			}
		}

		if manualInputs != nil {
			baseData["Inputs"] = manualInputs
		}

		// Always render with persisted outputs so cron and manual workflows can use {{.Outputs.*}}.
		injectMsg = renderInjectWithData(clawID, injectMsg, s.injectTemplateData(clawID, baseData))

		if inputContext := formatManualTriggerInputs(manualInputs); inputContext != "" {
			injectMsg = inputContext + "\n\n" + injectMsg
		}
		if s.clawNeedsInitialPlan(clawID) && s.insertSystemMarker(clawID, s.tenantIDForClaw(clawID), initialPlanRequiredMarker) {
			injectMsg = initialPlanWakeContent + "\n\nTask context:\n" + injectMsg
		}
		s.injectHubMessageByID(clawID, injectMsg)
		injectDelivered = true
	}

	if stage.OnEnter.MergePR {
		go s.mergePRForClaw(clawID)
	}

	if stage.OnEnter.CloseIssue {
		go s.closeGitHubIssueForClaw(clawID)
	}

	// Handle add_labels / remove_labels for GitHub Issues
	if len(stage.OnEnter.AddLabels) > 0 || len(stage.OnEnter.RemoveLabels) > 0 {
		if strings.Contains(issueID, "/") {
			ghToken := s.resolveGitHubIssuesTokenForPipeline(ctx)
			if ghToken != "" {
				parts := strings.Split(issueID, "/")
				if len(parts) == 3 {
					repo := parts[0] + "/" + parts[1]
					var issueNum int
					if _, err := fmt.Sscanf(parts[2], "%d", &issueNum); err == nil {
						base := s.githubBaseURL
						if base == "" {
							base = "https://api.github.com"
						}
						for _, label := range stage.OnEnter.AddLabels {
							if err := githubAPIAddLabel(base, repo, issueNum, label, ghToken); err != nil {
								log.Printf("[pipeline] failed to add label %q to issue %s: %v", label, issueID, err)
							} else {
								log.Printf("[pipeline] added label %q to issue %s", label, issueID)
							}
						}
						for _, label := range stage.OnEnter.RemoveLabels {
							if err := githubAPIDeleteLabel(base, repo, issueNum, label, ghToken); err != nil {
								log.Printf("[pipeline] failed to remove label %q from issue %s: %v", label, issueID, err)
							} else {
								log.Printf("[pipeline] removed label %q from issue %s", label, issueID)
							}
						}
					}
				}
			}
		}
	}

	if stage.OnEnter.Notify.Enabled {
		s.executeNotifyAction(clawID, stage, stage.OnEnter.Notify, ctx)
	}

	targetStatus := stage.OnEnter.MoveIssue.Status
	if targetStatus == "" {
		goto commentIssue
	}

	{
		// If pipeline specifies an explicit issue_id, resolve it from templates or use directly
		resolvedIssueID, _ := s.resolveIssueID(clawID, ctx, stage.OnEnter.MoveIssue.IssueID, issueID)
		if resolvedIssueID == "" {
			goto commentIssue
		}

		// Determine issue tracker: explicit workflow/factory integration takes precedence,
		// fall back to ID-format heuristics only when integration is empty.
		var isShortcut, isGitHub, isJira bool
		switch ctx.Integration() {
		case "shortcut":
			isShortcut = true
		case "github", "github-issues":
			isGitHub = true
		case "jira":
			isJira = true
		default:
			isShortcut = strings.HasPrefix(resolvedIssueID, "sc-")
			isGitHub = strings.Contains(resolvedIssueID, "/")
		}

		if isJira {
			tracker, ok := s.resolveJiraTrackerForPipeline(ctx)
			if !ok {
				log.Printf("[pipeline] %s: no Jira tracker for connection %q, skipping move_issue", ctx.Name(), ctx.TrackerName())
				goto commentIssue
			}
			if err := s.moveJiraIssue(tracker, resolvedIssueID, targetStatus); err != nil {
				log.Printf("[pipeline] failed to move Jira issue %s to %q: %v", resolvedIssueID, targetStatus, err)
			} else {
				log.Printf("[pipeline] moved Jira issue %s to %q", resolvedIssueID, targetStatus)
			}
		} else if isShortcut {
			// Shortcut story — ensure sc- prefix if missing (e.g. template rendered bare number)
			scID := resolvedIssueID
			if !strings.HasPrefix(scID, "sc-") {
				scID = "sc-" + scID
			}
			scToken := s.resolveShortcutTokenForPipeline(ctx)
			if scToken == "" {
				log.Printf("[pipeline] %s: no Shortcut token for connection %q, skipping move_issue", ctx.Name(), ctx.TrackerName())
				goto commentIssue
			}
			if err := moveShortcutStory(s.resolveShortcutBaseURL(), scToken, scID, targetStatus); err != nil {
				log.Printf("[pipeline] failed to move story %s to %q: %v", scID, targetStatus, err)
			} else {
				log.Printf("[pipeline] moved story %s to %q", scID, targetStatus)
			}
		} else if isGitHub {
			// GitHub issue (owner/repo/number format)
			ghToken := s.resolveGitHubIssuesTokenForPipeline(ctx)
			if ghToken == "" {
				log.Printf("[pipeline] %s: no GitHub Issues token for move_issue, skipping", ctx.Name())
				goto commentIssue
			}
			parts := strings.Split(resolvedIssueID, "/")
			if len(parts) != 3 {
				log.Printf("[pipeline] %s: GitHub issue ID %q is not owner/repo/number format — skipping move_issue", ctx.Name(), resolvedIssueID)
				goto commentIssue
			}
			repo := parts[0] + "/" + parts[1]
			var issueNum int
			if _, err := fmt.Sscanf(parts[2], "%d", &issueNum); err != nil {
				log.Printf("[pipeline] %s: invalid GitHub issue number in %q — skipping move_issue", ctx.Name(), resolvedIssueID)
				goto commentIssue
			}
			if err := moveGitHubIssue(ghToken, repo, issueNum, targetStatus, s.githubBaseURL); err != nil {
				log.Printf("[pipeline] failed to move GitHub issue %s to %q: %v", resolvedIssueID, targetStatus, err)
			} else {
				log.Printf("[pipeline] moved GitHub issue %s to %q", resolvedIssueID, targetStatus)
			}
		} else {
			// Linear issue
			linearToken := s.resolveLinearTokenForPipeline(ctx)
			if linearToken == "" {
				log.Printf("[pipeline] %s: no Linear token for connection %q, skipping move_issue", ctx.Name(), ctx.TrackerName())
				goto commentIssue
			}
			if err := s.moveLinearIssueOnServer(linearToken, resolvedIssueID, targetStatus); err != nil {
				log.Printf("[pipeline] failed to move issue %s to %q: %v", resolvedIssueID, targetStatus, err)
			} else {
				log.Printf("[pipeline] moved issue %s to %q", resolvedIssueID, targetStatus)
			}
		}
	}

commentIssue:
	if err := s.runCommentIssueOnStage(clawID, stage, ctx, issueID); err != nil {
		return injectDelivered, err
	}
	return injectDelivered, nil
}

// pipelinePullRequest is the template-facing view of the claw's active PR.
// Only fields intentionally exposed to comment_issue bodies are included.
type pipelinePullRequest struct {
	URL string
}

// loadPipelinePullRequest returns the most recently registered PR for a claw,
// or nil when the claw has no PR row yet. Templates dereference nil safely with
// {{ if .PullRequest }}…{{ end }}, so callers should pass either the struct or
// nil directly rather than synthesizing an empty value.
func (s *Server) loadPipelinePullRequest(clawID string) *pipelinePullRequest {
	var prURL string
	err := s.db.QueryRow(`SELECT pr_url FROM claw_prs WHERE claw_id=? ORDER BY rowid DESC LIMIT 1`, clawID).Scan(&prURL)
	if err != nil {
		return nil
	}
	if strings.TrimSpace(prURL) == "" {
		return nil
	}
	return &pipelinePullRequest{URL: prURL}
}

// resolveIssueID resolves an optional explicit issue ID (with template
// support) against the fallback trigger issue. When explicit is empty, the
// fallback is returned unchanged. Template rendering follows the same
// semantics as the on_enter.inject path — {{.Inputs.*}} for manual triggers
// and {{.Issue.*}} for automatic triggers.
func (s *Server) resolveIssueID(clawID string, ctx pipelineContext, explicit, fallback string) (string, error) {
	if explicit == "" {
		return fallback, nil
	}
	resolved := explicit
	if strings.Contains(resolved, "{{.Inputs.") {
		inputs := s.loadManualTriggerInputs(clawID)
		if inputs != nil {
			tmpl, err := template.New("issue_id").Parse(resolved)
			if err == nil {
				var buf bytes.Buffer
				data := s.injectTemplateData(clawID, map[string]interface{}{
					"Inputs": inputs,
				})
				if err := tmpl.Execute(&buf, data); err == nil {
					resolved = buf.String()
				}
			}
		}
	}
	if strings.Contains(resolved, "{{.Issue.") {
		var linearDetails *linearIssueDetails
		if fallback != "" && !strings.HasPrefix(fallback, "sc-") && !strings.Contains(fallback, "/") {
			linearToken := s.resolveLinearTokenForPipeline(ctx)
			if linearToken != "" {
				if d, err := s.fetchLinearIssueDetails(linearToken, fallback); err == nil && d != nil {
					linearDetails = d
				}
			}
		} else if strings.Contains(fallback, "/") {
			ghToken := s.resolveGitHubIssuesTokenForPipeline(ctx)
			if ghToken != "" {
				parts := strings.Split(fallback, "/")
				if len(parts) == 3 {
					repo := parts[0] + "/" + parts[1]
					var issueNum int
					if _, err := fmt.Sscanf(parts[2], "%d", &issueNum); err == nil {
						base := s.githubBaseURL
						if base == "" {
							base = "https://api.github.com"
						}
						if d, err := s.fetchGitHubIssueDetails(ghToken, repo, issueNum, base); err == nil && d != nil {
							var ghDetails githubIssueDetails = *d
							tmpl, err := template.New("issue_id").Parse(resolved)
							if err == nil {
								var buf bytes.Buffer
								data := s.injectTemplateData(clawID, map[string]interface{}{
									"Issue": &ghDetails,
								})
								if err := tmpl.Execute(&buf, data); err == nil {
									resolved = buf.String()
								}
							}
							return resolved, nil
						}
					}
				}
			}
		}
		if linearDetails != nil {
			tmpl, err := template.New("issue_id").Parse(resolved)
			if err == nil {
				var buf bytes.Buffer
				data := s.injectTemplateData(clawID, map[string]interface{}{
					"Issue": linearDetails,
				})
				if err := tmpl.Execute(&buf, data); err == nil {
					resolved = buf.String()
				}
			}
		}
	}
	return resolved, nil
}

// runCommentIssueOnStage posts a rendered comment_issue body to the workflow's
// tracker issue. Failures are always logged and surfaced as an in-transcript
// warning; they only propagate as an error when ContinueOnError is false and
// the stage is not terminal, so a failed comment on a terminal stage never
// blocks completion.
func (s *Server) runCommentIssueOnStage(clawID string, stage pipeline.Stage, ctx pipelineContext, issueID string) error {
	if strings.TrimSpace(stage.OnEnter.CommentIssue.Body) == "" {
		return nil
	}

	resolvedIssueID, _ := s.resolveIssueID(clawID, ctx, stage.OnEnter.CommentIssue.IssueID, issueID)
	if resolvedIssueID == "" {
		return nil
	}

	// Determine issue tracker with the same heuristic as move_issue.
	var isShortcut, isGitHub, isJira bool
	integration := ctx.Integration()
	switch integration {
	case "shortcut":
		isShortcut = true
	case "github", "github-issues":
		isGitHub = true
	case "jira":
		isJira = true
	default:
		isShortcut = strings.HasPrefix(resolvedIssueID, "sc-")
		isGitHub = strings.Contains(resolvedIssueID, "/")
	}

	// Build template data: {{.Issue.*}}, {{.Inputs.*}}, {{.PullRequest.*}}
	// (plus {{.Outputs.*}} via injectTemplateData) mirror the inject path so
	// authors can share templates between inject and comment_issue.
	data := map[string]interface{}{}
	if manualInputs := s.loadManualTriggerInputs(clawID); manualInputs != nil {
		data["Inputs"] = manualInputs
	}
	if pr := s.loadPipelinePullRequest(clawID); pr != nil {
		data["PullRequest"] = pr
	}
	switch {
	case isGitHub:
		details := fallbackGitHubIssueDetails(resolvedIssueID)
		ghToken := s.resolveGitHubIssuesTokenForPipeline(ctx)
		if ghToken != "" {
			parts := strings.Split(resolvedIssueID, "/")
			if len(parts) == 3 {
				repo := parts[0] + "/" + parts[1]
				var issueNum int
				if _, err := fmt.Sscanf(parts[2], "%d", &issueNum); err == nil {
					base := s.githubBaseURL
					if base == "" {
						base = "https://api.github.com"
					}
					if d, err := s.fetchGitHubIssueDetailsWithRetry(clawID, ghToken, repo, issueNum, base); err == nil && d != nil {
						details = d
					}
				}
			}
		}
		data["Issue"] = details
	case isShortcut, isJira:
		// Shortcut/Jira: no rich fetch — use a bare identifier stub so the
		// {{.Issue.Identifier}} template still resolves without a live API call.
		data["Issue"] = &linearIssueDetails{Identifier: resolvedIssueID}
	default:
		// Linear: try live fetch, fall back to identifier-only stub.
		details := &linearIssueDetails{Identifier: resolvedIssueID}
		linearToken := s.resolveLinearTokenForPipeline(ctx)
		if linearToken != "" {
			if d, err := s.fetchLinearIssueDetails(linearToken, resolvedIssueID); err == nil && d != nil {
				details = d
			}
		}
		data["Issue"] = details
	}
	renderedBody := renderInjectWithData(clawID, stage.OnEnter.CommentIssue.Body, s.injectTemplateData(clawID, data))

	handleErr := func(integration string, err error) error {
		log.Printf("[pipeline] failed to comment %s issue %s: %v", integration, resolvedIssueID, err)
		s.injectHubMessageByID(clawID, "[hub] Warning: comment_issue failed: "+err.Error())
		if stage.Terminal {
			return nil
		}
		if stage.OnEnter.CommentIssue.ContinueOnError {
			return nil
		}
		return err
	}

	if isJira {
		tracker, ok := s.resolveJiraTrackerForPipeline(ctx)
		if !ok {
			log.Printf("[pipeline] %s: no Jira tracker for connection %q, skipping comment_issue", ctx.Name(), ctx.TrackerName())
			return nil
		}
		s.publishHubNotice(clawID, fmt.Sprintf("[hub] ▶ Posting comment_issue for stage %q", stage.ID))
		if err := s.retryTrackerMove("comment jira issue", func() error {
			return s.commentJiraIssue(tracker, resolvedIssueID, renderedBody)
		}); err != nil {
			return handleErr("jira", err)
		}
		log.Printf("[pipeline] commented jira issue %s", resolvedIssueID)
		return nil
	}
	if isShortcut {
		scID := resolvedIssueID
		if !strings.HasPrefix(scID, "sc-") {
			scID = "sc-" + scID
		}
		scToken := s.resolveShortcutTokenForPipeline(ctx)
		if scToken == "" {
			log.Printf("[pipeline] %s: no Shortcut token for connection %q, skipping comment_issue", ctx.Name(), ctx.TrackerName())
			return nil
		}
		s.publishHubNotice(clawID, fmt.Sprintf("[hub] ▶ Posting comment_issue for stage %q", stage.ID))
		if err := s.retryTrackerMove("comment shortcut issue", func() error {
			return commentShortcutIssue(s.resolveShortcutBaseURL(), scToken, scID, renderedBody)
		}); err != nil {
			return handleErr("shortcut", err)
		}
		log.Printf("[pipeline] commented shortcut issue %s", scID)
		return nil
	}
	if isGitHub {
		ghToken := s.resolveGitHubIssuesTokenForPipeline(ctx)
		if ghToken == "" {
			log.Printf("[pipeline] %s: no GitHub Issues token for comment_issue, skipping", ctx.Name())
			return nil
		}
		parts := strings.Split(resolvedIssueID, "/")
		if len(parts) != 3 {
			log.Printf("[pipeline] %s: GitHub issue ID %q is not owner/repo/number format — skipping comment_issue", ctx.Name(), resolvedIssueID)
			return nil
		}
		repo := parts[0] + "/" + parts[1]
		var issueNum int
		if _, err := fmt.Sscanf(parts[2], "%d", &issueNum); err != nil {
			log.Printf("[pipeline] %s: invalid GitHub issue number in %q — skipping comment_issue", ctx.Name(), resolvedIssueID)
			return nil
		}
		s.publishHubNotice(clawID, fmt.Sprintf("[hub] ▶ Posting comment_issue for stage %q", stage.ID))
		base := s.githubBaseURL
		if err := s.retryTrackerMove("comment github issue", func() error {
			if base == "" {
				return commentGitHubIssue(ghToken, repo, issueNum, renderedBody)
			}
			return commentGitHubIssueWithBase(base, ghToken, repo, issueNum, renderedBody)
		}); err != nil {
			return handleErr("github", err)
		}
		log.Printf("[pipeline] commented github issue %s", resolvedIssueID)
		return nil
	}

	// Linear
	linearToken := s.resolveLinearTokenForPipeline(ctx)
	if linearToken == "" {
		log.Printf("[pipeline] %s: no Linear token for connection %q, skipping comment_issue", ctx.Name(), ctx.TrackerName())
		return nil
	}
	s.publishHubNotice(clawID, fmt.Sprintf("[hub] ▶ Posting comment_issue for stage %q", stage.ID))
	if err := s.retryTrackerMove("comment linear issue", func() error {
		return s.commentLinearIssue(linearToken, resolvedIssueID, renderedBody)
	}); err != nil {
		return handleErr("linear", err)
	}
	log.Printf("[pipeline] commented linear issue %s", resolvedIssueID)
	return nil
}

func pipelineTerminalWorkflowRunResult(stage pipeline.Stage, stageActionsSucceeded bool) (status, result string) {
	if !stageActionsSucceeded || pipelineTerminalStageIndicatesFailure(stage) {
		return "failed", "pipeline terminal stage failed"
	}
	return "completed", "pipeline terminal stage"
}

func pipelineTerminalStageIndicatesFailure(stage pipeline.Stage) bool {
	for _, trigger := range stage.Triggers {
		if pipelineFailureVerdict(trigger.JudgeVerdict) {
			return true
		}
		if trigger.GateResult != nil && pipelineFailureVerdict(trigger.GateResult.Verdict) {
			return true
		}
		if trigger.OutputMatches != nil {
			for _, value := range trigger.OutputMatches.AnyOf {
				if pipelineFailureVerdict(value) {
					return true
				}
			}
		}
	}
	return false
}

func pipelineFailureVerdict(value string) bool {
	switch strings.ToLower(strings.TrimSpace(value)) {
	case "fail", "failed", "failure", "error", "errored":
		return true
	default:
		return false
	}
}

// transitionPipelineStage sets the claw's current pipeline stage and runs on_enter.
// If the stage is terminal, it terminates the claw after running on_enter and
// ensuring any injected message is delivered (waits if agent is streaming).
func (s *Server) transitionPipelineStage(clawID string, stage pipeline.Stage, factory *types.FactoryConfig, issueID string) bool {
	return s.transitionPipelineStageWithContext(clawID, stage, pipelineContext{Factory: factory, IssueID: issueID})
}

// checkPipelineMessageTriggers evaluates pipeline triggers against a claw message
// and transitions to the matching stage if found. Returns true if a transition occurred.
func (s *Server) checkPipelineMessageTriggers(clawID, message string) bool {
	ctx, stage, ok := s.pipelineStageForMessageContains(clawID, message)
	if !ok {
		ctx, ok = s.findPipelineContextForClaw(clawID)
		if !ok {
			return false
		}
		pl := parsePipelineForContext(ctx)
		if pl == nil {
			return false
		}
		stage = nil
		// Also check output_matches triggers against current pipeline outputs.
		// Only evaluate if the claw has not already visited this stage, to
		// prevent persistent DB state from re-triggering backwards regressions
		// on every subsequent message.
		outputs := s.loadPipelineOutputs(clawID)
		stage = pl.StageForOutputMatches(outputs)
		if stage != nil && s.hasVisitedPipelineStage(clawID, stage.ID) {
			stage = nil
		}
	}
	if stage == nil {
		return false
	}
	return s.transitionPipelineStageWithContext(clawID, *stage, ctx)
}

func (s *Server) hasPipelineMessageContainsTrigger(clawID, message string) bool {
	_, _, ok := s.pipelineStageForMessageContains(clawID, message)
	return ok
}

func (s *Server) pipelineStageForMessageContains(clawID, message string) (pipelineContext, *pipeline.Stage, bool) {
	ctx, ok := s.findPipelineContextForClaw(clawID)
	if !ok {
		return pipelineContext{}, nil, false
	}
	pl := parsePipelineForContext(ctx)
	if pl == nil {
		return pipelineContext{}, nil, false
	}
	stage := pl.StageForMessageContains(message)
	if stage == nil {
		return pipelineContext{}, nil, false
	}
	return ctx, stage, true
}

func (s *Server) transitionPipelineStageWithContext(clawID string, stage pipeline.Stage, ctx pipelineContext) bool {
	stage = s.resolvePipelineStageSkips(clawID, stage, ctx)
	transitioned, _ := s.transitionResolvedPipelineStageWithContext(clawID, stage, ctx)
	return transitioned
}

func (s *Server) transitionResolvedPipelineStageWithContext(clawID string, stage pipeline.Stage, ctx pipelineContext) (transitioned, injectDelivered bool) {
	if !s.claimPipelineStageTransition(clawID, stage.ID) {
		log.Printf("[pipeline] claw %s already in stage %q (%s), skipping duplicate transition", clawID[:8], stage.ID, stage.Label)
		return false, false
	}
	// Record that this claw has visited this stage, so one-shot triggers
	// (like output_matches) don't re-fire on subsequent messages.
	s.recordPipelineStageVisit(clawID, stage.ID)
	log.Printf("[pipeline] claw %s → stage %q (%s)", clawID[:8], stage.ID, stage.Label)
	// User-visible stage progress in the transcript (dashboard only — does not
	// queue a model turn). Makes workflow progress less of a black box.
	stageLabel := strings.TrimSpace(stage.Label)
	if stageLabel == "" {
		stageLabel = stage.ID
	}
	s.publishHubNotice(clawID, fmt.Sprintf("[hub] ▶ Stage: %s", stageLabel))
	injectDelivered, onEnterErr := s.runOnEnter(clawID, stage, ctx)
	stageActionsSucceeded := onEnterErr == nil
	var routedGateErr *routedRequiredGateError
	if errors.As(onEnterErr, &routedGateErr) {
		// autoTransitionAfterGate was started by runOnEnter and owns the declared
		// gate_result route. Do not race it by terminating this source stage.
		return true, false
	}
	if onEnterErr != nil && !stage.Terminal {
		s.stopAgentWithReason(clawID, fmt.Sprintf("pipeline stage %q on_enter failed: %v", stage.ID, onEnterErr), false)
		return true, false
	}

	// If this is a terminal stage, terminate the claw
	if stage.Terminal {
		log.Printf("[pipeline] claw %s reached terminal stage %q — terminating", clawID[:8], stage.ID)

		// Wait for any streaming response to finish so injected terminal message
		// is delivered before we close the connection.
		for i := 0; i < 60; i++ {
			s.mu.RLock()
			cc, connected := s.claws[clawID]
			streaming := connected && cc.streamingBuf.Len() > 0
			s.mu.RUnlock()
			if !streaming {
				break
			}
			log.Printf("[pipeline] claw %s is streaming, waiting before terminal termination...", clawID[:8])
			time.Sleep(500 * time.Millisecond)
		}

		var tenantID, providerID, provider string
		_ = s.db.QueryRow(`SELECT tenant_id, COALESCE(provider_id,''), COALESCE(provider,'') FROM claws WHERE id=?`, clawID).Scan(&tenantID, &providerID, &provider)

		s.checkpointBeforeTermination(clawID, "pipeline-terminal")
		s.syncWorkflowVolumes(clawID)

		status, result := pipelineTerminalWorkflowRunResult(stage, stageActionsSucceeded)
		applied, err := s.finishClawTerminalTx(clawID, "deleted", "", status, result, terminalTxOpts{})
		if err != nil || !applied {
			return transitioned, injectDelivered
		}
		if s.cronScheduler != nil {
			s.cronScheduler.releaseClawWorkflowSlot(clawID)
		}
		s.mu.Lock()
		if cc, ok := s.claws[clawID]; ok {
			cc.conn.Close(1000, "pipeline terminal stage")
			delete(s.claws, clawID)
		}
		s.mu.Unlock()

		s.broadcastToUsers(tenantID, types.WSMessage{
			Type:    "claw_status",
			Payload: map[string]string{"claw_id": clawID, "status": "deleted"},
		})

		if providerID != "" {
			go s.terminateVMForClaw(clawID, provider, providerID)
		}
	}
	return true, injectDelivered
}

func (s *Server) resolvePipelineStageSkips(clawID string, stage pipeline.Stage, ctx pipelineContext) pipeline.Stage {
	pl := parsePipelineForContext(ctx)
	if pl == nil {
		return stage
	}
	if !ctx.IssueLabelsAvailable {
		if labels, ok := s.loadIssueLabelsForClaw(clawID); ok {
			ctx.IssueLabels = labels
			ctx.IssueLabelsAvailable = true
		}
	}
	current := &stage
	visited := map[string]bool{}
	for current != nil {
		if visited[current.ID] {
			log.Printf("[pipeline] claw %s: skip cycle reached at stage %q", clawID[:8], current.ID)
			return *current
		}
		visited[current.ID] = true
		targetID, skip := pipelineStageSkipTarget(*current, ctx.IssueLabels, ctx.IssueLabelsAvailable)
		if !skip {
			return *current
		}
		target := pl.StageByID(targetID)
		if target == nil {
			log.Printf("[pipeline] claw %s: stage %q skip target %q not found", clawID[:8], current.ID, targetID)
			return *current
		}
		log.Printf("[pipeline] claw %s: skipping stage %q to %q", clawID[:8], current.ID, target.ID)
		current = target
	}
	return stage
}

func pipelineStageSkipTarget(stage pipeline.Stage, issueLabels []string, labelsAvailable bool) (string, bool) {
	if stage.SkipIf != nil {
		if labelsAvailable && issueLabelSkipMatches(stage.SkipIf, issueLabels) {
			return stage.SkipIf.GoTo, true
		}
	}
	if stage.SkipUnless != nil {
		if !labelsAvailable || !issueLabelSkipMatches(stage.SkipUnless, issueLabels) {
			return stage.SkipUnless.GoTo, true
		}
	}
	return "", false
}

func issueLabelSkipMatches(rule *pipeline.StageSkip, issueLabels []string) bool {
	if rule == nil || rule.IssueLabels == nil {
		return false
	}
	actual := normalizedIssueLabelSet(issueLabels)
	for _, label := range rule.IssueLabels.Labels {
		if actual[strings.ToLower(strings.TrimSpace(label))] {
			return true
		}
	}
	return false
}

func normalizeIssueLabels(labels []string) []string {
	seen := map[string]bool{}
	normalized := make([]string, 0, len(labels))
	for _, label := range labels {
		label = strings.TrimSpace(label)
		if label == "" {
			continue
		}
		key := strings.ToLower(label)
		if seen[key] {
			continue
		}
		seen[key] = true
		normalized = append(normalized, label)
	}
	return normalized
}

func normalizedIssueLabelSet(labels []string) map[string]bool {
	set := make(map[string]bool, len(labels))
	for _, label := range normalizeIssueLabels(labels) {
		set[strings.ToLower(label)] = true
	}
	return set
}

// autoTransitionAfterJudge checks the pipeline for a stage with a judge_verdict
// trigger matching the given verdict and transitions to it. This enables
// automated retry loops: judge fail → fix stage → retest → final judge.
func (s *Server) autoTransitionAfterJudge(clawID, verdict string, ctx pipelineContext) {
	pl := parsePipelineForContext(ctx)
	if pl == nil {
		return
	}
	stage := pl.StageForJudgeVerdict(verdict)
	if stage == nil {
		log.Printf("[pipeline] claw %s: no stage with judge_verdict=%q trigger found", clawID[:8], verdict)
		return
	}
	log.Printf("[pipeline] claw %s: auto-transitioning to stage %q after judge verdict=%s", clawID[:8], stage.ID, verdict)
	s.transitionPipelineStageWithContext(clawID, *stage, ctx)
}

// GateEvaluationResult is the outcome of evaluating a gate.
type GateEvaluationResult struct {
	Verdict      string // "pass", "fail", "skipped", "error"
	MatchedPath  string
	MatchedValue string
	Reason       string
}

// evaluateGate inspects a named pipeline output and evaluates the gate
// conditions to produce a pass/fail/skipped/error verdict.
func (s *Server) evaluateGate(clawID, stageID string, gate *pipeline.Gate) *GateEvaluationResult {
	result := &GateEvaluationResult{Verdict: "error"}

	// Load the named output
	outputs := s.loadPipelineOutputs(clawID)
	out, ok := outputs[gate.Output]
	if !ok {
		if gate.TreatSkippedAsPass {
			result.Verdict = "skipped"
		} else {
			result.Verdict = "error"
			result.Reason = fmt.Sprintf("required output %q was not found", gate.Output)
		}
		log.Printf("[pipeline] gate %q for claw %s: output %q not found, verdict=%s", stageID, clawID[:8], gate.Output, result.Verdict)
		s.persistGateResult(clawID, stageID, gate, result)
		return result
	}

	// Evaluate pass condition
	if gate.Pass.Path != "" {
		val := pipeline.GetJSONPath(out, gate.Pass.Path)
		strVal := fmt.Sprintf("%v", val)
		for _, expected := range gate.Pass.Values {
			if strings.EqualFold(strVal, expected) {
				result.Verdict = "pass"
				result.MatchedPath = gate.Pass.Path
				result.MatchedValue = strVal
				log.Printf("[pipeline] gate %q for claw %s: pass matched path=%s value=%s", stageID, clawID[:8], gate.Pass.Path, strVal)
				s.persistGateResult(clawID, stageID, gate, result)
				return result
			}
		}
	}

	// Evaluate fail condition
	if gate.Fail.Path != "" {
		val := pipeline.GetJSONPath(out, gate.Fail.Path)
		strVal := fmt.Sprintf("%v", val)
		for _, expected := range gate.Fail.Values {
			if strings.EqualFold(strVal, expected) {
				result.Verdict = "fail"
				result.MatchedPath = gate.Fail.Path
				result.MatchedValue = strVal
				log.Printf("[pipeline] gate %q for claw %s: fail matched path=%s value=%s", stageID, clawID[:8], gate.Fail.Path, strVal)
				s.persistGateResult(clawID, stageID, gate, result)
				return result
			}
		}
	}

	// Neither pass nor fail matched — error
	result.Reason = "no gate condition matched"
	log.Printf("[pipeline] gate %q for claw %s: no conditions matched, verdict=error", stageID, clawID[:8])
	s.persistGateResult(clawID, stageID, gate, result)
	return result
}

// persistGateResult stores a gate evaluation result in the DB.
func (s *Server) persistGateResult(clawID, stageID string, gate *pipeline.Gate, result *GateEvaluationResult) {
	_, err := s.db.Exec(`
		INSERT INTO pipeline_gate_results(claw_id, stage_id, output_name, verdict, matched_path, matched_value, required, created_at)
		VALUES(?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(claw_id, stage_id) DO UPDATE SET
			output_name=excluded.output_name,
			verdict=excluded.verdict,
			matched_path=excluded.matched_path,
			matched_value=excluded.matched_value,
			required=excluded.required,
			created_at=excluded.created_at`,
		clawID, stageID, gate.Output, result.Verdict, result.MatchedPath, result.MatchedValue, gate.Required, now())
	if err != nil {
		log.Printf("[pipeline] failed to persist gate result for claw %s stage %s: %v", clawID[:8], stageID, err)
	}
}

// loadGateResult returns the most recent gate result for a given claw and stage.
// Currently used by tests and available for future UI / dashboard display of gate history.
func (s *Server) loadGateResult(clawID, stageID string) *GateEvaluationResult {
	var verdict, matchedPath, matchedValue string
	var required bool
	err := s.db.QueryRow(`
		SELECT verdict, matched_path, matched_value, required
		FROM pipeline_gate_results
		WHERE claw_id=? AND stage_id=?`, clawID, stageID).Scan(&verdict, &matchedPath, &matchedValue, &required)
	if err != nil {
		return nil
	}
	return &GateEvaluationResult{
		Verdict:      verdict,
		MatchedPath:  matchedPath,
		MatchedValue: matchedValue,
	}
}

// hasFailedRequiredGate checks whether any required gate for this claw has failed
// or errored (i.e. produced a non-pass verdict that should block PR creation).
func (s *Server) hasFailedRequiredGate(clawID string) bool {
	var count int
	err := s.db.QueryRow(`
		SELECT COUNT(*) FROM pipeline_gate_results
		WHERE claw_id=? AND verdict IN ('fail','error') AND required=1`, clawID).Scan(&count)
	if err != nil {
		return false
	}
	return count > 0
}

// autoTransitionAfterGate checks the pipeline for a stage with a gate_result
// trigger matching the given stage ID and verdict, and transitions to it.
func (s *Server) autoTransitionAfterGate(clawID, stageID, verdict string, ctx pipelineContext, errorReasons ...string) {
	pl := parsePipelineForContext(ctx)
	if pl == nil {
		return
	}
	stage := pl.StageForGateResult(stageID, verdict)
	if stage == nil {
		log.Printf("[pipeline] claw %s: no stage with gate_result stage=%s verdict=%s trigger found", clawID[:8], stageID, verdict)
		if verdict == "error" {
			reason := "no gate condition matched"
			if len(errorReasons) > 0 && strings.TrimSpace(errorReasons[0]) != "" {
				reason = errorReasons[0]
			}
			s.stopAgentWithReason(clawID, fmt.Sprintf("pipeline gate %q errored: %s", stageID, reason), false)
		}
		return
	}
	log.Printf("[pipeline] claw %s: auto-transitioning to stage %q after gate stage=%s verdict=%s", clawID[:8], stage.ID, stageID, verdict)
	s.transitionPipelineStageWithContext(clawID, *stage, ctx)
}

// initializePipelineEntryIfNeeded transitions a claw into its entry pipeline stage
// exactly once, after the claw is connected and ready.
// Returns true when entry on_enter inject should be used as the initial wake-up.
func (s *Server) initializePipelineEntryIfNeeded(clawID string) bool {
	// Entry runs only once; if a stage is already set we are done.
	if s.getPipelineStage(clawID) != "" {
		return false
	}

	ctx, ok := s.findPipelineContextForClaw(clawID)
	log.Printf("[pipeline] initializePipelineEntryIfNeeded: claw=%s pipeline=%s found=%v issueID=%q", clawID[:8], ctx.Name(), ok, ctx.IssueID)
	if !ok {
		return false
	}
	pl := parsePipelineForContext(ctx)
	if pl == nil {
		return false
	}
	entry := pl.EntryStage()
	if entry == nil {
		return false
	}

	effectiveEntry := s.resolvePipelineStageSkips(clawID, *entry, ctx)
	_, injectDelivered := s.transitionResolvedPipelineStageWithContext(clawID, effectiveEntry, ctx)
	return injectDelivered
}

// stopAgentWithReason is the centralized handler for unexpected agent termination.
// Every path that means "the agent is dead" routes through here.
// It: sets status='error', broadcasts to dashboard, writes issue-tracker comment, terminates VM.
// skipVMTerminate should be true when the caller already knows the VM is gone (e.g. Replicated
// poll saw "terminated") to avoid redundant delete attempts that spam the logs with 404 errors.
func (s *Server) stopAgentWithReason(clawID, reason string, skipVMTerminate bool) {
	disposition := resolveClawStopDisposition(func() clawRetryDisposition {
		return s.scheduleClawRetry(clawID, reason)
	}, time.Sleep)
	switch disposition {
	case clawRetryScheduled, clawRetryAlreadyHandled:
		s.promotePendingClaws()
		return
	}
	s.stopAgentTerminalWithReason(clawID, reason, skipVMTerminate)
	s.promotePendingClaws()
}

func (s *Server) stopAgentTerminalWithReason(clawID, reason string, skipVMTerminate bool) {
	// Resolve workflow/factory + issueID
	pipelineCtx, hasPipelineCtx := s.findPipelineContextForClaw(clawID)
	factory, issueID := s.findFactoryForClaw(clawID)
	if factory == nil && (!hasPipelineCtx || pipelineCtx.Workflow == nil) {
		log.Printf("[stopAgent] claw %s: no issue tracker context found, skipping issue tracker comment", clawID[:8])
	}

	// Fetch tenantID + provider info for broadcast + VM cleanup
	var tenantID, providerID, provider string
	_ = s.db.QueryRow(`SELECT tenant_id, COALESCE(provider_id,''), COALESCE(provider,'') FROM claws WHERE id=?`, clawID).Scan(&tenantID, &providerID, &provider)

	// 1. Set terminal status and finish the workflow run atomically.
	safeReason := firstUsefulFailureLines(sanitizeFailureDetails(reason), 4)
	commentOwed := hasPipelineCtx && pipelineCtx.Workflow != nil && pipelineCtx.IssueID != "" && isFailureFeedbackWorkflowIntegration(pipelineCtx.Workflow.Integration) || factory != nil && issueID != ""
	applied, updateErr := s.finishClawTerminalTx(clawID, "error", safeReason, "failed", safeReason, terminalTxOpts{
		setStopCommentPending: commentOwed,
		clearProviderID:       skipVMTerminate && providerID != "",
	})
	if updateErr != nil {
		return
	}
	if !applied {
		return
	}
	if s.cronScheduler != nil {
		s.cronScheduler.releaseClawWorkflowSlot(clawID)
	}
	{
		if err := s.recordTaskRunEventForClaw(clawID, TaskRunEvent{
			EventKey:        "agent_stopped:" + clawID,
			Source:          taskRunSourceHub,
			EventType:       taskRunEventAgentStopped,
			ActorType:       taskRunActorSystem,
			InteractionRole: taskRunInteractionTerminal,
			FailureType:     taskRunFailureAgentStopped,
			Detail:          map[string]any{"reason": safeReason},
			OccurredAt:      now(),
		}); err != nil {
			log.Printf("[task-run-analytics] failed to record agent stop for claw %s: %v", clawID, err)
		}
	}

	// A checkpoint request can wait up to checkpointTerminationTimeout for a
	// bridge response.  Do not leave the claw and any workflow run marked as
	// running while that best-effort preservation step waits.
	s.checkpointBeforeTermination(clawID, "stop-agent")
	s.syncWorkflowVolumes(clawID)

	// 2. Broadcast "Agent Stopped" card to dashboard
	s.broadcastToUsers(tenantID, types.WSMessage{
		Type:    "claw_status",
		Payload: map[string]string{"claw_id": clawID, "status": "error", "reason": safeReason, "bootstrap_diagnostic": safeReason},
	})

	// 3. Disconnect WebSocket if still connected
	s.mu.Lock()
	if cc, ok := s.claws[clawID]; ok {
		cc.conn.Close(1000, "Agent stopped: "+safeReason)
		delete(s.claws, clawID)
	}
	delete(s.gatewayUnhealthyCounts, clawID)
	s.mu.Unlock()

	// 4. Write issue-tracker comment without delaying agent shutdown.
	if hasPipelineCtx && pipelineCtx.Workflow != nil && pipelineCtx.IssueID != "" && isFailureFeedbackWorkflowIntegration(pipelineCtx.Workflow.Integration) {
		s.dispatchWorkflowStopComment(clawID, pipelineCtx, reason)
	} else if factory != nil && issueID != "" {
		factoryCopy := *factory
		s.dispatchFactoryStopComment(clawID, &factoryCopy, issueID, reason)
	}

	// 5. Terminate VM if still running
	if providerID != "" && !skipVMTerminate {
		go func() {
			s.captureGatewayLog(clawID, provider, providerID)
			s.terminateVMForClaw(clawID, provider, providerID)
		}()
	}

	log.Printf("[stopAgent] claw %s stopped: %s", clawID[:8], reason)
}

func isFailureFeedbackWorkflowIntegration(integration string) bool {
	return integration == "github-issues" || integration == "linear" || integration == "shortcut" || integration == "jira"
}

// dispatchStopComment claims a pending notification before starting network I/O.
// The claim prevents a slow initial delivery and a reaper redrive from posting
// the same tracker comment concurrently. Failed deliveries restore pending=1.
func (s *Server) dispatchStopComment(clawID string, deliver func()) {
	res, err := s.db.Exec(`UPDATE claws SET stop_comment_pending=2 WHERE id=? AND stop_comment_pending=1`, clawID)
	if err != nil {
		return
	}
	if rows, _ := res.RowsAffected(); rows == 1 {
		go deliver()
	}
}

func (s *Server) dispatchWorkflowStopComment(clawID string, ctx pipelineContext, reason string) {
	s.dispatchStopComment(clawID, func() { s.commentWorkflowAgentStopToTracker(clawID, ctx, reason) })
}

func (s *Server) dispatchFactoryStopComment(clawID string, factory *types.FactoryConfig, issueID, reason string) {
	s.dispatchStopComment(clawID, func() { s.commentAgentStopToTracker(clawID, factory, issueID, reason) })
}

func (s *Server) commentWorkflowAgentStopToTracker(clawID string, ctx pipelineContext, reason string) {
	clear := func() {
		_, _ = s.execStatusLogged("clear stop comment claw "+clawID, `UPDATE claws SET stop_comment_pending=0 WHERE id=? AND stop_comment_pending=2`, clawID)
	}
	retry := func() {
		_, _ = s.execStatusLogged("retry stop comment claw "+clawID, `UPDATE claws SET stop_comment_pending=1 WHERE id=? AND stop_comment_pending=2`, clawID)
		// Keep retries spaced by the redrive grace period rather than attempting
		// a permanently failing tracker on every reaper tick.
		s.firstSeen("comment:"+clawID, true, s.reaperNow())
	}
	if ctx.Workflow == nil || ctx.Workspace == nil || ctx.IssueID == "" {
		clear()
		return
	}
	feedback := agentFailureFeedback{
		Integration:  ctx.Workflow.Integration,
		IssueID:      ctx.IssueID,
		TriggerActor: s.triggerActorForClaw(clawID),
		Failure:      classifyAgentFailure(reason),
		ClawID:       clawID,
	}
	switch ctx.Workflow.Integration {
	case "github-issues":
		repo, issueNum, ok := parseGitHubIssueWorkflowID(ctx.IssueID)
		if !ok {
			log.Printf("[stopAgent] invalid GitHub workflow issue id %q for claw %s", ctx.IssueID, shortID(clawID))
			clear()
			return
		}
		token := s.resolveGitHubIssuesTokenForWorkflow(ctx.Workspace.Name, ctx.Workflow)
		if token == "" {
			clear()
			return
		}
		feedback.GitHubRepo = repo
		feedback.GitHubIssueNum = issueNum
		if ctx.Workflow.Trigger != nil && ctx.Workflow.Trigger.GitHubIssues != nil {
			feedback.AgentStatusError = strings.TrimSpace(ctx.Workflow.Trigger.GitHubIssues.AgentStatusError)
		}
		if s.handleAgentFailureFeedback(feedback, token) {
			clear()
		} else {
			retry()
		}
	case "linear":
		token := s.resolveLinearTokenForWorkflow(ctx.Workspace.Name, ctx.Workflow)
		if token == "" {
			clear()
			return
		}
		feedback.LinearIdentifier = ctx.IssueID
		if ctx.Workflow.Trigger != nil && ctx.Workflow.Trigger.Linear != nil {
			feedback.AgentStatusError = strings.TrimSpace(ctx.Workflow.Trigger.Linear.AgentStatusError)
		}
		if s.handleAgentFailureFeedback(feedback, token) {
			clear()
		} else {
			retry()
		}
	case "shortcut":
		token := s.resolveShortcutTokenForWorkflow(ctx.Workspace.Name, ctx.Workflow)
		if token == "" {
			clear()
			return
		}
		if ctx.Workflow.Trigger != nil && ctx.Workflow.Trigger.Shortcut != nil {
			feedback.AgentStatusError = strings.TrimSpace(ctx.Workflow.Trigger.Shortcut.AgentStatusError)
		}
		if s.handleAgentFailureFeedback(feedback, token) {
			clear()
		} else {
			retry()
		}
	case "jira":
		tracker, ok := s.resolveJiraTrackerForWorkflow(ctx.Workspace.Name, ctx.Workflow)
		if !ok || tracker.Token == "" {
			clear()
			return
		}
		if ctx.Workflow.Trigger != nil && ctx.Workflow.Trigger.Jira != nil {
			feedback.AgentStatusError = strings.TrimSpace(ctx.Workflow.Trigger.Jira.AgentStatusError)
		}
		feedback.JiraTracker = tracker
		if s.handleAgentFailureFeedback(feedback, tracker.Token) {
			clear()
		} else {
			retry()
		}
	}
}

func parseGitHubIssueWorkflowID(issueID string) (string, int, bool) {
	lastSlash := strings.LastIndex(issueID, "/")
	if lastSlash <= 0 || lastSlash == len(issueID)-1 {
		return "", 0, false
	}
	repo := issueID[:lastSlash]
	var issueNum int
	if _, err := fmt.Sscanf(issueID[lastSlash+1:], "%d", &issueNum); err != nil || issueNum <= 0 {
		return "", 0, false
	}
	return repo, issueNum, true
}

func (s *Server) commentAgentStopToTracker(clawID string, factory *types.FactoryConfig, issueID, reason string) {
	clear := func() {
		_, _ = s.execStatusLogged("clear stop comment claw "+clawID, `UPDATE claws SET stop_comment_pending=0 WHERE id=? AND stop_comment_pending=2`, clawID)
	}
	retry := func() {
		_, _ = s.execStatusLogged("retry stop comment claw "+clawID, `UPDATE claws SET stop_comment_pending=1 WHERE id=? AND stop_comment_pending=2`, clawID)
		s.firstSeen("comment:"+clawID, true, s.reaperNow())
	}
	var commentBody string
	getCommentBody := func() string {
		if commentBody == "" {
			commentBody = s.buildAgentStopComment(clawID, reason)
		}
		return commentBody
	}

	success := false
	switch factory.Integration {
	case "linear":
		token := s.resolveLinearTokenForFactory(factory)
		if token != "" {
			if factory.AgentStatusError != "" {
				if err := s.retryTrackerMove("move Linear issue", func() error { return s.moveLinearIssueOnServer(token, issueID, factory.AgentStatusError) }); err != nil {
					log.Printf("[stopAgent] failed to move Linear issue %s: %v", issueID, err)
				}
			}
			if err := s.commentLinearIssue(token, issueID, getCommentBody()); err != nil {
				log.Printf("[stopAgent] failed to comment Linear issue %s: %v", issueID, err)
			} else {
				log.Printf("[stopAgent] commented Linear issue %s", issueID)
				success = true
			}
		}
	case "shortcut":
		token := s.resolveShortcutToken(factory.Workspace)
		if token != "" {
			if factory.AgentStatusError != "" {
				if err := s.retryTrackerMove("move Shortcut story", func() error {
					return moveShortcutStory(s.resolveShortcutBaseURL(), token, issueID, factory.AgentStatusError)
				}); err != nil {
					log.Printf("[stopAgent] failed to move Shortcut story %s: %v", issueID, err)
				}
			}
			if err := commentShortcutIssue(s.resolveShortcutBaseURL(), token, issueID, getCommentBody()); err != nil {
				log.Printf("[stopAgent] failed to comment Shortcut story %s: %v", issueID, err)
			} else {
				log.Printf("[stopAgent] commented Shortcut story %s", issueID)
				success = true
			}
		}
	case "github-issues":
		parts := strings.Split(issueID, "/")
		if len(parts) == 3 {
			token := s.resolveGitHubIssuesTokenForFactory(factory)
			if token != "" {
				repo := parts[0] + "/" + parts[1]
				var issueNum int
				if _, err := fmt.Sscanf(parts[2], "%d", &issueNum); err == nil {
					if factory.AgentStatusError != "" {
						base := s.githubBaseURL
						if base == "" {
							base = "https://api.github.com"
						}
						if err := s.retryTrackerMove("mark GitHub issue", func() error { return githubAPIAddLabel(base, repo, issueNum, factory.AgentStatusError, token) }); err != nil {
							log.Printf("[stopAgent] failed to mark GitHub issue %s: %v", issueID, err)
						}
					}
					if err := commentGitHubIssue(token, repo, issueNum, getCommentBody()); err != nil {
						log.Printf("[stopAgent] failed to comment GitHub issue %s: %v", issueID, err)
					} else {
						log.Printf("[stopAgent] commented GitHub issue %s", issueID)
						success = true
					}
				}
			}
		}
	case "jira":
		if tracker, ok := s.resolveJiraTrackerForFactory(factory); ok {
			if factory.AgentStatusError != "" {
				if err := s.retryTrackerMove("move Jira issue", func() error { return s.moveJiraIssue(tracker, issueID, factory.AgentStatusError) }); err != nil {
					log.Printf("[stopAgent] failed to move Jira issue %s: %v", issueID, err)
				}
			}
			if err := s.commentJiraIssue(tracker, issueID, getCommentBody()); err != nil {
				log.Printf("[stopAgent] failed to comment Jira issue %s: %v", issueID, err)
			} else {
				log.Printf("[stopAgent] commented Jira issue %s", issueID)
				success = true
			}
		}
	}
	if success {
		clear()
	} else {
		retry()
	}
}

// findFactoryForClaw looks up the factory that created a claw by its claw ID.
// It uses the factory:<name> tag stored on the claw to identify the factory.
func (s *Server) findFactoryForClaw(clawID string) (*types.FactoryConfig, string) {
	var issueID, githubIssueID, shortcutStoryID, jiraIssueID, tagsJSON string
	if err := s.db.QueryRow(`SELECT COALESCE(linear_issue_id,''), COALESCE(github_issue_id,''), COALESCE(shortcut_story_id,''), COALESCE(jira_issue_id,''), COALESCE(tags,'[]') FROM claws WHERE id=?`, clawID).Scan(&issueID, &githubIssueID, &shortcutStoryID, &jiraIssueID, &tagsJSON); err != nil {
		return nil, ""
	}

	// Prefer provider-specific IDs for non-Linear issue-based claws.
	if githubIssueID != "" {
		issueID = githubIssueID
	} else if shortcutStoryID != "" {
		issueID = shortcutStoryID
	} else if jiraIssueID != "" {
		issueID = jiraIssueID
	}

	if issueID != "" {
		if factory := s.findFactoryForIssue(issueID); factory != nil {
			return factory, issueID
		}
	}

	var tags []string
	if err := json.Unmarshal([]byte(tagsJSON), &tags); err != nil {
		return nil, issueID
	}
	for _, tag := range tags {
		if !strings.HasPrefix(tag, "factory:") {
			continue
		}
		factoryName := strings.TrimPrefix(tag, "factory:")
		for _, factory := range s.resolveFactories() {
			if factory.Name == factoryName {
				return factory, issueID
			}
		}
	}
	return nil, issueID
}

func (s *Server) findPipelineContextForClaw(clawID string) (pipelineContext, bool) {
	issueID, tags := s.clawIssueAndTags(clawID)
	if workspaceName, workflowName := workflowTags(tags); workspaceName != "" && workflowName != "" {
		workspace, workflow, ok := loadWorkflowPipelineContext(workspaceName, workflowName)
		if ok {
			return pipelineContext{Workspace: workspace, Workflow: workflow, IssueID: issueID}, true
		}
	}
	for _, tag := range tags {
		if !strings.HasPrefix(tag, "factory:") {
			continue
		}
		factoryName := strings.TrimPrefix(tag, "factory:")
		for _, factory := range s.resolveFactories() {
			if factory.Name == factoryName {
				return pipelineContext{Factory: factory, IssueID: issueID}, true
			}
		}
	}
	return pipelineContext{IssueID: issueID}, false
}

func (s *Server) findPipelineContextForIssue(issueID string) (pipelineContext, bool) {
	var clawID string
	queries := []string{
		`SELECT id FROM claws WHERE linear_issue_id=? AND status NOT IN ('error','deleted') ORDER BY created_at DESC LIMIT 1`,
		`SELECT id FROM claws WHERE github_issue_id=? AND status NOT IN ('error','deleted') ORDER BY created_at DESC LIMIT 1`,
		`SELECT id FROM claws WHERE shortcut_story_id=? AND status NOT IN ('error','deleted') ORDER BY created_at DESC LIMIT 1`,
		`SELECT id FROM claws WHERE jira_issue_id=? AND status NOT IN ('error','deleted') ORDER BY created_at DESC LIMIT 1`,
	}
	for _, query := range queries {
		if err := s.db.QueryRow(query, issueID).Scan(&clawID); err == nil && clawID != "" {
			return s.findPipelineContextForClaw(clawID)
		}
	}
	return pipelineContext{IssueID: issueID}, false
}

func (s *Server) clawIssueAndTags(clawID string) (string, []string) {
	var issueID, githubIssueID, shortcutStoryID, jiraIssueID, tagsJSON string
	if err := s.db.QueryRow(`SELECT COALESCE(linear_issue_id,''), COALESCE(github_issue_id,''), COALESCE(shortcut_story_id,''), COALESCE(jira_issue_id,''), COALESCE(tags,'[]') FROM claws WHERE id=?`, clawID).Scan(&issueID, &githubIssueID, &shortcutStoryID, &jiraIssueID, &tagsJSON); err != nil {
		return "", nil
	}
	if githubIssueID != "" {
		issueID = githubIssueID
	} else if shortcutStoryID != "" {
		issueID = shortcutStoryID
	} else if jiraIssueID != "" {
		issueID = jiraIssueID
	}
	var tags []string
	_ = json.Unmarshal([]byte(tagsJSON), &tags)
	return issueID, tags
}

const issueLabelsTemplateFile = "__hub__/ISSUE_LABELS.json"

func (s *Server) loadIssueLabelsForClaw(clawID string) ([]string, bool) {
	var filesJSON string
	if err := s.db.QueryRow(`SELECT COALESCE(template_files,'{}') FROM claws WHERE id=?`, clawID).Scan(&filesJSON); err != nil {
		return nil, false
	}
	var files map[string]string
	if err := json.Unmarshal([]byte(filesJSON), &files); err != nil {
		return nil, false
	}
	raw, ok := files[issueLabelsTemplateFile]
	if !ok {
		return nil, false
	}
	var labels []string
	if err := json.Unmarshal([]byte(raw), &labels); err != nil {
		return nil, false
	}
	return labels, true
}

func workflowTags(tags []string) (string, string) {
	var workspaceName, workflowName string
	for _, tag := range tags {
		switch {
		case strings.HasPrefix(tag, "workspace:"):
			workspaceName = strings.TrimPrefix(tag, "workspace:")
		case strings.HasPrefix(tag, "workflow:"):
			workflowName = strings.TrimPrefix(tag, "workflow:")
		}
	}
	return workspaceName, workflowName
}

func loadWorkflowPipelineContext(workspaceName, workflowName string) (*types.WorkspaceConfig, *types.WorkflowConfig, bool) {
	workspace, err := loadExternalWorkspace(workspaceName)
	if err != nil {
		return nil, nil, false
	}
	for _, workflow := range workspace.Workflows {
		if workflow == nil || !strings.EqualFold(workflow.Name, workflowName) {
			continue
		}
		return workspace, workflow, true
	}
	return nil, nil, false
}

// buildWorkspaceRunCommand returns the shell command string that should be
// executed on the remote claw for a workflow run action.
// When the claw has a workspace flake, it wraps via flake-run so the command
// runs inside devShells.default while preserving cwd and original command semantics.
//
// It fails closed: if the DB query fails or template_files cannot be parsed,
// an error is returned instead of silently falling back to a plain command.
// This ensures that declared devShell tools are never silently dropped for
// flake workspaces (per #526 contract).
func (s *Server) buildWorkspaceRunCommand(clawID, command string) (string, error) {
	var tmplFilesJSON string
	if err := s.db.QueryRow(`SELECT COALESCE(template_files,'{}') FROM claws WHERE id=?`, clawID).Scan(&tmplFilesJSON); err != nil {
		return "", fmt.Errorf("load template_files for claw %s: %w", clawID, err)
	}
	var tmplFiles map[string]string
	if err := json.Unmarshal([]byte(tmplFilesJSON), &tmplFiles); err != nil {
		return "", fmt.Errorf("parse template_files for claw %s: %w", clawID, err)
	}
	hasWorkspaceFlake := tmplFiles["flake.nix"] != ""

	if hasWorkspaceFlake {
		inner := `cd "$HOME/.openclaw/workspace" && ` + command
		quoted := shellQuote(inner)
		return `~/.elasticclaw/flake-run bash -lc ` + quoted, nil
	}
	return `cd "$HOME/.openclaw/workspace" && ` + command, nil
}
