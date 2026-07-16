package hub

import (
	"bytes"
	"crypto/subtle"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/elasticclaw/elasticclaw/pkg/types"
)

type jiraString string

const jiraSearchMaxPages = 1000

func (s *jiraString) UnmarshalJSON(data []byte) error {
	if bytes.Equal(data, []byte("null")) {
		*s = ""
		return nil
	}
	var text string
	if err := json.Unmarshal(data, &text); err == nil {
		*s = jiraString(text)
		return nil
	}
	var number json.Number
	if err := json.Unmarshal(data, &number); err == nil {
		*s = jiraString(number.String())
		return nil
	}
	return fmt.Errorf("invalid Jira string value %s", string(data))
}

type jiraWebhookPayload struct {
	WebhookEvent string                `json:"webhookEvent"`
	Timestamp    int64                 `json:"timestamp"`
	Issue        jiraIssue             `json:"issue"`
	User         jiraUser              `json:"user"`
	Changelog    *jiraWebhookChangelog `json:"changelog,omitempty"`
}

type jiraWebhookChangelog struct {
	ID    string              `json:"id"`
	Items []jiraChangelogItem `json:"items"`
}

type jiraChangelogItem struct {
	Field      string `json:"field"`
	FromString string `json:"fromString"`
	ToString   string `json:"toString"`
}

type jiraAutomationIssuePayload struct {
	jiraIssue
	Changelog *struct {
		Histories []jiraAutomationHistory `json:"histories"`
	} `json:"changelog,omitempty"`
}

type jiraAutomationHistory struct {
	ID    jiraString          `json:"id"`
	Items []jiraChangelogItem `json:"items"`
}

type jiraIssue struct {
	ID     jiraString `json:"id"`
	Key    string     `json:"key"`
	Self   string     `json:"self"`
	Fields struct {
		Summary     string     `json:"summary"`
		Description any        `json:"description"`
		Labels      []string   `json:"labels"`
		Updated     jiraString `json:"updated"`
		Status      struct {
			Name string `json:"name"`
		} `json:"status"`
		Project struct {
			Key  string `json:"key"`
			Name string `json:"name"`
		} `json:"project"`
		Assignee *jiraUser `json:"assignee"`
	} `json:"fields"`
}

type jiraUser struct {
	AccountID    string `json:"accountId"`
	Name         string `json:"name"`
	Key          string `json:"key"`
	DisplayName  string `json:"displayName"`
	EmailAddress string `json:"emailAddress"`
}

type jiraPollIssue = jiraIssue

func (s *Server) handleJiraWebhook(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	workspaceName := strings.TrimSpace(r.PathValue("workspace"))

	body, err := io.ReadAll(io.LimitReader(r.Body, 1<<20))
	if err != nil {
		http.Error(w, "read error", http.StatusBadRequest)
		return
	}
	if !s.validateJiraWebhookSecret(workspaceName, r) {
		http.Error(w, "invalid signature", http.StatusUnauthorized)
		return
	}

	payload, err := parseJiraWebhookPayload(body)
	if err != nil {
		http.Error(w, "invalid payload", http.StatusBadRequest)
		return
	}
	if payload.Issue.Key == "" {
		w.WriteHeader(http.StatusOK)
		return
	}

	dedupKey := jiraWebhookDedupKey(payload)
	if dedupKey != "" && s.isDuplicateWebhook(dedupKey) {
		w.WriteHeader(http.StatusOK)
		return
	}

	s.safeGo("jira webhook", func() { s.processJiraEvent(workspaceName, payload) })
	w.WriteHeader(http.StatusOK)
}

func parseJiraWebhookPayload(body []byte) (jiraWebhookPayload, error) {
	var payload jiraWebhookPayload
	if err := json.Unmarshal(body, &payload); err != nil {
		return payload, err
	}
	if payload.Issue.Key != "" {
		return payload, nil
	}

	var issuePayload jiraAutomationIssuePayload
	if err := json.Unmarshal(body, &issuePayload); err != nil {
		return payload, err
	}
	if issuePayload.Key == "" {
		return payload, nil
	}

	payload.WebhookEvent = "jira:issue_updated"
	payload.Timestamp = time.Now().UnixMilli()
	payload.Issue = issuePayload.jiraIssue
	if issuePayload.Changelog != nil && len(issuePayload.Changelog.Histories) > 0 {
		history := selectJiraAutomationHistory(issuePayload.Changelog.Histories)
		payload.Changelog = &jiraWebhookChangelog{
			ID:    string(history.ID),
			Items: history.Items,
		}
	} else {
		payload.Changelog = &jiraWebhookChangelog{
			ID: "automation:" + firstNonEmpty(string(issuePayload.ID), issuePayload.Key) + ":" + string(issuePayload.Fields.Updated),
		}
	}
	return payload, nil
}

func selectJiraAutomationHistory(histories []jiraAutomationHistory) jiraAutomationHistory {
	for i := len(histories) - 1; i >= 0; i-- {
		for _, item := range histories[i].Items {
			if strings.EqualFold(item.Field, "status") {
				return histories[i]
			}
		}
	}
	return histories[len(histories)-1]
}

func (s *Server) validateJiraWebhookSecret(workspaceName string, r *http.Request) bool {
	secrets := workspaceIssueTrackerWebhookSecrets(workspaceName, "jira")
	if workspaceName == "" {
		secrets = append(secrets, s.globalJiraWebhookSecrets()...)
	}
	if len(secrets) == 0 {
		return true
	}
	got := r.Header.Get("X-ElasticClaw-Webhook-Secret")
	if got == "" {
		got = r.URL.Query().Get("secret")
	}
	for _, secret := range secrets {
		if constantTimeStringEqual(got, secret) {
			return true
		}
	}
	return false
}

func (s *Server) globalJiraWebhookSecrets() []string {
	s.mu.RLock()
	integrations := s.hubCfg.Integrations
	secretRefs := s.hubCfg.Secrets
	s.mu.RUnlock()

	var secrets []string
	if integrations != nil {
		for _, ji := range integrations.Jira {
			if ji.WebhookSecret != "" {
				secrets = append(secrets, ji.WebhookSecret)
			}
		}
	}

	for _, factory := range s.resolveFactories() {
		if factory.Integration != "jira" {
			continue
		}
		secret := factory.WebhookSecret
		if secret == "" && factory.WebhookSecretRef != "" && secretRefs != nil {
			secret = secretRefs[factory.WebhookSecretRef]
		}
		if secret != "" {
			secrets = append(secrets, secret)
		}
	}

	workspaces, err := loadExternalWorkflowsByIntegration("jira")
	if err != nil {
		return secrets
	}
	for _, workspace := range workspaces {
		secrets = append(secrets, workspaceIssueTrackerWebhookSecrets(workspace.Name, "jira")...)
		for _, secretRef := range workspace.WebhookSecrets {
			if secretRef == "" || secretRefs == nil {
				continue
			}
			if secret := secretRefs[secretRef]; secret != "" {
				secrets = append(secrets, secret)
			}
		}
	}
	return secrets
}

func constantTimeStringEqual(a, b string) bool {
	if a == "" || b == "" || len(a) != len(b) {
		return false
	}
	return subtle.ConstantTimeCompare([]byte(a), []byte(b)) == 1
}

func jiraWebhookDedupKey(payload jiraWebhookPayload) string {
	key := strings.TrimSpace(payload.Issue.Key)
	if key == "" {
		return ""
	}
	if payload.Changelog != nil && payload.Changelog.ID != "" {
		return "jira:" + key + ":" + payload.Changelog.ID
	}
	if payload.Timestamp > 0 {
		return fmt.Sprintf("jira:%s:%d", key, payload.Timestamp)
	}
	return ""
}

func (s *Server) processJiraEvent(workspaceName string, payload jiraWebhookPayload) {
	workspaces, err := loadExternalWorkflowsByIntegration("jira")
	if err != nil {
		log.Printf("[jira-webhook] failed to load workflows: %v", err)
		return
	}
	workspaces = filterWorkflowWorkspacesByName(workspaces, workspaceName)
	_ = s.processJiraWorkflowEvent(workspaces, payload, "jira webhook")

	factories := s.resolveFactories()
	if len(factories) == 0 {
		return
	}

	currentStatus, previousStatus := jiraWebhookStatuses(payload)
	labels := jiraIssueLabels(payload.Issue)
	assignee := jiraIssueAssignee(payload.Issue)
	for _, factory := range factories {
		if factory.Integration != "jira" {
			continue
		}
		if factory.Enabled != nil && !*factory.Enabled {
			continue
		}
		if !jiraProjectMatches(payload.Issue.Fields.Project.Key, factory.Projects) {
			continue
		}
		if factory.TriggerStatus == "" || !strings.EqualFold(currentStatus, factory.TriggerStatus) || strings.EqualFold(previousStatus, factory.TriggerStatus) {
			continue
		}
		if !labelsAllowed(labels, factory.Labels, factory.ExcludeLabels) || !s.assigneeMatches(assignee, factory.AssignedTo) {
			continue
		}
		if err := s.createClawForJiraIssue(factory, payload, "jira webhook"); err != nil {
			if isFactoryTriggerAlreadyClaimed(err) {
				continue
			}
			log.Printf("[factory:%s] failed to create claw for Jira issue %s: %v", factory.Name, payload.Issue.Key, err)
			s.trackFactoryCreationFailure(factory.Name, payload.Issue.Key, err.Error())
		}
	}
	for _, factory := range factories {
		if factory.Integration != "jira" || !factory.TerminateOnLeave {
			continue
		}
		if factory.Enabled != nil && !*factory.Enabled {
			continue
		}
		if !jiraProjectMatches(payload.Issue.Fields.Project.Key, factory.Projects) {
			continue
		}
		if factory.TriggerStatus == "" || strings.EqualFold(currentStatus, factory.TriggerStatus) {
			continue
		}
		var activeClaw string
		_ = s.db.QueryRow(`SELECT id FROM claws WHERE jira_issue_id = ? AND status NOT IN ('error','deleted') LIMIT 1`, payload.Issue.Key).Scan(&activeClaw)
		if activeClaw != "" {
			s.terminateClawForIssue(payload.Issue.Key)
		}
	}
}

func (s *Server) processJiraWorkflowEvent(workspaces []*types.WorkspaceConfig, payload jiraWebhookPayload, reason string) bool {
	currentStatus, previousStatus := jiraWebhookStatuses(payload)
	labels := jiraIssueLabels(payload.Issue)
	assignee := jiraIssueAssignee(payload.Issue)
	matched := false
	for _, workspace := range workspaces {
		for _, workflow := range workspace.Workflows {
			if workflow == nil || workflow.Integration != "jira" {
				continue
			}
			if workflow.Enabled != nil && !*workflow.Enabled {
				continue
			}
			if !jiraWorkflowMatchesProject(workflow, payload.Issue.Fields.Project.Key) {
				continue
			}
			if workflow.TriggerStatus == "" || !strings.EqualFold(currentStatus, workflow.TriggerStatus) || strings.EqualFold(previousStatus, workflow.TriggerStatus) {
				continue
			}
			if workflow.AssignedTo != "" && !assignedToMatches(workflow.AssignedTo, assignee) {
				continue
			}
			if !labelsAllowed(labels, workflow.Labels, workflow.ExcludeLabels) {
				continue
			}
			matched = true
			if err := s.createClawForJiraWorkflow(workspace, workflow, payload, reason); err != nil {
				if isFactoryTriggerAlreadyClaimed(err) {
					continue
				}
				log.Printf("[workflow:%s/%s] failed to create claw for Jira issue %s: %v", workspace.Name, workflow.Name, payload.Issue.Key, err)
				tracker, _ := s.resolveJiraTrackerForWorkflow(workspace.Name, workflow)
				s.notifyWorkflowCreateFailure(workspace, workflow, workflowIssueRef{Integration: "jira", IssueID: payload.Issue.Key, JiraTracker: tracker}, triggerActor{}, err)
			}
		}
		for _, workflow := range workspace.Workflows {
			if workflow == nil || workflow.Integration != "jira" || !workflow.TerminateOnLeave {
				continue
			}
			if workflow.Enabled != nil && !*workflow.Enabled {
				continue
			}
			if !jiraWorkflowMatchesProject(workflow, payload.Issue.Fields.Project.Key) {
				continue
			}
			if workflow.TriggerStatus == "" || strings.EqualFold(currentStatus, workflow.TriggerStatus) {
				continue
			}
			var activeClaw string
			_ = s.db.QueryRow(`SELECT id FROM claws WHERE jira_issue_id = ? AND status NOT IN ('error','deleted') LIMIT 1`, payload.Issue.Key).Scan(&activeClaw)
			if activeClaw != "" {
				matched = true
				s.terminateClawForIssue(payload.Issue.Key)
			}
		}
	}
	return matched
}

func jiraWebhookStatuses(payload jiraWebhookPayload) (string, string) {
	current := payload.Issue.Fields.Status.Name
	previous := ""
	if payload.Changelog != nil {
		for _, item := range payload.Changelog.Items {
			if strings.EqualFold(item.Field, "status") {
				if item.ToString != "" {
					current = item.ToString
				}
				previous = item.FromString
				break
			}
		}
	}
	return current, previous
}

func jiraIssueLabels(issue jiraIssue) []string {
	return append([]string(nil), issue.Fields.Labels...)
}

func jiraIssueAssignee(issue jiraIssue) string {
	if issue.Fields.Assignee == nil {
		return ""
	}
	for _, value := range []string{issue.Fields.Assignee.Name, issue.Fields.Assignee.DisplayName, issue.Fields.Assignee.EmailAddress, issue.Fields.Assignee.AccountID} {
		if strings.TrimSpace(value) != "" {
			return value
		}
	}
	return ""
}

func jiraProjectMatches(project string, projects []string) bool {
	if len(projects) == 0 {
		return true
	}
	for _, candidate := range projects {
		if strings.EqualFold(strings.TrimSpace(candidate), project) {
			return true
		}
	}
	return false
}

func jiraWorkflowMatchesProject(workflow *types.WorkflowConfig, project string) bool {
	if workflow == nil {
		return false
	}
	projects := workflow.TriggerRepos
	if workflow.Trigger != nil && workflow.Trigger.Jira != nil && len(workflow.Trigger.Jira.Projects) > 0 {
		projects = workflow.Trigger.Jira.Projects
	}
	return jiraProjectMatches(project, projects)
}

func (s *Server) createClawForJiraWorkflow(workspace *types.WorkspaceConfig, workflow *types.WorkflowConfig, payload jiraWebhookPayload, reason string) error {
	issueID := payload.Issue.Key
	var existing string
	_ = s.db.QueryRow(`SELECT id FROM claws WHERE jira_issue_id=? AND status NOT IN ('error','deleted') LIMIT 1`, issueID).Scan(&existing)
	if existing != "" {
		return nil
	}

	triggerKey := factoryTriggerKey("jira", issueID)
	triggerOwner := fmt.Sprintf("workflow:%s/%s", workspace.Name, workflow.Name)
	claimed, err := s.claimFactoryTrigger(triggerOwner, "jira", triggerKey, reason, map[string]string{
		"issue_id":  issueID,
		"source":    reason,
		"workspace": workspace.Name,
		"workflow":  workflow.Name,
	})
	if err != nil {
		return fmt.Errorf("claim workflow trigger: %w", err)
	}
	if !claimed {
		return errFactoryTriggerAlreadyClaimed
	}
	claimOpen := true
	defer func() {
		if claimOpen {
			s.failFactoryTrigger(triggerOwner, "jira", triggerKey)
		}
	}()

	templateFiles := cloneStringMap(workspace.Files)
	templateFiles["CONTEXT.md"] = s.buildJiraContext(payload)
	clawName := issueID
	if workflow.NamePattern != "" {
		clawName = strings.ReplaceAll(workflow.NamePattern, "{issue_id}", issueID)
		clawName = strings.ReplaceAll(clawName, "{project}", payload.Issue.Fields.Project.Key)
	}
	clawID, isPending, err := s.createClawFromWorkflowWithOptions(workspace, workflow, workflowCreateOptions{
		workspaceFiles:       templateFiles,
		clawName:             clawName,
		jiraIssueID:          issueID,
		issueLabels:          jiraIssueLabels(payload.Issue),
		issueLabelsAvailable: true,
		reason:               reason,
		triggerActor: &triggerActor{
			ID:    firstNonEmpty(payload.User.AccountID, payload.User.Name, payload.User.Key),
			Type:  "User",
			Name:  payload.User.DisplayName,
			Email: payload.User.EmailAddress,
		},
	})
	if err != nil {
		return err
	}
	if err := s.completeFactoryTrigger(triggerOwner, "jira", triggerKey, clawID); err != nil {
		_, _ = s.finishClawTerminalTx(clawID, "deleted", "", "failed", err.Error(), terminalTxOpts{})
		if s.cronScheduler != nil {
			s.cronScheduler.releaseClawWorkflowSlot(clawID)
		}
		return fmt.Errorf("complete workflow trigger: %w", err)
	}
	claimOpen = false
	if !isPending && workflow.WorkingStatus != "" {
		if tracker, ok := s.resolveJiraTrackerForWorkflow(workspace.Name, workflow); ok {
			if err := s.moveJiraIssue(tracker, issueID, workflow.WorkingStatus); err != nil {
				log.Printf("[workflow:%s/%s] failed to move Jira issue %s to %q: %v", workspace.Name, workflow.Name, issueID, workflow.WorkingStatus, err)
			}
		}
	}
	return nil
}

func (s *Server) createClawForJiraIssue(factory *types.FactoryConfig, payload jiraWebhookPayload, reason string) error {
	issueID := payload.Issue.Key
	triggerKey := factoryTriggerKey("jira", issueID)
	claimed, err := s.claimFactoryTrigger(factory.Name, "jira", triggerKey, reason, map[string]string{
		"issue_id": issueID,
		"source":   reason,
	})
	if err != nil {
		return fmt.Errorf("claim factory trigger: %w", err)
	}
	if !claimed {
		return errFactoryTriggerAlreadyClaimed
	}
	claimOpen := true
	defer func() {
		if claimOpen {
			s.failFactoryTrigger(factory.Name, "jira", triggerKey)
		}
	}()
	if tracker, ok := s.resolveJiraTrackerForFactory(factory); ok {
		if _, err := s.fetchJiraIssue(tracker, issueID); err != nil {
			return fmt.Errorf("cannot read Jira issue %s: %w", issueID, err)
		}
	}
	templateFiles, err := s.resolveTemplateFiles(factory.Template)
	if err != nil {
		return err
	}
	templateFiles["CONTEXT.md"] = s.buildJiraContext(payload)
	clawID, isPending, err := s.createClawFromFactory(factory, issueID, nil, templateFiles, reason)
	if err != nil {
		return err
	}
	if err := s.completeFactoryTrigger(factory.Name, "jira", triggerKey, clawID); err != nil {
		_, _ = s.finishClawTerminalTx(clawID, "deleted", "", "failed", err.Error(), terminalTxOpts{})
		if s.cronScheduler != nil {
			s.cronScheduler.releaseClawWorkflowSlot(clawID)
		}
		return fmt.Errorf("complete factory trigger: %w", err)
	}
	claimOpen = false
	if !isPending && factory.WorkingStatus != "" {
		if tracker, ok := s.resolveJiraTrackerForFactory(factory); ok {
			if err := s.moveJiraIssue(tracker, issueID, factory.WorkingStatus); err != nil {
				log.Printf("[factory:%s] failed to move Jira issue %s to %q: %v", factory.Name, issueID, factory.WorkingStatus, err)
			}
		}
	}
	return nil
}

func (s *Server) buildJiraContext(payload jiraWebhookPayload) string {
	issue := payload.Issue
	url := s.jiraBrowseURL(issue)
	var b strings.Builder
	b.WriteString("# Jira Issue Context\n\n")
	fmt.Fprintf(&b, "Issue: %s\n", issue.Key)
	if url != "" {
		fmt.Fprintf(&b, "URL: %s\n", url)
	}
	fmt.Fprintf(&b, "Project: %s\n", issue.Fields.Project.Key)
	fmt.Fprintf(&b, "Status: %s\n", issue.Fields.Status.Name)
	if len(issue.Fields.Labels) > 0 {
		fmt.Fprintf(&b, "Labels: %s\n", strings.Join(issue.Fields.Labels, ", "))
	}
	if assignee := jiraIssueAssignee(issue); assignee != "" {
		fmt.Fprintf(&b, "Assignee: %s\n", assignee)
	}
	fmt.Fprintf(&b, "Title: %s\n\n", issue.Fields.Summary)
	if text := jiraDescriptionText(issue.Fields.Description); text != "" {
		b.WriteString(text)
		b.WriteString("\n")
	}
	return b.String()
}

func (s *Server) jiraBrowseURL(issue jiraIssue) string {
	if issue.Key == "" {
		return ""
	}
	base := ""
	if tracker, ok := s.resolveJiraTrackerByIssue(issue); ok {
		base = tracker.BaseURL
	}
	if base == "" {
		base = s.jiraBaseURL
	}
	if base == "" && issue.Self != "" {
		if u, err := url.Parse(issue.Self); err == nil {
			base = u.Scheme + "://" + u.Host
		}
	}
	if base == "" {
		return ""
	}
	return strings.TrimRight(base, "/") + "/browse/" + url.PathEscape(issue.Key)
}

func jiraDescriptionText(v any) string {
	switch t := v.(type) {
	case string:
		return t
	case nil:
		return ""
	default:
		b, _ := json.MarshalIndent(t, "", "  ")
		return string(b)
	}
}

func (s *Server) resolveJiraTrackerForWorkflow(workspaceName string, workflow *types.WorkflowConfig) (workspaceIssueTracker, bool) {
	if workflow == nil {
		return workspaceIssueTracker{}, false
	}
	return findWorkspaceIssueTracker(workspaceName, "jira", workflow.Workspace)
}

func (s *Server) resolveJiraTrackerForFactory(factory *types.FactoryConfig) (workspaceIssueTracker, bool) {
	if factory == nil {
		return workspaceIssueTracker{}, false
	}
	if s.hubCfg.Integrations != nil {
		for _, ji := range s.hubCfg.Integrations.Jira {
			if factory.Workspace == "" || strings.EqualFold(ji.Workspace, factory.Workspace) {
				return workspaceIssueTracker{BaseURL: ji.BaseURL, Username: ji.Username, Token: ji.Token, WebhookSecret: ji.WebhookSecret}, true
			}
		}
	}
	return workspaceIssueTracker{}, false
}

func (s *Server) resolveJiraTrackerByIssue(issue jiraIssue) (workspaceIssueTracker, bool) {
	if s.hubCfg.Integrations == nil {
		return workspaceIssueTracker{}, false
	}
	if issue.Self != "" {
		if u, err := url.Parse(issue.Self); err == nil {
			issueHost := strings.ToLower(u.Host)
			for _, ji := range s.hubCfg.Integrations.Jira {
				if ji.BaseURL == "" {
					continue
				}
				if bu, err := url.Parse(ji.BaseURL); err == nil && strings.ToLower(bu.Host) == issueHost {
					return workspaceIssueTracker{BaseURL: ji.BaseURL, Username: ji.Username, Token: ji.Token, WebhookSecret: ji.WebhookSecret}, true
				}
			}
		}
	}
	for _, ji := range s.hubCfg.Integrations.Jira {
		if ji.BaseURL != "" {
			return workspaceIssueTracker{BaseURL: ji.BaseURL, Username: ji.Username, Token: ji.Token, WebhookSecret: ji.WebhookSecret}, true
		}
	}
	return workspaceIssueTracker{}, false
}

func (s *Server) jiraBaseForTracker(tracker workspaceIssueTracker) string {
	if tracker.BaseURL != "" {
		return strings.TrimRight(tracker.BaseURL, "/")
	}
	if s.jiraBaseURL != "" {
		return strings.TrimRight(s.jiraBaseURL, "/")
	}
	return ""
}

func (s *Server) fetchJiraIssue(tracker workspaceIssueTracker, key string) (jiraIssue, error) {
	var issue jiraIssue
	base := s.jiraBaseForTracker(tracker)
	if base == "" {
		return issue, fmt.Errorf("missing Jira base URL")
	}
	req, err := http.NewRequest(http.MethodGet, base+"/rest/api/2/issue/"+url.PathEscape(key)+"?fields=summary,description,status,labels,assignee,project,updated", nil)
	if err != nil {
		return issue, err
	}
	applyJiraAuth(req, tracker)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return issue, err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode >= 400 {
		return issue, fmt.Errorf("jira API error %d: %s", resp.StatusCode, string(body))
	}
	if err := json.Unmarshal(body, &issue); err != nil {
		return issue, err
	}
	return issue, nil
}

func (s *Server) queryJiraIssues(tracker workspaceIssueTracker, since time.Time, projects []string) ([]jiraPollIssue, error) {
	base := s.jiraBaseForTracker(tracker)
	if base == "" {
		return nil, fmt.Errorf("missing Jira base URL")
	}
	jql := fmt.Sprintf("updated >= %q", since.Format("2006-01-02 15:04"))
	if len(projects) > 0 {
		quoted := make([]string, 0, len(projects))
		for _, project := range projects {
			project = strings.TrimSpace(project)
			if project != "" {
				quoted = append(quoted, fmt.Sprintf("%q", project))
			}
		}
		if len(quoted) > 0 {
			jql = "project in (" + strings.Join(quoted, ",") + ") AND " + jql
		}
	}
	var issues []jiraPollIssue
	var nextPageToken string
	for page := 0; page < jiraSearchMaxPages; page++ {
		requestBody := map[string]any{
			"jql":        jql,
			"maxResults": 100,
			"fields":     []string{"summary", "description", "status", "labels", "assignee", "project", "updated"},
		}
		if nextPageToken != "" {
			requestBody["nextPageToken"] = nextPageToken
		}
		body, _ := json.Marshal(requestBody)
		req, err := http.NewRequest(http.MethodPost, base+"/rest/api/3/search/jql", bytes.NewReader(body))
		if err != nil {
			return nil, err
		}
		req.Header.Set("Content-Type", "application/json")
		applyJiraAuth(req, tracker)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			return nil, err
		}
		respBody, _ := io.ReadAll(resp.Body)
		_ = resp.Body.Close()
		if resp.StatusCode >= 400 {
			return nil, fmt.Errorf("jira API error %d: %s", resp.StatusCode, string(respBody))
		}
		var result struct {
			Issues        []jiraPollIssue `json:"issues"`
			NextPageToken string          `json:"nextPageToken"`
		}
		if err := json.Unmarshal(respBody, &result); err != nil {
			return nil, err
		}
		issues = append(issues, result.Issues...)
		nextPageToken = result.NextPageToken
		if nextPageToken == "" {
			return issues, nil
		}
	}
	return nil, fmt.Errorf("jira search exceeded %d pages", jiraSearchMaxPages)
}

func (s *Server) moveJiraIssue(tracker workspaceIssueTracker, key, targetStatus string) error {
	base := s.jiraBaseForTracker(tracker)
	if base == "" {
		return fmt.Errorf("missing Jira base URL")
	}
	req, err := http.NewRequest(http.MethodGet, base+"/rest/api/2/issue/"+url.PathEscape(key)+"/transitions", nil)
	if err != nil {
		return err
	}
	applyJiraAuth(req, tracker)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return err
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode >= 400 {
		return fmt.Errorf("jira transitions API error %d: %s", resp.StatusCode, string(body))
	}
	var result struct {
		Transitions []struct {
			ID   string `json:"id"`
			Name string `json:"name"`
			To   struct {
				Name string `json:"name"`
			} `json:"to"`
		} `json:"transitions"`
	}
	if err := json.Unmarshal(body, &result); err != nil {
		return err
	}
	transitionID := ""
	for _, transition := range result.Transitions {
		if strings.EqualFold(transition.Name, targetStatus) || strings.EqualFold(transition.To.Name, targetStatus) {
			transitionID = transition.ID
			break
		}
	}
	if transitionID == "" {
		return fmt.Errorf("no Jira transition to status %q", targetStatus)
	}
	payload, _ := json.Marshal(map[string]any{"transition": map[string]string{"id": transitionID}})
	req, err = http.NewRequest(http.MethodPost, base+"/rest/api/2/issue/"+url.PathEscape(key)+"/transitions", bytes.NewReader(payload))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	applyJiraAuth(req, tracker)
	resp, err = http.DefaultClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	body, _ = io.ReadAll(resp.Body)
	if resp.StatusCode >= 400 {
		return fmt.Errorf("jira transition API error %d: %s", resp.StatusCode, string(body))
	}
	return nil
}

func (s *Server) commentJiraIssue(tracker workspaceIssueTracker, key, text string) error {
	base := s.jiraBaseForTracker(tracker)
	if base == "" {
		return fmt.Errorf("missing Jira base URL")
	}
	payload, _ := json.Marshal(map[string]string{"body": text})
	req, err := http.NewRequest(http.MethodPost, base+"/rest/api/2/issue/"+url.PathEscape(key)+"/comment", bytes.NewReader(payload))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	applyJiraAuth(req, tracker)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode >= 400 {
		return fmt.Errorf("jira comment API error %d: %s", resp.StatusCode, string(body))
	}
	return nil
}

func applyJiraAuth(req *http.Request, tracker workspaceIssueTracker) {
	if tracker.Token == "" {
		return
	}
	if tracker.Username != "" {
		raw := tracker.Username + ":" + tracker.Token
		req.Header.Set("Authorization", "Basic "+base64.StdEncoding.EncodeToString([]byte(raw)))
		return
	}
	req.Header.Set("Authorization", "Bearer "+tracker.Token)
}
