package hub

import (
	"bytes"
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
		return nil
	}
	return p
}

// runOnEnter executes the on_enter actions for a given stage.
//
// - stage.OnEnter.Inject: injects a user message into the claw
// - stage.OnEnter.MoveIssue: moves the Linear/Shortcut issue to the named status
//
// factory is required for MoveIssue; if nil the move is skipped silently.
// issueID is the default issue from the factory/webhook; it can be overridden
// by MoveIssue.IssueID (including template references like {{.Inputs.xxx}}).
func (s *Server) runOnEnter(clawID string, stage pipeline.Stage, factory *types.FactoryConfig, issueID string) {
	if stage.OnEnter.Inject != "" {
		injectMsg := stage.OnEnter.Inject

		// Render {{.Issue.Identifier}}, {{.Issue.Title}}, {{.Issue.URL}} if this is a Linear claw
		// GitHub Issues IDs are owner/repo/number format (contain "/"), Shortcut IDs start with "sc-"
		if issueID != "" && !strings.HasPrefix(issueID, "sc-") && !strings.Contains(issueID, "/") {
			log.Printf("[pipeline] attempting to render template for claw %s issue %s", clawID[:8], issueID)
			linearToken := s.resolveLinearTokenForFactory(factory)
			if linearToken == "" {
				log.Printf("[pipeline] no linear token for factory %q, putting claw in error state", factory.Name)
				s.stopAgentWithReason(clawID, fmt.Sprintf("Pipeline template render failed: no linear token for factory %s", factory.Name), false)
				return
			}
			details, err := s.fetchLinearIssueDetails(linearToken, issueID)
			if err != nil {
				log.Printf("[pipeline] fetchLinearIssueDetails FAILED for %s: %v", issueID, err)
				s.stopAgentWithReason(clawID, fmt.Sprintf("Pipeline template render failed: %v", err), false)
				return
			}
			if details == nil {
				log.Printf("[pipeline] fetchLinearIssueDetails returned nil details for %s", issueID)
				s.stopAgentWithReason(clawID, fmt.Sprintf("Pipeline template render failed: issue %s returned no details", issueID), false)
				return
			}
			log.Printf("[pipeline] fetched issue %s: identifier=%s title=%s", issueID, details.Identifier, details.Title)
			log.Printf("[pipeline] RAW TEMPLATE for claw %s:\n%s", clawID[:8], injectMsg)
			tmpl, err := template.New("inject").Parse(injectMsg)
			if err != nil {
				log.Printf("[pipeline] template PARSE FAILED for claw %s: %v", clawID[:8], err)
				s.stopAgentWithReason(clawID, fmt.Sprintf("Pipeline template render failed: %v", err), false)
				return
			}
			var buf bytes.Buffer
			data := struct {
				Issue *linearIssueDetails
			}{
				Issue: details,
			}
			log.Printf("[pipeline] template DATA for claw %s: Issue.Identifier=%q Issue.Title=%q Issue.URL=%q", clawID[:8], data.Issue.Identifier, data.Issue.Title, data.Issue.URL)
			if err := tmpl.Execute(&buf, data); err != nil {
				log.Printf("[pipeline] template EXECUTE FAILED for claw %s: %v", clawID[:8], err)
				s.stopAgentWithReason(clawID, fmt.Sprintf("Pipeline template render failed: %v", err), false)
				return
			}
			injectMsg = buf.String()
			log.Printf("[pipeline] template RENDERED for claw %s:\n%s", clawID[:8], injectMsg)
		} else if strings.Contains(issueID, "/") {
			// GitHub issue — fetch details and render with same {{.Issue.*}} variables
			ghToken := s.resolveGitHubIssuesTokenForFactory(factory)
			if ghToken == "" {
				log.Printf("[pipeline] no GitHub token for factory %q, putting claw in error state", factory.Name)
				s.stopAgentWithReason(clawID, fmt.Sprintf("Pipeline template render failed: no GitHub token for factory %s", factory.Name), false)
				return
			}
			parts := strings.Split(issueID, "/")
			if len(parts) != 3 {
				log.Printf("[pipeline] invalid GitHub issue ID format %q, putting claw in error state", issueID)
				s.stopAgentWithReason(clawID, fmt.Sprintf("Pipeline template render failed: invalid GitHub issue ID format %q", issueID), false)
				return
			}
			repo := parts[0] + "/" + parts[1]
			var issueNum int
			if _, err := fmt.Sscanf(parts[2], "%d", &issueNum); err != nil {
				log.Printf("[pipeline] invalid GitHub issue number in %q: %v, putting claw in error state", issueID, err)
				s.stopAgentWithReason(clawID, fmt.Sprintf("Pipeline template render failed: %v", err), false)
				return
			}
			base := s.githubBaseURL
			if base == "" {
				base = "https://api.github.com"
			}
			details, err := s.fetchGitHubIssueDetails(ghToken, repo, issueNum, base)
			if err != nil {
				log.Printf("[pipeline] fetchGitHubIssueDetails FAILED for %s: %v, putting claw in error state", issueID, err)
				s.stopAgentWithReason(clawID, fmt.Sprintf("Pipeline template render failed: %v", err), false)
				return
			}
			if details == nil {
				log.Printf("[pipeline] fetchGitHubIssueDetails returned nil for %s, putting claw in error state", issueID)
				s.stopAgentWithReason(clawID, fmt.Sprintf("Pipeline template render failed: issue %s returned no details", issueID), false)
				return
			}
			log.Printf("[pipeline] fetched GitHub issue %s: #%s title=%s", issueID, details.Identifier, details.Title)
			tmpl, err := template.New("inject").Parse(injectMsg)
			if err != nil {
				log.Printf("[pipeline] template PARSE FAILED for claw %s: %v, putting claw in error state", clawID[:8], err)
				s.stopAgentWithReason(clawID, fmt.Sprintf("Pipeline template render failed: %v", err), false)
				return
			}
			var buf bytes.Buffer
			data := struct {
				Issue *githubIssueDetails
			}{
				Issue: details,
			}
			if err := tmpl.Execute(&buf, data); err != nil {
				log.Printf("[pipeline] template EXECUTE FAILED for claw %s: %v, putting claw in error state", clawID[:8], err)
				s.stopAgentWithReason(clawID, fmt.Sprintf("Pipeline template render failed: %v", err), false)
				return
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
			ghToken := s.resolveGitHubIssuesTokenForFactory(factory)
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
	if targetStatus == "" || factory == nil {
		return
	}

	// If pipeline specifies an explicit issue_id, resolve it from inputs or use directly
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
	}
	if resolvedIssueID == "" {
		return
	}

	if strings.HasPrefix(resolvedIssueID, "sc-") {
		// Shortcut story
		scToken := s.resolveShortcutToken(factory.Workspace)
		if scToken == "" {
			log.Printf("[pipeline] factory %q: no Shortcut token for workspace %q, skipping move_issue", factory.Name, factory.Workspace)
			return
		}
		if err := moveShortcutStory(scToken, resolvedIssueID, targetStatus); err != nil {
			log.Printf("[pipeline] failed to move story %s to %q: %v", resolvedIssueID, targetStatus, err)
		} else {
			log.Printf("[pipeline] moved story %s to %q", resolvedIssueID, targetStatus)
		}
	} else if strings.Contains(resolvedIssueID, "/") {
		// GitHub issue (owner/repo/number format)
		ghToken := s.resolveGitHubIssuesTokenForFactory(factory)
		if ghToken == "" {
			log.Printf("[pipeline] factory %q: no GitHub token for move_issue, skipping", factory.Name)
			return
		}
		parts := strings.Split(resolvedIssueID, "/")
		if len(parts) == 3 {
			repo := parts[0] + "/" + parts[1]
			var issueNum int
			if _, err := fmt.Sscanf(parts[2], "%d", &issueNum); err == nil {
				if err := moveGitHubIssue(ghToken, repo, issueNum, targetStatus, s.githubBaseURL); err != nil {
					log.Printf("[pipeline] failed to move GitHub issue %s to %q: %v", resolvedIssueID, targetStatus, err)
				} else {
					log.Printf("[pipeline] moved GitHub issue %s to %q", resolvedIssueID, targetStatus)
				}
			}
		}
	} else {
		// Linear issue
		linearToken := s.resolveLinearTokenForFactory(factory)
		if linearToken == "" {
			log.Printf("[pipeline] factory %q: no Linear token for workspace %q, skipping move_issue", factory.Name, factory.Workspace)
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
	s.setPipelineStage(clawID, stage.ID)
	log.Printf("[pipeline] claw %s → stage %q (%s)", clawID[:8], stage.ID, stage.Label)
	s.runOnEnter(clawID, stage, factory, issueID)

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

		_, _ = s.db.Exec(`UPDATE claws SET status='deleted' WHERE id=?`, clawID)
		s.mu.Lock()
		if cc, ok := s.claws[clawID]; ok {
			cc.conn.Close(1000, "factory: pipeline terminal stage")
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

// initializePipelineEntryIfNeeded transitions a factory claw into its entry stage
// exactly once, after the claw is connected and ready.
// Returns true when entry on_enter inject should be used as the initial wake-up.
func (s *Server) initializePipelineEntryIfNeeded(clawID string) bool {
	// Entry runs only once; if a stage is already set we are done.
	if s.getPipelineStage(clawID) != "" {
		return false
	}

	factory, issueID := s.findFactoryForClaw(clawID)
	log.Printf("[pipeline] initializePipelineEntryIfNeeded: claw=%s factory=%v issueID=%q", clawID[:8], factory != nil, issueID)
	if factory == nil {
		return false
	}
	pl := parsePipelineForFactory(factory)
	if pl == nil {
		return false
	}
	entry := pl.EntryStage()
	if entry == nil {
		return false
	}

	s.transitionPipelineStage(clawID, *entry, factory, issueID)
	return strings.TrimSpace(entry.OnEnter.Inject) != ""
}

// stopAgentWithReason is the centralized handler for unexpected agent termination.
// Every path that means "the agent is dead" routes through here.
// It: sets status='error', broadcasts to dashboard, writes issue-tracker comment, terminates VM.
// skipVMTerminate should be true when the caller already knows the VM is gone (e.g. Replicated
// poll saw "terminated") to avoid redundant delete attempts that spam the logs with 404 errors.
func (s *Server) stopAgentWithReason(clawID, reason string, skipVMTerminate bool) {
	// Resolve factory + issueID
	factory, issueID := s.findFactoryForClaw(clawID)
	if factory == nil {
		log.Printf("[stopAgent] claw %s: no factory found, skipping issue tracker comment", clawID[:8])
	}

	// Fetch tenantID + provider info for broadcast + VM cleanup
	var tenantID, providerID, provider string
	_ = s.db.QueryRow(`SELECT tenant_id, COALESCE(provider_id,''), COALESCE(provider,'') FROM claws WHERE id=?`, clawID).Scan(&tenantID, &providerID, &provider)

	// 1. Set terminal status
	_, _ = s.db.Exec(`UPDATE claws SET status='error' WHERE id=? AND status != 'deleted'`, clawID)

	// 2. Broadcast "Agent Stopped" card to dashboard
	s.broadcastToUsers(tenantID, types.WSMessage{
		Type: "claw_status",
		Payload: map[string]string{"claw_id": clawID, "status": "error", "reason": reason},
	})

	// 3. Write issue-tracker comment based on factory integration
	if factory != nil && issueID != "" {
		switch factory.Integration {
		case "linear":
			token := s.resolveLinearTokenForFactory(factory)
			if token != "" {
				if err := s.commentLinearIssue(token, issueID, fmt.Sprintf("Agent stopped: %s", reason)); err != nil {
					log.Printf("[stopAgent] failed to comment Linear issue %s: %v", issueID, err)
				} else {
					log.Printf("[stopAgent] commented Linear issue %s", issueID)
				}
			}
		case "shortcut":
			token := s.resolveShortcutToken(factory.Workspace)
			if token != "" {
				if err := commentShortcutIssue(token, issueID, fmt.Sprintf("Agent stopped: %s", reason)); err != nil {
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
						if err := commentGitHubIssue(token, repo, issueNum, fmt.Sprintf("Agent stopped: %s", reason)); err != nil {
							log.Printf("[stopAgent] failed to comment GitHub issue %s: %v", issueID, err)
						} else {
							log.Printf("[stopAgent] commented GitHub issue %s", issueID)
						}
					}
				}
			}
		}
	}

	// 4. Disconnect WebSocket if still connected
	s.mu.Lock()
	if cc, ok := s.claws[clawID]; ok {
		cc.conn.Close(1000, "Agent stopped: "+reason)
		delete(s.claws, clawID)
	}
	s.mu.Unlock()

	// 5. Terminate VM if still running
	if providerID != "" && !skipVMTerminate {
		go s.terminateVM(provider, providerID)
	}

	log.Printf("[stopAgent] claw %s stopped: %s", clawID[:8], reason)
}

// findFactoryForClaw looks up the factory that created a claw by its claw ID.
// It uses the factory:<name> tag stored on the claw to identify the factory.
func (s *Server) findFactoryForClaw(clawID string) (*types.FactoryConfig, string) {
	var issueID, githubIssueID, tagsJSON string
	if err := s.db.QueryRow(`SELECT COALESCE(linear_issue_id,''), COALESCE(github_issue_id,''), COALESCE(tags,'[]') FROM claws WHERE id=?`, clawID).Scan(&issueID, &githubIssueID, &tagsJSON); err != nil {
		return nil, ""
	}

	// Prefer github_issue_id for GitHub issue-based claws
	if githubIssueID != "" {
		issueID = githubIssueID
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
