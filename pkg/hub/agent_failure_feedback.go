package hub

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

var issueTrackerHTTPClient = &http.Client{Timeout: 30 * time.Second}

type linearGraphQLError struct {
	Message string `json:"message"`
}

func (s *Server) handleAgentFailureFeedback(feedback agentFailureFeedback, token string) {
	if feedback.Failure.UserMessage == "" {
		feedback.Failure = classifyAgentFailure(feedback.Failure.SafeDetail)
	}
	comment := buildAgentFailureFeedbackComment(feedback)

	switch feedback.Integration {
	case "github-issues":
		base := s.githubBaseURL
		if base == "" {
			base = "https://api.github.com"
		}
		if err := commentGitHubIssueWithBase(base, token, feedback.GitHubRepo, feedback.GitHubIssueNum, comment); err != nil {
			logf("[agent-failure-feedback] failed to comment GitHub issue %s#%d: %v", feedback.GitHubRepo, feedback.GitHubIssueNum, err)
		}
		if feedback.AgentStatusError != "" {
			if err := githubAPIAddLabel(base, feedback.GitHubRepo, feedback.GitHubIssueNum, feedback.AgentStatusError, token); err != nil {
				logf("[agent-failure-feedback] failed to mark GitHub issue %s#%d with label %q: %v", feedback.GitHubRepo, feedback.GitHubIssueNum, feedback.AgentStatusError, err)
			}
		}
		if canAssignFailureFeedback(feedback) {
			if err := assignGitHubIssueWithBase(base, token, feedback.GitHubRepo, feedback.GitHubIssueNum, feedback.TriggerActor.Login); err != nil {
				logf("[agent-failure-feedback] failed to assign GitHub issue %s#%d to %q: %v", feedback.GitHubRepo, feedback.GitHubIssueNum, feedback.TriggerActor.Login, err)
			}
		}
	case "linear":
		if err := s.commentLinearIssue(token, feedback.LinearIdentifier, comment); err != nil {
			logf("[agent-failure-feedback] failed to comment Linear issue %s: %v", feedback.LinearIdentifier, err)
		}
		if feedback.AgentStatusError != "" {
			if err := s.moveLinearIssueOnServer(token, feedback.LinearIdentifier, feedback.AgentStatusError); err != nil {
				logf("[agent-failure-feedback] failed to mark Linear issue %s with status %q: %v", feedback.LinearIdentifier, feedback.AgentStatusError, err)
			}
		}
		if canAssignFailureFeedback(feedback) {
			if err := s.assignLinearIssue(token, feedback.LinearIdentifier, feedback.TriggerActor.ID); err != nil {
				logf("[agent-failure-feedback] failed to assign Linear issue %s to %q: %v", feedback.LinearIdentifier, feedback.TriggerActor.ID, err)
			}
		}
	}
}

func (s *Server) triggerActorForClaw(clawID string) triggerActor {
	var actorJSON string
	if err := s.db.QueryRow(`SELECT COALESCE(trigger_actor_json,'') FROM claws WHERE id=?`, clawID).Scan(&actorJSON); err != nil {
		return triggerActor{}
	}
	if strings.TrimSpace(actorJSON) == "" {
		return triggerActor{}
	}
	var actor triggerActor
	if err := json.Unmarshal([]byte(actorJSON), &actor); err != nil {
		logf("[agent-failure-feedback] failed to parse trigger actor for claw %s: %v", shortID(clawID), err)
		return triggerActor{}
	}
	return actor
}

func buildAgentFailureFeedbackComment(feedback agentFailureFeedback) string {
	var actorPrefix string
	if feedback.TriggerActor.Login != "" {
		actorPrefix = "@" + feedback.TriggerActor.Login + " "
	} else if feedback.TriggerActor.Name != "" {
		actor := feedback.TriggerActor.Name
		if feedback.TriggerActor.URL != "" {
			actor = fmt.Sprintf("[%s](%s)", actor, feedback.TriggerActor.URL)
		}
		actorPrefix = actor + " "
	}

	var b strings.Builder
	b.WriteString(fmt.Sprintf("%sElasticClaw could not finish this implementation.\n\n", actorPrefix))
	b.WriteString("The agent hit an error while preparing or running the workspace, so no reliable code change was completed. ")
	b.WriteString(agentFailureFeedbackActionText(feedback))
	b.WriteString("\n\n")
	b.WriteString(fmt.Sprintf("Status code: %d\n", feedback.Failure.StatusCode))
	if feedback.ClawID != "" {
		b.WriteString(fmt.Sprintf("Agent run: %s\n", feedback.ClawID))
	}
	b.WriteString("\nWhat happened:\n")
	b.WriteString(feedback.Failure.UserMessage)
	b.WriteString("\n\nNext step:\n")
	b.WriteString(feedback.Failure.NextStep)
	return clampFailureComment(b.String())
}

func agentFailureFeedbackActionText(feedback agentFailureFeedback) string {
	canMark := strings.TrimSpace(feedback.AgentStatusError) != ""
	canAssign := canAssignFailureFeedback(feedback)
	switch {
	case canMark && canAssign:
		return "I marked this issue for review and assigned it back to you so you can decide whether to retry or update the setup."
	case canMark:
		return "I marked this issue for review so the workflow setup can be checked before retrying."
	case canAssign:
		return "I assigned this issue back to you so you can decide whether to retry or update the setup."
	default:
		return "Please review the workflow setup before retrying."
	}
}

func canAssignFailureFeedback(feedback agentFailureFeedback) bool {
	switch feedback.Integration {
	case "github-issues":
		return feedback.TriggerActor.Login != "" && strings.EqualFold(feedback.TriggerActor.Type, "user")
	case "linear":
		return feedback.TriggerActor.ID != "" && strings.EqualFold(feedback.TriggerActor.Type, "user")
	default:
		return false
	}
}

func assignGitHubIssueWithBase(base, token, repo string, issueNumber int, login string) error {
	_, err := githubAPIPostWithBase(base, fmt.Sprintf("repos/%s/issues/%d", repo, issueNumber), token, http.MethodPatch, map[string][]string{"assignees": []string{login}})
	return err
}

func (s *Server) assignLinearIssue(token, issueIdentifier, assigneeID string) error {
	base := s.linearBaseURL
	if base == "" {
		base = "https://api.linear.app"
	}
	return assignLinearIssueWithBase(base, token, issueIdentifier, assigneeID)
}

func assignLinearIssueWithBase(baseURL, token, issueIdentifier, assigneeID string) error {
	queryBody := map[string]interface{}{
		"query":     "query($id: String!) { issue(id: $id) { id } }",
		"variables": map[string]string{"id": issueIdentifier},
	}
	queryJSON, _ := json.Marshal(queryBody)
	req, err := http.NewRequest(http.MethodPost, baseURL+"/graphql", strings.NewReader(string(queryJSON)))
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", token)
	req.Header.Set("Content-Type", "application/json")
	resp, err := issueTrackerHTTPClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 400 {
		body, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("Linear API error %d: %s", resp.StatusCode, string(body))
	}
	var result struct {
		Data struct {
			Issue struct {
				ID string `json:"id"`
			} `json:"issue"`
		} `json:"data"`
		Errors []linearGraphQLError `json:"errors"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return err
	}
	if err := linearGraphQLErrorsError(result.Errors); err != nil {
		return err
	}
	if result.Data.Issue.ID == "" {
		return fmt.Errorf("issue %s not found", issueIdentifier)
	}

	mutationBody := map[string]interface{}{
		"query": "mutation($id: String!, $assigneeId: String!) { issueUpdate(id: $id, input: { assigneeId: $assigneeId }) { success } }",
		"variables": map[string]string{
			"id":         result.Data.Issue.ID,
			"assigneeId": assigneeID,
		},
	}
	mutationJSON, _ := json.Marshal(mutationBody)
	req2, err := http.NewRequest(http.MethodPost, baseURL+"/graphql", strings.NewReader(string(mutationJSON)))
	if err != nil {
		return err
	}
	req2.Header.Set("Authorization", token)
	req2.Header.Set("Content-Type", "application/json")
	resp2, err := issueTrackerHTTPClient.Do(req2)
	if err != nil {
		return err
	}
	defer resp2.Body.Close()
	body, _ := io.ReadAll(resp2.Body)
	if resp2.StatusCode >= 400 {
		return fmt.Errorf("Linear API error %d: %s", resp2.StatusCode, string(body))
	}
	var mutationResult struct {
		Data struct {
			IssueUpdate struct {
				Success bool `json:"success"`
			} `json:"issueUpdate"`
		} `json:"data"`
		Errors []linearGraphQLError `json:"errors"`
	}
	if err := json.Unmarshal(body, &mutationResult); err != nil {
		return err
	}
	if err := linearGraphQLErrorsError(mutationResult.Errors); err != nil {
		return err
	}
	if !mutationResult.Data.IssueUpdate.Success {
		return fmt.Errorf("issueUpdate returned success=false")
	}
	return nil
}

func linearGraphQLErrorsError(errs []linearGraphQLError) error {
	if len(errs) == 0 {
		return nil
	}
	messages := make([]string, 0, len(errs))
	for _, err := range errs {
		messages = append(messages, err.Message)
	}
	return fmt.Errorf("GraphQL error: %s", strings.Join(messages, "; "))
}
