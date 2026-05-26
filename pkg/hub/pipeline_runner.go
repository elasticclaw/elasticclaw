package hub

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
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

	resp, err := http.DefaultClient.Do(req)
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
	resp, err := http.DefaultClient.Do(req)
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
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	respBody, _ := io.ReadAll(resp.Body)
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
	Factory   *types.FactoryConfig
	Workspace *types.WorkspaceConfig
	Workflow  *types.WorkflowConfig
	IssueID   string
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
	timeout := defaultPipelineRunTimeout
	if strings.TrimSpace(action.Timeout) != "" {
		parsed, err := time.ParseDuration(strings.TrimSpace(action.Timeout))
		if err != nil {
			return nil, fmt.Errorf("invalid run timeout %q: %w", action.Timeout, err)
		}
		timeout = parsed
	}

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

	workspaceCommand := `cd "$HOME/.openclaw/workspace" && ` + command
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
	case "replicated":
		if sshHost == "" || sshPort == 0 || sshUser == "" {
			return nil, fmt.Errorf("replicated agent has no SSH connection details")
		}
		return s.executeReplicatedPipelineRun(sshUser, fmt.Sprintf("%s:%d", sshHost, sshPort), workspaceCommand, timeout)
	case "noop":
		return &pipelineRunResult{ExitCode: 0, Stdout: "noop provider skipped workflow command"}, nil
	default:
		return nil, fmt.Errorf("provider %q does not support workflow run actions", providerName)
	}
}

func (s *Server) executeReplicatedPipelineRun(user, host, command string, timeout time.Duration) (*pipelineRunResult, error) {
	output, err := s.sshRunWithTimeout(user, host, command, timeout)
	if err != nil {
		return &pipelineRunResult{ExitCode: 1, Stderr: err.Error(), Stdout: output}, err
	}
	return &pipelineRunResult{ExitCode: 0, Stdout: output}, nil
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

// runOnEnter executes the on_enter actions for a given stage.
//
// - stage.OnEnter.Run: executes a command in the agent workspace
// - stage.OnEnter.Inject: injects a user message into the claw
// - stage.OnEnter.MoveIssue: moves the Linear/Shortcut issue to the named status
//
// issueID is the default issue from the trigger; it can be overridden by
// MoveIssue.IssueID (including template references like {{.Inputs.xxx}}).
func (s *Server) runOnEnter(clawID string, stage pipeline.Stage, ctx pipelineContext) {
	issueID := ctx.IssueID
	if strings.TrimSpace(stage.OnEnter.Run.Command) != "" {
		log.Printf("[pipeline] running workflow command for claw %s stage %q: %s", clawID[:8], stage.ID, stage.OnEnter.Run.Command)
		result, err := s.executePipelineRunAction(clawID, stage.OnEnter.Run)
		if err != nil || (result != nil && result.ExitCode != 0) {
			msg := formatPipelineRunFailure(stage.OnEnter.Run, result, err)
			if stage.OnEnter.Run.ContinueOnError {
				log.Printf("[pipeline] %s; continuing because continue_on_error=true", msg)
				s.injectHubMessageByID(clawID, "[hub] Warning: "+msg)
			} else {
				log.Printf("[pipeline] %s", msg)
				s.injectHubMessageByID(clawID, "[hub] Error: "+msg)
				return
			}
		} else {
			log.Printf("[pipeline] workflow command completed for claw %s stage %q", clawID[:8], stage.ID)
		}
	}

	if stage.OnEnter.Inject != "" {
		injectMsg := stage.OnEnter.Inject

		// Render {{.Issue.Identifier}}, {{.Issue.Title}}, {{.Issue.URL}} if this is a Linear claw
		// GitHub Issues IDs are owner/repo/number format (contain "/"), Shortcut IDs start with "sc-"
		if issueID != "" && !strings.HasPrefix(issueID, "sc-") && !strings.Contains(issueID, "/") {
			log.Printf("[pipeline] attempting to render template for claw %s issue %s", clawID[:8], issueID)
			linearToken := s.resolveLinearTokenForPipeline(ctx)
			if linearToken == "" {
				s.warnPipelineRender(clawID, "%s: no Linear issue tracker token configured; rendering inject with fallback issue context", ctx.Name())
				injectMsg = renderInjectWithData(clawID, injectMsg, struct {
					Issue *linearIssueDetails
				}{Issue: &linearIssueDetails{Identifier: issueID}})
				goto injectMessage
			}
			details, err := s.fetchLinearIssueDetails(linearToken, issueID)
			if err != nil {
				s.warnPipelineRender(clawID, "%s: failed to fetch Linear issue details for %s: %v", ctx.Name(), issueID, err)
				details = &linearIssueDetails{Identifier: issueID}
			}
			if details == nil {
				s.warnPipelineRender(clawID, "%s: Linear issue %s returned no details", ctx.Name(), issueID)
				details = &linearIssueDetails{Identifier: issueID}
			}
			log.Printf("[pipeline] fetched issue %s: identifier=%s title=%s", issueID, details.Identifier, details.Title)
			log.Printf("[pipeline] RAW TEMPLATE for claw %s:\n%s", clawID[:8], injectMsg)
			tmpl, err := template.New("inject").Parse(injectMsg)
			if err != nil {
				s.warnPipelineRender(clawID, "%s: inject template parse failed: %v", ctx.Name(), err)
				goto injectMessage
			}
			var buf bytes.Buffer
			data := struct {
				Issue *linearIssueDetails
			}{
				Issue: details,
			}
			log.Printf("[pipeline] template DATA for claw %s: Issue.Identifier=%q Issue.Title=%q Issue.URL=%q", clawID[:8], data.Issue.Identifier, data.Issue.Title, data.Issue.URL)
			if err := tmpl.Execute(&buf, data); err != nil {
				s.warnPipelineRender(clawID, "%s: inject template execute failed: %v", ctx.Name(), err)
				goto injectMessage
			}
			injectMsg = buf.String()
			log.Printf("[pipeline] template RENDERED for claw %s:\n%s", clawID[:8], injectMsg)
		} else if strings.Contains(issueID, "/") {
			// GitHub issue — fetch details and render with same {{.Issue.*}} variables
			ghToken := s.resolveGitHubIssuesTokenForPipeline(ctx)
			details := fallbackGitHubIssueDetails(issueID)
			if ghToken == "" {
				s.warnPipelineRender(clawID, "%s: no GitHub Issues token configured; rendering inject with fallback issue context", ctx.Name())
				injectMsg = renderInjectWithData(clawID, injectMsg, struct {
					Issue *githubIssueDetails
				}{Issue: details})
				goto injectMessage
			}
			parts := strings.Split(issueID, "/")
			if len(parts) != 3 {
				s.warnPipelineRender(clawID, "%s: invalid GitHub issue ID format %q", ctx.Name(), issueID)
				goto injectMessage
			}
			repo := parts[0] + "/" + parts[1]
			var issueNum int
			if _, err := fmt.Sscanf(parts[2], "%d", &issueNum); err != nil {
				s.warnPipelineRender(clawID, "%s: invalid GitHub issue number in %q: %v", ctx.Name(), issueID, err)
				goto injectMessage
			}
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
			log.Printf("[pipeline] fetched GitHub issue %s: #%s title=%s", issueID, details.Identifier, details.Title)
			tmpl, err := template.New("inject").Parse(injectMsg)
			if err != nil {
				s.warnPipelineRender(clawID, "%s: inject template parse failed: %v", ctx.Name(), err)
				goto injectMessage
			}
			var buf bytes.Buffer
			data := struct {
				Issue *githubIssueDetails
			}{
				Issue: details,
			}
			if err := tmpl.Execute(&buf, data); err != nil {
				s.warnPipelineRender(clawID, "%s: inject template execute failed: %v", ctx.Name(), err)
				goto injectMessage
			}
			injectMsg = buf.String()
			log.Printf("[pipeline] template RENDERED for claw %s:\n%s", clawID[:8], injectMsg)
		} else {
			log.Printf("[pipeline] skipping template render for claw %s: issueID=%q", clawID[:8], issueID)
		}

		// For manual triggers, also try rendering with {{ .Inputs.* }} variables
		// if no issue context was available
		if issueID == "" {
			tmpl, err := template.New("inject").Parse(injectMsg)
			if err == nil {
				var buf bytes.Buffer
				// Load inputs from CONTEXT.md (stored during manual trigger)
				inputs := s.loadManualTriggerInputs(clawID)
				if inputs != nil {
					data := struct {
						Inputs map[string]string
					}{
						Inputs: inputs,
					}
					if err := tmpl.Execute(&buf, data); err == nil {
						injectMsg = buf.String()
						log.Printf("[pipeline] template RENDERED with inputs for claw %s", clawID[:8])
					}
				}
			}
		}

	injectMessage:
		s.injectHubMessageByID(clawID, injectMsg)
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

	targetStatus := stage.OnEnter.MoveIssue.Status
	if targetStatus == "" {
		return
	}

	// If pipeline specifies an explicit issue_id, resolve it from templates or use directly
	resolvedIssueID := issueID
	if stage.OnEnter.MoveIssue.IssueID != "" {
		resolvedIssueID = stage.OnEnter.MoveIssue.IssueID
		// Support template syntax {{.Inputs.xxx}} for manual trigger inputs
		if strings.Contains(resolvedIssueID, "{{.Inputs.") {
			inputs := s.loadManualTriggerInputs(clawID)
			if inputs != nil {
				tmpl, err := template.New("issue_id").Parse(resolvedIssueID)
				if err == nil {
					var buf bytes.Buffer
					data := struct{ Inputs map[string]string }{Inputs: inputs}
					if err := tmpl.Execute(&buf, data); err == nil {
						resolvedIssueID = buf.String()
					}
				}
			}
		}
		// Support template syntax {{.Issue.xxx}} for automatic triggers
		if strings.Contains(resolvedIssueID, "{{.Issue.") {
			var details *linearIssueDetails
			if issueID != "" && !strings.HasPrefix(issueID, "sc-") && !strings.Contains(issueID, "/") {
				linearToken := s.resolveLinearTokenForPipeline(ctx)
				if linearToken != "" {
					d, err := s.fetchLinearIssueDetails(linearToken, issueID)
					if err == nil && d != nil {
						details = d
					}
				}
			} else if strings.Contains(issueID, "/") {
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
							d, err := s.fetchGitHubIssueDetails(ghToken, repo, issueNum, base)
							if err == nil && d != nil {
								var ghDetails githubIssueDetails = *d
								tmpl, err := template.New("issue_id").Parse(resolvedIssueID)
								if err == nil {
									var buf bytes.Buffer
									data := struct{ Issue *githubIssueDetails }{Issue: &ghDetails}
									if err := tmpl.Execute(&buf, data); err == nil {
										resolvedIssueID = buf.String()
									}
								}
								goto issueResolved
							}
						}
					}
				}
			}
			if details != nil {
				tmpl, err := template.New("issue_id").Parse(resolvedIssueID)
				if err == nil {
					var buf bytes.Buffer
					data := struct{ Issue *linearIssueDetails }{Issue: details}
					if err := tmpl.Execute(&buf, data); err == nil {
						resolvedIssueID = buf.String()
					}
				}
			}
		}
	}
issueResolved:
	if resolvedIssueID == "" {
		return
	}

	// Determine issue tracker: explicit workflow/factory integration takes precedence,
	// fall back to ID-format heuristics only when integration is empty.
	var isShortcut, isGitHub bool
	switch ctx.Integration() {
	case "shortcut":
		isShortcut = true
	case "github", "github-issues":
		isGitHub = true
	default:
		isShortcut = strings.HasPrefix(resolvedIssueID, "sc-")
		isGitHub = strings.Contains(resolvedIssueID, "/")
	}

	if isShortcut {
		// Shortcut story — ensure sc- prefix if missing (e.g. template rendered bare number)
		scID := resolvedIssueID
		if !strings.HasPrefix(scID, "sc-") {
			scID = "sc-" + scID
		}
		scToken := s.resolveShortcutTokenForPipeline(ctx)
		if scToken == "" {
			log.Printf("[pipeline] %s: no Shortcut token for connection %q, skipping move_issue", ctx.Name(), ctx.TrackerName())
			return
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
			return
		}
		parts := strings.Split(resolvedIssueID, "/")
		if len(parts) != 3 {
			log.Printf("[pipeline] %s: GitHub issue ID %q is not owner/repo/number format — skipping move_issue", ctx.Name(), resolvedIssueID)
			return
		}
		repo := parts[0] + "/" + parts[1]
		var issueNum int
		if _, err := fmt.Sscanf(parts[2], "%d", &issueNum); err != nil {
			log.Printf("[pipeline] %s: invalid GitHub issue number in %q — skipping move_issue", ctx.Name(), resolvedIssueID)
			return
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
			return
		}
		if err := s.moveLinearIssueOnServer(linearToken, resolvedIssueID, targetStatus); err != nil {
			log.Printf("[pipeline] failed to move issue %s to %q: %v", resolvedIssueID, targetStatus, err)
		} else {
			log.Printf("[pipeline] moved issue %s to %q", resolvedIssueID, targetStatus)
		}
	}
}

// transitionPipelineStage sets the claw's current pipeline stage and runs on_enter.
// If the stage is terminal, it terminates the claw after running on_enter and
// ensuring any injected message is delivered (waits if agent is streaming).
func (s *Server) transitionPipelineStage(clawID string, stage pipeline.Stage, factory *types.FactoryConfig, issueID string) {
	s.transitionPipelineStageWithContext(clawID, stage, pipelineContext{Factory: factory, IssueID: issueID})
}

func (s *Server) transitionPipelineStageWithContext(clawID string, stage pipeline.Stage, ctx pipelineContext) {
	if !s.claimPipelineStageTransition(clawID, stage.ID) {
		log.Printf("[pipeline] claw %s already in stage %q (%s), skipping duplicate transition", clawID[:8], stage.ID, stage.Label)
		return
	}
	log.Printf("[pipeline] claw %s → stage %q (%s)", clawID[:8], stage.ID, stage.Label)
	s.runOnEnter(clawID, stage, ctx)

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

		_, _ = s.db.Exec(`UPDATE claws SET status='deleted' WHERE id=?`, clawID)
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
			go s.terminateVM(provider, providerID)
		}
	}
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

	s.transitionPipelineStageWithContext(clawID, *entry, ctx)
	return strings.TrimSpace(entry.OnEnter.Inject) != ""
}

// stopAgentWithReason is the centralized handler for unexpected agent termination.
// Every path that means "the agent is dead" routes through here.
// It: sets status='error', broadcasts to dashboard, writes issue-tracker comment, terminates VM.
// skipVMTerminate should be true when the caller already knows the VM is gone (e.g. Replicated
// poll saw "terminated") to avoid redundant delete attempts that spam the logs with 404 errors.
func (s *Server) stopAgentWithReason(clawID, reason string, skipVMTerminate bool) {
	s.checkpointBeforeTermination(clawID, "stop-agent")

	// Resolve factory + issueID
	factory, issueID := s.findFactoryForClaw(clawID)
	if factory == nil {
		log.Printf("[stopAgent] claw %s: no factory found, skipping issue tracker comment", clawID[:8])
	}

	// Fetch tenantID + provider info for broadcast + VM cleanup
	var tenantID, providerID, provider string
	_ = s.db.QueryRow(`SELECT tenant_id, COALESCE(provider_id,''), COALESCE(provider,'') FROM claws WHERE id=?`, clawID).Scan(&tenantID, &providerID, &provider)

	// 1. Set terminal status
	_, _ = s.db.Exec(`UPDATE claws SET status='error', bootstrap_status='' WHERE id=? AND status != 'deleted'`, clawID)

	// 2. Broadcast "Agent Stopped" card to dashboard
	safeReason := firstUsefulFailureLines(sanitizeFailureDetails(reason), 4)
	s.broadcastToUsers(tenantID, types.WSMessage{
		Type:    "claw_status",
		Payload: map[string]string{"claw_id": clawID, "status": "error", "reason": safeReason},
	})

	// 3. Disconnect WebSocket if still connected
	s.mu.Lock()
	if cc, ok := s.claws[clawID]; ok {
		cc.conn.Close(1000, "Agent stopped: "+safeReason)
		delete(s.claws, clawID)
	}
	s.mu.Unlock()

	// 4. Write issue-tracker comment without delaying agent shutdown.
	if factory != nil && issueID != "" {
		factoryCopy := *factory
		go s.commentAgentStopToTracker(clawID, &factoryCopy, issueID, reason)
	}

	// 5. Terminate VM if still running
	if providerID != "" && !skipVMTerminate {
		go s.terminateVM(provider, providerID)
	}

	log.Printf("[stopAgent] claw %s stopped: %s", clawID[:8], reason)
}

func (s *Server) commentAgentStopToTracker(clawID string, factory *types.FactoryConfig, issueID, reason string) {
	var commentBody string
	getCommentBody := func() string {
		if commentBody == "" {
			commentBody = s.buildAgentStopComment(clawID, reason)
		}
		return commentBody
	}

	switch factory.Integration {
	case "linear":
		token := s.resolveLinearTokenForFactory(factory)
		if token != "" {
			if err := s.commentLinearIssue(token, issueID, getCommentBody()); err != nil {
				log.Printf("[stopAgent] failed to comment Linear issue %s: %v", issueID, err)
			} else {
				log.Printf("[stopAgent] commented Linear issue %s", issueID)
			}
		}
	case "shortcut":
		token := s.resolveShortcutToken(factory.Workspace)
		if token != "" {
			if err := commentShortcutIssue(s.resolveShortcutBaseURL(), token, issueID, getCommentBody()); err != nil {
				log.Printf("[stopAgent] failed to comment Shortcut story %s: %v", issueID, err)
			} else {
				log.Printf("[stopAgent] commented Shortcut story %s", issueID)
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
					if err := commentGitHubIssue(token, repo, issueNum, getCommentBody()); err != nil {
						log.Printf("[stopAgent] failed to comment GitHub issue %s: %v", issueID, err)
					} else {
						log.Printf("[stopAgent] commented GitHub issue %s", issueID)
					}
				}
			}
		}
	}
}

// findFactoryForClaw looks up the factory that created a claw by its claw ID.
// It uses the factory:<name> tag stored on the claw to identify the factory.
func (s *Server) findFactoryForClaw(clawID string) (*types.FactoryConfig, string) {
	var issueID, githubIssueID, shortcutStoryID, tagsJSON string
	if err := s.db.QueryRow(`SELECT COALESCE(linear_issue_id,''), COALESCE(github_issue_id,''), COALESCE(shortcut_story_id,''), COALESCE(tags,'[]') FROM claws WHERE id=?`, clawID).Scan(&issueID, &githubIssueID, &shortcutStoryID, &tagsJSON); err != nil {
		return nil, ""
	}

	// Prefer github_issue_id for GitHub issue-based claws, then shortcut
	if githubIssueID != "" {
		issueID = githubIssueID
	} else if shortcutStoryID != "" {
		issueID = shortcutStoryID
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
	}
	for _, query := range queries {
		if err := s.db.QueryRow(query, issueID).Scan(&clawID); err == nil && clawID != "" {
			return s.findPipelineContextForClaw(clawID)
		}
	}
	return pipelineContext{IssueID: issueID}, false
}

func (s *Server) clawIssueAndTags(clawID string) (string, []string) {
	var issueID, githubIssueID, shortcutStoryID, tagsJSON string
	if err := s.db.QueryRow(`SELECT COALESCE(linear_issue_id,''), COALESCE(github_issue_id,''), COALESCE(shortcut_story_id,''), COALESCE(tags,'[]') FROM claws WHERE id=?`, clawID).Scan(&issueID, &githubIssueID, &shortcutStoryID, &tagsJSON); err != nil {
		return "", nil
	}
	if githubIssueID != "" {
		issueID = githubIssueID
	} else if shortcutStoryID != "" {
		issueID = shortcutStoryID
	}
	var tags []string
	_ = json.Unmarshal([]byte(tagsJSON), &tags)
	return issueID, tags
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
