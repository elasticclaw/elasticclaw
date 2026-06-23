package hub

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/elasticclaw/elasticclaw/pkg/types"
)

// startIntegrationPoller launches the background polling goroutine that queries
// each configured integration's API every 30 seconds to detect missed webhook
// events. It evaluates factory/workflow filters against current state and
// creates agents when the matching trigger has not already claimed that
// external item.
func (s *Server) startIntegrationPoller() {
	go func() {
		ticker := time.NewTicker(30 * time.Second)
		defer ticker.Stop()
		for range ticker.C {
			s.pollTick()
		}
	}()
}

// pollTick queries integration platforms for recently updated items and
// processes any trigger transitions that webhooks may have missed.
func (s *Server) pollTick() {
	now := time.Now().UTC()
	since := now.Add(-2 * time.Minute).Format(time.RFC3339)

	factories := s.resolveFactories()
	linearWorkflowWorkspaces, err := loadExternalWorkflowsByIntegration("linear")
	if err != nil {
		log.Printf("[poll] tick: failed to load linear workflows: %v", err)
	}
	shortcutWorkflowWorkspaces, err := loadExternalWorkflowsByIntegration("shortcut")
	if err != nil {
		log.Printf("[poll] tick: failed to load shortcut workflows: %v", err)
	}
	githubIssueWorkflowWorkspaces, err := loadExternalWorkflowsByIntegration("github-issues")
	if err != nil {
		log.Printf("[poll] tick: failed to load github-issues workflows: %v", err)
	}
	jiraWorkflowWorkspaces, err := loadExternalWorkflowsByIntegration("jira")
	if err != nil {
		log.Printf("[poll] tick: failed to load jira workflows: %v", err)
	}
	if len(factories) == 0 && len(linearWorkflowWorkspaces) == 0 && len(shortcutWorkflowWorkspaces) == 0 && len(githubIssueWorkflowWorkspaces) == 0 && len(jiraWorkflowWorkspaces) == 0 {
		log.Printf("[poll] tick: no factories or workflows configured")
		return
	}

	s.mu.RLock()
	integrations := s.hubCfg.Integrations
	s.mu.RUnlock()

	// === LINEAR ===
	if integrations != nil && len(integrations.Linear) > 0 {
		s.pollLinear(factories, integrations.Linear, since)
	}
	if len(linearWorkflowWorkspaces) > 0 {
		s.pollLinearWorkflows(linearWorkflowWorkspaces, since)
	}

	// === SHORTCUT ===
	if integrations != nil && len(integrations.Shortcut) > 0 {
		s.pollShortcut(factories, integrations.Shortcut, since)
	}
	if len(shortcutWorkflowWorkspaces) > 0 {
		s.pollShortcutWorkflows(shortcutWorkflowWorkspaces, since)
	}

	// === GITHUB ISSUES ===
	if integrations != nil && len(integrations.GitHubIssues) > 0 {
		s.pollGitHubIssues(factories, integrations.GitHubIssues, since)
	}
	if len(githubIssueWorkflowWorkspaces) > 0 {
		s.pollGitHubIssueWorkflows(githubIssueWorkflowWorkspaces, since)
	}

	// === JIRA ===
	if integrations != nil && len(integrations.Jira) > 0 {
		s.pollJira(factories, integrations.Jira, now.Add(-2*time.Minute))
	}
	if len(jiraWorkflowWorkspaces) > 0 {
		s.pollJiraWorkflows(jiraWorkflowWorkspaces, now.Add(-2*time.Minute))
	}

	// === GITHUB PRs ===
	// Use factories with integration=="github" to discover repos
	if len(factories) > 0 {
		s.pollGitHubPRs(factories, since)
	}
}

// ── LINEAR POLLER ───────────────────────────────────────────────────────────

func (s *Server) pollLinear(factories []*types.FactoryConfig, linearCfgs []*types.LinearIntegrationConfig, since string) {
	// Group factories by workspace (team key) for efficient batching
	workspaceFactories := map[string][]*types.FactoryConfig{}
	for _, f := range factories {
		if f.Integration != "linear" {
			continue
		}
		if f.Enabled != nil && !*f.Enabled {
			continue
		}
		ws := f.Workspace
		if ws == "" && f.Team != "" {
			ws = f.Team
		}
		workspaceFactories[ws] = append(workspaceFactories[ws], f)
	}

	for _, li := range linearCfgs {
		if li.Token == "" {
			continue
		}
		ws := li.Workspace
		wsFactories := workspaceFactories[ws]
		// Also include workspace-agnostic factories (empty workspace/team)
		if ws != "" {
			if agnostic, ok := workspaceFactories[""]; ok {
				wsFactories = append(wsFactories, agnostic...)
			}
		}
		if len(wsFactories) == 0 {
			continue
		}

		issues, err := s.queryLinearIssues(li.Token, since)
		if err != nil {
			log.Printf("[poll-linear] query failed for workspace %q: %v", ws, err)
			continue
		}

		for _, issue := range issues {
			s.processLinearPollItem(issue, wsFactories, ws)
		}
	}
}

type linearWorkflowPollTarget struct {
	workspace *types.WorkspaceConfig
	workflow  *types.WorkflowConfig
}

func (s *Server) pollLinearWorkflows(workspaces []*types.WorkspaceConfig, since string) {
	tokenTargets := map[string][]linearWorkflowPollTarget{}
	for _, workspace := range workspaces {
		if workspace == nil {
			continue
		}
		for _, workflow := range workspace.Workflows {
			if workflow == nil || workflow.Integration != "linear" {
				continue
			}
			if workflow.Enabled != nil && !*workflow.Enabled {
				continue
			}
			token := s.resolveLinearTokenForWorkflow(workspace.Name, workflow)
			if token == "" {
				log.Printf("[poll-linear] workflow %s/%s: no Linear token configured — skipping", workspace.Name, workflow.Name)
				continue
			}
			tokenTargets[token] = append(tokenTargets[token], linearWorkflowPollTarget{workspace: workspace, workflow: workflow})
		}
	}
	for token, targets := range tokenTargets {
		issues, err := s.queryLinearIssues(token, since)
		if err != nil {
			log.Printf("[poll-linear] workflow query failed: %v", err)
			continue
		}
		for _, issue := range issues {
			s.processLinearWorkflowPollItem(issue, targets)
		}
	}
}

// linearPollIssue is the subset of Linear GraphQL data we need for polling.
type linearPollIssue struct {
	ID          string `json:"id"`
	Identifier  string `json:"identifier"`
	Title       string `json:"title"`
	Description string `json:"description"`
	URL         string `json:"url"`
	UpdatedAt   string `json:"updatedAt"`
	State       struct {
		Name string `json:"name"`
	} `json:"state"`
	Team struct {
		Key  string `json:"key"`
		Name string `json:"name"`
	} `json:"team"`
	Labels []struct {
		Name string `json:"name"`
	} `json:"labels"`
	Assignee *struct {
		Name string `json:"name"`
	} `json:"assignee"`
}

func (s *Server) queryLinearIssues(token, since string) ([]linearPollIssue, error) {
	base := s.linearBaseURL
	if base == "" {
		base = "https://api.linear.app"
	}

	query := fmt.Sprintf(`query {
		issues(filter: { updatedAt: { gt: "%s" } }) {
			nodes {
				id
				identifier
				title
				description
				url
				updatedAt
				state { name }
				team { key name }
				labels { nodes { name } }
				assignee { name }
			}
		}
	}`, since)

	body := map[string]interface{}{"query": query}
	jsonBody, _ := json.Marshal(body)
	req, _ := http.NewRequest("POST", base+"/graphql", bytes.NewReader(jsonBody))
	req.Header.Set("Authorization", token)
	req.Header.Set("Content-Type", "application/json")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	respBody, _ := io.ReadAll(resp.Body)

	if resp.StatusCode >= 400 {
		return nil, fmt.Errorf("linear API error %d: %s", resp.StatusCode, string(respBody))
	}

	var result struct {
		Data struct {
			Issues struct {
				Nodes []struct {
					ID          string `json:"id"`
					Identifier  string `json:"identifier"`
					Title       string `json:"title"`
					Description string `json:"description"`
					URL         string `json:"url"`
					UpdatedAt   string `json:"updatedAt"`
					State       struct {
						Name string `json:"name"`
					} `json:"state"`
					Team struct {
						Key  string `json:"key"`
						Name string `json:"name"`
					} `json:"team"`
					Labels struct {
						Nodes []struct {
							Name string `json:"name"`
						} `json:"nodes"`
					} `json:"labels"`
					Assignee *struct {
						Name string `json:"name"`
					} `json:"assignee"`
				} `json:"nodes"`
			} `json:"issues"`
		} `json:"data"`
	}
	if err := json.Unmarshal(respBody, &result); err != nil {
		return nil, fmt.Errorf("parse linear response: %w", err)
	}

	var issues []linearPollIssue
	for _, n := range result.Data.Issues.Nodes {
		li := linearPollIssue{
			ID:          n.ID,
			Identifier:  n.Identifier,
			Title:       n.Title,
			Description: n.Description,
			URL:         n.URL,
			UpdatedAt:   n.UpdatedAt,
			State:       n.State,
			Team:        n.Team,
			Assignee:    n.Assignee,
		}
		for _, l := range n.Labels.Nodes {
			label := struct {
				Name string `json:"name"`
			}{Name: l.Name}
			li.Labels = append(li.Labels, label)
		}
		issues = append(issues, li)
	}
	return issues, nil
}

func (s *Server) processLinearPollItem(issue linearPollIssue, factories []*types.FactoryConfig, workspace string) {
	entityID := issue.Identifier
	currentStatus := issue.State.Name

	// Extract labels
	currentLabels := []string{}
	for _, l := range issue.Labels {
		currentLabels = append(currentLabels, l.Name)
	}

	// Extract assignee
	currentAssignee := ""
	if issue.Assignee != nil {
		currentAssignee = issue.Assignee.Name
	}

	// Evaluate factories against current state (no transition gating)
	for _, factory := range factories {
		if !s.factoryMatchesWorkspace(factory, workspace, issue.Team.Key, "linear") {
			continue
		}
		if factory.TriggerStatus == "" {
			continue
		}

		if !strings.EqualFold(currentStatus, factory.TriggerStatus) {
			continue
		}
		if !s.labelsMatch(currentLabels, factory.Labels, factory.ExcludeLabels) {
			continue
		}
		if !s.assigneeMatches(currentAssignee, factory.AssignedTo) {
			continue
		}

		// Build synthetic webhook payload and create claw
		payload := s.buildLinearPollPayload(issue)
		if err := s.createClawForIssue(factory, payload, "poll"); err != nil {
			if isFactoryTriggerAlreadyClaimed(err) {
				continue
			}
			log.Printf("[poll-linear] failed to create claw for %s: %v", entityID, err)
			s.trackFactoryCreationFailure(factory.Name, entityID, err.Error())
		} else {
			log.Printf("[poll-linear] created claw for %s via factory %s", entityID, factory.Name)
			s.logFactoryEvent(factory.Name, entityID, issue.Title, "", currentStatus, "claw_created", "", "poll")
			var clawID string
			_ = s.db.QueryRow(`SELECT id FROM claws WHERE linear_issue_id=? AND factory_name=? ORDER BY created_at DESC LIMIT 1`, entityID, factory.Name).Scan(&clawID)
			if clawID != "" {
				s.trackFactoryCreationSuccess(factory.Name, entityID, clawID)
			}
		}
	}
}

func (s *Server) processLinearWorkflowPollItem(issue linearPollIssue, targets []linearWorkflowPollTarget) {
	entityID := issue.Identifier
	currentStatus := issue.State.Name
	currentLabels := make(map[string]bool)
	for _, l := range issue.Labels {
		currentLabels[strings.ToLower(l.Name)] = true
	}
	currentAssignee := ""
	if issue.Assignee != nil {
		currentAssignee = issue.Assignee.Name
	}
	payload := s.buildLinearPollPayload(issue)

	for _, target := range targets {
		workspace := target.workspace
		workflow := target.workflow
		if workspace == nil || workflow == nil {
			continue
		}
		if workflow.Team != "" && !strings.EqualFold(workflow.Team, issue.Team.Key) {
			continue
		}
		if workflow.TriggerStatus == "" || !strings.EqualFold(currentStatus, workflow.TriggerStatus) {
			continue
		}
		if workflow.AssignedTo != "" && !assignedToMatches(workflow.AssignedTo, currentAssignee) {
			continue
		}
		if !labelSetAllowed(currentLabels, workflow.Labels, workflow.ExcludeLabels) {
			continue
		}

		log.Printf("[workflow:%s/%s] Linear issue %s matched workflow trigger via poll — creating claw", workspace.Name, workflow.Name, entityID)
		if err := s.createClawForLinearWorkflow(workspace, workflow, payload, "poll"); err != nil {
			log.Printf("[workflow:%s/%s] failed to create claw for %s via poll: %v", workspace.Name, workflow.Name, entityID, err)
		}
	}
}

func (s *Server) buildLinearPollPayload(issue linearPollIssue) linearWebhookPayload {
	var payload linearWebhookPayload
	payload.Action = "update"
	payload.Type = "Issue"
	payload.Data.ID = issue.ID
	payload.Data.Identifier = issue.Identifier
	payload.Data.Title = issue.Title
	payload.Data.Description = issue.Description
	payload.Data.URL = issue.URL
	payload.Data.State.Name = issue.State.Name
	payload.Data.Team.Key = issue.Team.Key
	payload.Data.Team.Name = issue.Team.Name
	for _, l := range issue.Labels {
		label := struct {
			Name string `json:"name"`
		}{Name: l.Name}
		payload.Data.Labels = append(payload.Data.Labels, label)
	}
	if issue.Assignee != nil {
		payload.Data.Assignee = &struct {
			Name string `json:"name"`
		}{Name: issue.Assignee.Name}
	}
	return payload
}

// ── SHORTCUT POLLER ─────────────────────────────────────────────────────────

func (s *Server) pollShortcut(factories []*types.FactoryConfig, shortcutCfgs []*types.ShortcutIntegrationConfig, since string) {
	workspaceFactories := map[string][]*types.FactoryConfig{}
	for _, f := range factories {
		if f.Integration != "shortcut" {
			continue
		}
		if f.Enabled != nil && !*f.Enabled {
			continue
		}
		ws := f.Workspace
		workspaceFactories[ws] = append(workspaceFactories[ws], f)
	}

	for _, sc := range shortcutCfgs {
		if sc.Token == "" {
			continue
		}
		ws := sc.Workspace
		wsFactories := workspaceFactories[ws]
		// Also include workspace-agnostic factories (empty workspace)
		if ws != "" {
			if agnostic, ok := workspaceFactories[""]; ok {
				wsFactories = append(wsFactories, agnostic...)
			}
		}
		if len(wsFactories) == 0 {
			continue
		}

		stories, err := s.queryShortcutStories(sc.Token, since)
		if err != nil {
			log.Printf("[poll-shortcut] query failed for workspace %q: %v", ws, err)
			continue
		}

		// Pre-fetch workflow states once per workspace so the poller loop does
		// not repeat the full /workflows API call for every story.
		stateNameMap := buildShortcutStateMap(s.resolveShortcutBaseURL(), sc.Token)
		if len(stateNameMap) == 0 {
			log.Printf("[poll-shortcut] failed to load workflow states for workspace %q — skipping %d stories", ws, len(stories))
			continue
		}
		for _, story := range stories {
			s.processShortcutPollItem(story, wsFactories, ws, sc.Token, stateNameMap)
		}
	}
}

type shortcutPollStory struct {
	ID              int64  `json:"id"`
	Name            string `json:"name"`
	Description     string `json:"description"`
	AppURL          string `json:"app_url"`
	UpdatedAt       string `json:"updated_at"`
	WorkflowStateID int64  `json:"workflow_state_id"`
	Labels          []struct {
		Name string `json:"name"`
	} `json:"labels"`
	OwnerIDs []string `json:"owner_ids"`
}

type shortcutWorkflowPollTarget struct {
	workspace *types.WorkspaceConfig
	workflow  *types.WorkflowConfig
	token     string
}

func (s *Server) queryShortcutStories(token, since string) ([]shortcutPollStory, error) {
	body := map[string]interface{}{
		"updated_at_start": since,
	}
	jsonBody, _ := json.Marshal(body)
	req, _ := http.NewRequest("POST", s.resolveShortcutBaseURL()+"/api/v3/stories/search", bytes.NewReader(jsonBody))
	req.Header.Set("Shortcut-Token", token)
	req.Header.Set("Content-Type", "application/json")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	respBody, _ := io.ReadAll(resp.Body)

	if resp.StatusCode >= 400 {
		return nil, fmt.Errorf("shortcut API error %d: %s", resp.StatusCode, string(respBody))
	}

	var stories []shortcutPollStory
	if err := json.Unmarshal(respBody, &stories); err != nil {
		return nil, fmt.Errorf("parse shortcut response: %w", err)
	}
	return stories, nil
}

func (s *Server) pollShortcutWorkflows(workspaces []*types.WorkspaceConfig, since string) {
	tokenTargets := map[string][]shortcutWorkflowPollTarget{}
	for _, workspace := range workspaces {
		if workspace == nil {
			continue
		}
		for _, workflow := range workspace.Workflows {
			if workflow == nil || workflow.Integration != "shortcut" {
				continue
			}
			if workflow.Enabled != nil && !*workflow.Enabled {
				continue
			}
			token := s.resolveShortcutTokenForWorkflow(workspace.Name, workflow)
			if token == "" {
				log.Printf("[poll-shortcut] workflow %s/%s: no Shortcut token configured — skipping", workspace.Name, workflow.Name)
				continue
			}
			tokenTargets[token] = append(tokenTargets[token], shortcutWorkflowPollTarget{workspace: workspace, workflow: workflow, token: token})
		}
	}
	for token, targets := range tokenTargets {
		stories, err := s.queryShortcutStories(token, since)
		if err != nil {
			log.Printf("[poll-shortcut] workflow query failed: %v", err)
			continue
		}
		stateNameMap := buildShortcutStateMap(s.resolveShortcutBaseURL(), token)
		if len(stateNameMap) == 0 {
			log.Printf("[poll-shortcut] failed to load workflow states for workflow polling — skipping %d stories", len(stories))
			continue
		}
		for _, story := range stories {
			s.processShortcutWorkflowPollItem(story, targets, token, stateNameMap)
		}
	}
}

func (s *Server) processShortcutPollItem(story shortcutPollStory, factories []*types.FactoryConfig, workspace, token string, stateNameMap map[int64]string) {
	storyID := fmt.Sprintf("sc-%d", story.ID)
	currentStateName := stateNameMap[story.WorkflowStateID]
	if currentStateName == "" {
		currentStateName = strconv.FormatInt(story.WorkflowStateID, 10)
	}

	// Extract labels
	currentLabels := []string{}
	for _, l := range story.Labels {
		currentLabels = append(currentLabels, l.Name)
	}

	// For assignee, we need to resolve owner IDs to names. For polling simplicity,
	// we'll fetch the story details if any factory has an AssignedTo filter.
	currentAssignee := ""
	needsAssignee := false
	for _, f := range factories {
		if f.AssignedTo != "" {
			needsAssignee = true
			break
		}
	}
	if needsAssignee && len(story.OwnerIDs) > 0 {
		// Fetch first owner's name for assignee matching
		data, err := shortcutAPI(s.resolveShortcutBaseURL(), fmt.Sprintf("members/%s", story.OwnerIDs[0]), token)
		if err == nil {
			if name, ok := data["mention_name"].(string); ok && name != "" {
				currentAssignee = name
			} else if name, ok := data["name"].(string); ok && name != "" {
				currentAssignee = name
			}
		}
	}

	// Evaluate factories against current state (no transition gating)
	for _, factory := range factories {
		if !s.factoryMatchesWorkspace(factory, workspace, "", "shortcut") {
			continue
		}
		if factory.TriggerStatus == "" {
			continue
		}

		if !strings.EqualFold(currentStateName, factory.TriggerStatus) {
			continue
		}
		if !s.labelsMatch(currentLabels, factory.Labels, factory.ExcludeLabels) {
			continue
		}
		if !s.assigneeMatches(currentAssignee, factory.AssignedTo) {
			continue
		}

		action := shortcutAction{
			ID:          story.ID,
			EntityType:  "story",
			Action:      "update",
			Name:        story.Name,
			AppURL:      story.AppURL,
			Description: story.Description,
		}
		if err := s.createClawForShortcutStory(factory, action, storyID, token, "poll"); err != nil {
			if isFactoryTriggerAlreadyClaimed(err) {
				continue
			}
			log.Printf("[poll-shortcut] failed to create claw for %s: %v", storyID, err)
		} else {
			log.Printf("[poll-shortcut] created claw for %s via factory %s", storyID, factory.Name)
			s.logFactoryEvent(factory.Name, storyID, story.Name, "", currentStateName, "claw_created", "", "poll")
		}
	}
}

func (s *Server) processShortcutWorkflowPollItem(story shortcutPollStory, targets []shortcutWorkflowPollTarget, token string, stateNameMap map[int64]string) {
	storyID := fmt.Sprintf("sc-%d", story.ID)
	currentStateName := stateNameMap[story.WorkflowStateID]
	if currentStateName == "" {
		currentStateName = strconv.FormatInt(story.WorkflowStateID, 10)
	}
	currentLabels := make(map[string]bool)
	for _, l := range story.Labels {
		currentLabels[strings.ToLower(l.Name)] = true
	}
	currentAssignee := ""
	needsAssignee := false
	for _, target := range targets {
		if target.workflow != nil && target.workflow.AssignedTo != "" {
			needsAssignee = true
			break
		}
	}
	if needsAssignee && len(story.OwnerIDs) > 0 {
		data, err := shortcutAPI(s.resolveShortcutBaseURL(), fmt.Sprintf("members/%s", story.OwnerIDs[0]), token)
		if err == nil {
			if name, ok := data["mention_name"].(string); ok && name != "" {
				currentAssignee = name
			} else if name, ok := data["name"].(string); ok && name != "" {
				currentAssignee = name
			}
		}
	}
	action := shortcutAction{
		ID:          story.ID,
		EntityType:  "story",
		Action:      "update",
		Name:        story.Name,
		AppURL:      story.AppURL,
		Description: story.Description,
	}
	for _, target := range targets {
		workspace := target.workspace
		workflow := target.workflow
		if workspace == nil || workflow == nil {
			continue
		}
		if workflow.TriggerStatus == "" || !strings.EqualFold(currentStateName, workflow.TriggerStatus) {
			continue
		}
		if workflow.AssignedTo != "" && !assignedToMatches(workflow.AssignedTo, currentAssignee) {
			continue
		}
		if !labelSetAllowed(currentLabels, workflow.Labels, workflow.ExcludeLabels) {
			continue
		}
		log.Printf("[workflow:%s/%s] Shortcut story %s matched workflow trigger via poll — creating claw", workspace.Name, workflow.Name, storyID)
		if err := s.createClawForShortcutWorkflow(workspace, workflow, action, storyID, "poll"); err != nil {
			log.Printf("[workflow:%s/%s] failed to create claw for %s via poll: %v", workspace.Name, workflow.Name, storyID, err)
		}
	}
}

// ── GITHUB ISSUES POLLER ────────────────────────────────────────────────────

func (s *Server) pollGitHubIssues(factories []*types.FactoryConfig, ghIssueCfgs []*types.GitHubIssuesIntegrationConfig, since string) {
	// Discover repos to poll from factory configs
	repoFactories := map[string][]*types.FactoryConfig{}
	for _, f := range factories {
		if f.Integration != "github-issues" {
			continue
		}
		if f.Enabled != nil && !*f.Enabled {
			continue
		}
		triggerRepos := githubIssuesPollingRepos(f)
		if len(triggerRepos) == 0 {
			log.Printf("[poll-github-issues] factory %q: missing required 'trigger_repos' field — skipping", f.Name)
			continue
		}
		for _, repo := range triggerRepos {
			if repo == "" {
				continue
			}
			if strings.HasSuffix(repo, "/*") {
				log.Printf("[poll-github-issues] factory %q: trigger_repos entry %q is an org wildcard; webhooks can match it, but polling requires exact owner/repo entries", f.Name, repo)
				continue
			}
			repoFactories[repo] = append(repoFactories[repo], f)
		}
	}

	for repo, repoFactories := range repoFactories {
		// Resolve token from first matching factory's workspace
		token := ""
		for _, f := range repoFactories {
			token = s.resolveGitHubIssuesTokenForFactory(f)
			if token != "" {
				break
			}
		}
		if token == "" {
			continue
		}

		base := s.githubBaseURL
		if base == "" {
			base = "https://api.github.com"
		}

		issues, err := s.queryGitHubIssues(repo, token, since, base)
		if err != nil {
			log.Printf("[poll-github-issues] query failed for repo %q: %v", repo, err)
			continue
		}

		for _, issue := range issues {
			s.processGitHubIssuesPollItem(issue, repoFactories, repo, token, base)
		}
	}
}

type githubIssueWorkflowPollTarget struct {
	workspace *types.WorkspaceConfig
	workflow  *types.WorkflowConfig
	token     string
}

func (s *Server) pollGitHubIssueWorkflows(workspaces []*types.WorkspaceConfig, since string) {
	type repoTokenKey struct {
		repo  string
		token string
	}
	repoTargets := map[repoTokenKey][]githubIssueWorkflowPollTarget{}
	for _, workspace := range workspaces {
		if workspace == nil {
			continue
		}
		for _, workflow := range workspace.Workflows {
			if workflow == nil || workflow.Integration != "github-issues" {
				continue
			}
			if workflow.Enabled != nil && !*workflow.Enabled {
				continue
			}
			token := s.resolveGitHubIssuesTokenForWorkflow(workspace.Name, workflow)
			if token == "" {
				log.Printf("[poll-github-issues] workflow %s/%s: no GitHub Issues token configured — skipping", workspace.Name, workflow.Name)
				continue
			}
			for _, repo := range githubIssuesWorkflowTriggerRepos(workflow) {
				if repo == "" {
					continue
				}
				if strings.HasSuffix(repo, "/*") {
					log.Printf("[poll-github-issues] workflow %s/%s: repository %q is an org wildcard; polling requires exact owner/repo entries", workspace.Name, workflow.Name, repo)
					continue
				}
				key := repoTokenKey{repo: repo, token: token}
				repoTargets[key] = append(repoTargets[key], githubIssueWorkflowPollTarget{
					workspace: workspace,
					workflow:  workflow,
					token:     token,
				})
			}
		}
	}

	base := s.githubBaseURL
	if base == "" {
		base = "https://api.github.com"
	}
	for key, targets := range repoTargets {
		if key.token == "" {
			continue
		}
		issues, err := s.queryGitHubIssues(key.repo, key.token, since, base)
		if err != nil {
			log.Printf("[poll-github-issues] workflow query failed for repo %q: %v", key.repo, err)
			continue
		}
		for _, issue := range issues {
			s.processGitHubIssueWorkflowPollItem(issue, targets, key.repo, key.token, base)
		}
	}
}

func githubIssuesPollingRepos(factory *types.FactoryConfig) []string {
	if len(factory.TriggerRepos) > 0 {
		return factory.TriggerRepos
	}
	// Backward compatibility for older GitHub Issues factory configs.
	return factory.Repos
}

type githubIssuesPollItem struct {
	Number    int    `json:"number"`
	Title     string `json:"title"`
	Body      string `json:"body"`
	HTMLURL   string `json:"html_url"`
	State     string `json:"state"`
	UpdatedAt string `json:"updated_at"`
	Labels    []struct {
		Name string `json:"name"`
	} `json:"labels"`
	Assignee *struct {
		Login string `json:"login"`
	} `json:"assignee"`
	User struct {
		Login string `json:"login"`
	} `json:"user"`
	PullRequest *struct{} `json:"pull_request,omitempty"`
}

type githubIssueEvent struct {
	ID        int64  `json:"id"`
	Event     string `json:"event"`
	CreatedAt string `json:"created_at"`
	Actor     struct {
		Login string `json:"login"`
	} `json:"actor"`
	Label *struct {
		Name string `json:"name"`
	} `json:"label,omitempty"`
	Issue *struct {
		Number int `json:"number"`
	} `json:"issue,omitempty"`
}

func (s *Server) queryGitHubIssues(repo, token, since, base string) ([]githubIssuesPollItem, error) {
	items, err := githubAPIListWithBase(base, fmt.Sprintf("repos/%s/issues?since=%s&state=all&sort=updated&direction=desc", repo, since), token)
	if err != nil {
		return nil, err
	}

	var issues []githubIssuesPollItem
	for _, item := range items {
		m, ok := item.(map[string]interface{})
		if !ok {
			continue
		}
		// Skip PRs
		if _, isPR := m["pull_request"]; isPR {
			continue
		}
		var issue githubIssuesPollItem
		b, _ := json.Marshal(m)
		_ = json.Unmarshal(b, &issue)
		issues = append(issues, issue)
	}
	return issues, nil
}

func (s *Server) queryGitHubIssueEvents(repo, token, base string, issueNumber int) ([]githubIssueEvent, error) {
	items, err := githubAPIListWithBase(base, fmt.Sprintf("repos/%s/issues/%d/events", repo, issueNumber), token)
	if err != nil {
		return nil, err
	}

	var events []githubIssueEvent
	for _, item := range items {
		m, ok := item.(map[string]interface{})
		if !ok {
			continue
		}
		var event githubIssueEvent
		b, _ := json.Marshal(m)
		_ = json.Unmarshal(b, &event)
		events = append(events, event)
	}
	return events, nil
}

func (s *Server) processGitHubIssuesPollItem(issue githubIssuesPollItem, factories []*types.FactoryConfig, repo, token, base string) {
	issueID := fmt.Sprintf("%s/%d", repo, issue.Number)
	currentStatus := issue.State

	currentLabels := []string{}
	for _, l := range issue.Labels {
		currentLabels = append(currentLabels, l.Name)
	}

	currentAssignee := ""
	if issue.Assignee != nil {
		currentAssignee = issue.Assignee.Login
	}

	// Fetch per-issue events only if any factory requires AllowedLabelers.
	// The repo-level events endpoint does not support since filtering, so
	// querying per-issue avoids missing events in repos with >100 total events.
	var events []githubIssueEvent
	needsEvents := false
	for _, factory := range factories {
		if factory.Integration == "github-issues" && len(factory.AllowedLabelers) > 0 {
			needsEvents = true
			break
		}
	}
	if needsEvents {
		var err error
		events, err = s.queryGitHubIssueEvents(repo, token, base, issue.Number)
		if err != nil {
			log.Printf("[poll-github-issues] failed to fetch events for %s: %v", issueID, err)
		}
	}

	// Evaluate factories against current state (no transition gating)
	for _, factory := range factories {
		if factory.Integration != "github-issues" {
			continue
		}
		// For GitHub Issues, workspace is just a human label — no token/repo filtering needed here.
		if factory.TriggerStatus == "" {
			continue
		}

		// Check if trigger status matches current state OR a label
		triggerMatched := strings.EqualFold(currentStatus, factory.TriggerStatus)
		for _, l := range currentLabels {
			if strings.EqualFold(l, factory.TriggerStatus) {
				triggerMatched = true
				break
			}
		}
		if !triggerMatched {
			continue
		}

		if !s.labelsMatch(currentLabels, factory.Labels, factory.ExcludeLabels) {
			continue
		}
		if !s.assigneeMatches(currentAssignee, factory.AssignedTo) {
			continue
		}

		// AllowedLabelers check
		if len(factory.AllowedLabelers) > 0 {
			allowed := false
			for _, event := range events {
				// Per-issue events endpoint (repos/{owner}/{repo}/issues/{n}/events)
				// does not include an issue field — all events belong to the queried issue.
				if event.Event != "labeled" {
					continue
				}
				if event.Label == nil {
					continue
				}
				for _, allowedLabeler := range factory.AllowedLabelers {
					if strings.EqualFold(event.Actor.Login, allowedLabeler) {
						allowed = true
						break
					}
				}
				if allowed {
					break
				}
			}
			if !allowed {
				continue
			}
		}

		payload := s.buildGitHubIssuesPollPayload(issue, repo)
		if err := s.createClawForGitHubIssue(factory, payload, "poll"); err != nil {
			if isFactoryTriggerAlreadyClaimed(err) {
				continue
			}
			log.Printf("[poll-github-issues] failed to create claw for %s: %v", issueID, err)
		} else {
			log.Printf("[poll-github-issues] created claw for %s via factory %s", issueID, factory.Name)
			s.logFactoryEvent(factory.Name, issueID, issue.Title, "", currentStatus, "claw_created", "", "poll")
		}
	}
}

func (s *Server) processGitHubIssueWorkflowPollItem(issue githubIssuesPollItem, targets []githubIssueWorkflowPollTarget, repo, token, base string) {
	issueID := fmt.Sprintf("%s/%d", repo, issue.Number)
	currentStatus := issue.State
	issueLabels := githubIssuesPollLabels(issue)
	assignee := ""
	if issue.Assignee != nil {
		assignee = issue.Assignee.Login
	}
	var events []githubIssueEvent
	if githubIssueWorkflowPollNeedsLabelerEvents(targets) {
		var err error
		events, err = s.queryGitHubIssueEvents(repo, token, base, issue.Number)
		if err != nil {
			log.Printf("[poll-github-issues] failed to fetch events for %s: %v", issueID, err)
		}
	}

	for _, target := range targets {
		workspace := target.workspace
		workflow := target.workflow
		if workspace == nil || workflow == nil {
			continue
		}
		payload, ok := buildGitHubIssuesWorkflowPollPayload(issue, repo, workflow, currentStatus, issueLabels, assignee, events)
		if !ok {
			continue
		}
		if _, _, err := s.createClawForGitHubIssueWorkflow(workspace, workflow, payload, "poll"); err != nil {
			if isFactoryTriggerAlreadyClaimed(err) {
				continue
			}
			log.Printf("[workflow:%s/%s] failed to create claw for %s via poll: %v", workspace.Name, workflow.Name, issueID, err)
		}
	}
}

func githubIssuesPollLabels(issue githubIssuesPollItem) map[string]bool {
	labels := map[string]bool{}
	for _, label := range issue.Labels {
		labels[strings.ToLower(label.Name)] = true
	}
	return labels
}

func buildGitHubIssuesWorkflowPollPayload(issue githubIssuesPollItem, repo string, workflow *types.WorkflowConfig, currentStatus string, issueLabels map[string]bool, assignee string, events []githubIssueEvent) (githubIssuesWebhookPayload, bool) {
	payload := buildGitHubIssuesPollPayloadForAction(issue, repo, githubIssuesWorkflowPollAction(workflow))
	if payload.Action == "" {
		return payload, false
	}
	if workflow.AssignedTo != "" && !assignedToMatches(workflow.AssignedTo, assignee) {
		return payload, false
	}
	if payload.Action == "labeled" {
		labelName, ok := githubIssuesWorkflowPollLabel(workflow, issueLabels)
		if !ok {
			return payload, false
		}
		labeler, ok := githubIssuesWorkflowPollLabeler(workflow, labelName, events)
		if !ok {
			return payload, false
		}
		payload.Label = &struct {
			Name string `json:"name"`
		}{Name: labelName}
		if labeler != "" {
			payload.Sender.Login = labeler
		}
	}
	if workflow.Trigger != nil {
		if !githubIssuesWorkflowTriggerMatches(workflow.Trigger, payload, currentStatus, issueLabels) {
			return payload, false
		}
		return payload, true
	}
	if !labelSetAllowed(issueLabels, workflow.Labels, workflow.ExcludeLabels) {
		return payload, false
	}
	triggerStatus := workflow.TriggerStatus
	if triggerStatus == "" {
		triggerStatus = "open"
	}
	if !strings.EqualFold(currentStatus, triggerStatus) && !issueLabels[strings.ToLower(triggerStatus)] {
		return payload, false
	}
	return payload, true
}

func githubIssuesWorkflowPollAction(workflow *types.WorkflowConfig) string {
	event := ""
	if workflow != nil && workflow.Trigger != nil {
		if workflow.Trigger.GitHubIssues != nil {
			event = workflow.Trigger.GitHubIssues.Event
		}
	}
	switch event {
	case "issue_labeled":
		return "labeled"
	case "issue_opened":
		return "opened"
	case "issue_reopened":
		return "reopened"
	case "issue_edited":
		return "edited"
	case "issue_assigned":
		return "assigned"
	case "issue_unassigned":
		return "unassigned"
	case "":
		return "opened"
	default:
		return ""
	}
}

func githubIssuesWorkflowPollLabel(workflow *types.WorkflowConfig, issueLabels map[string]bool) (string, bool) {
	if workflow != nil && workflow.Trigger != nil {
		var labels []string
		if workflow.Trigger.GitHubIssues != nil {
			labels = workflow.Trigger.GitHubIssues.Labels
		}
		for _, label := range labels {
			if issueLabels[strings.ToLower(label)] {
				return label, true
			}
		}
	}
	for label := range issueLabels {
		return label, true
	}
	return "", false
}

func githubIssueWorkflowPollNeedsLabelerEvents(targets []githubIssueWorkflowPollTarget) bool {
	for _, target := range targets {
		for _, labeler := range githubIssuesWorkflowPollLabelers(target.workflow) {
			labeler = strings.TrimSpace(labeler)
			if labeler != "" && labeler != "*" {
				return true
			}
		}
	}
	return false
}

func githubIssuesWorkflowPollLabelers(workflow *types.WorkflowConfig) []string {
	if workflow == nil {
		return nil
	}
	if workflow.Trigger != nil {
		if workflow.Trigger.GitHubIssues != nil {
			return workflow.Trigger.GitHubIssues.Labelers
		}
	}
	return workflow.AllowedLabelers
}

func githubIssuesWorkflowPollLabeler(workflow *types.WorkflowConfig, labelName string, events []githubIssueEvent) (string, bool) {
	labelers := githubIssuesWorkflowPollLabelers(workflow)
	needsSpecificLabeler := false
	for _, labeler := range labelers {
		labeler = strings.TrimSpace(labeler)
		if labeler != "" && labeler != "*" {
			needsSpecificLabeler = true
			break
		}
	}
	if !needsSpecificLabeler {
		return "", true
	}
	for _, event := range events {
		if event.Event != "labeled" || event.Label == nil {
			continue
		}
		if !strings.EqualFold(event.Label.Name, labelName) {
			continue
		}
		for _, labeler := range labelers {
			if strings.EqualFold(strings.TrimSpace(labeler), event.Actor.Login) {
				return event.Actor.Login, true
			}
		}
	}
	return "", false
}

func (s *Server) buildGitHubIssuesPollPayload(issue githubIssuesPollItem, repo string) githubIssuesWebhookPayload {
	return buildGitHubIssuesPollPayloadForAction(issue, repo, "opened")
}

func buildGitHubIssuesPollPayloadForAction(issue githubIssuesPollItem, repo, action string) githubIssuesWebhookPayload {
	var payload githubIssuesWebhookPayload
	payload.Action = action
	payload.Issue.Number = issue.Number
	payload.Issue.Title = issue.Title
	payload.Issue.Body = issue.Body
	payload.Issue.HTMLURL = issue.HTMLURL
	payload.Issue.State = issue.State
	payload.Issue.User.Login = issue.User.Login
	for _, l := range issue.Labels {
		label := struct {
			Name string `json:"name"`
		}{Name: l.Name}
		payload.Issue.Labels = append(payload.Issue.Labels, label)
	}
	if issue.Assignee != nil {
		payload.Issue.Assignee = &struct {
			Login string `json:"login"`
		}{Login: issue.Assignee.Login}
	}
	payload.Repository.FullName = repo
	payload.Sender.Login = issue.User.Login
	return payload
}

// ── JIRA POLLER ─────────────────────────────────────────────────────────────

func (s *Server) pollJira(factories []*types.FactoryConfig, jiraCfgs []*types.JiraIntegrationConfig, since time.Time) {
	workspaceFactories := map[string][]*types.FactoryConfig{}
	for _, f := range factories {
		if f.Integration != "jira" {
			continue
		}
		if f.Enabled != nil && !*f.Enabled {
			continue
		}
		workspaceFactories[f.Workspace] = append(workspaceFactories[f.Workspace], f)
	}
	for _, ji := range jiraCfgs {
		if ji.Token == "" || ji.BaseURL == "" {
			continue
		}
		factories := append([]*types.FactoryConfig{}, workspaceFactories[ji.Workspace]...)
		if ji.Workspace != "" {
			factories = append(factories, workspaceFactories[""]...)
		}
		if len(factories) == 0 {
			continue
		}
		projects := jiraFactoryProjects(factories)
		tracker := workspaceIssueTracker{BaseURL: ji.BaseURL, Username: ji.Username, Token: ji.Token, WebhookSecret: ji.WebhookSecret}
		issues, err := s.queryJiraIssues(tracker, since, projects)
		if err != nil {
			log.Printf("[poll-jira] query failed for workspace %q: %v", ji.Workspace, err)
			continue
		}
		for _, issue := range issues {
			s.processJiraPollItem(issue, factories, tracker)
		}
	}
}

type jiraWorkflowPollTarget struct {
	workspace *types.WorkspaceConfig
	workflow  *types.WorkflowConfig
	tracker   workspaceIssueTracker
}

func (s *Server) pollJiraWorkflows(workspaces []*types.WorkspaceConfig, since time.Time) {
	type trackerKey struct {
		baseURL  string
		username string
		token    string
	}
	targetsByTracker := map[trackerKey][]jiraWorkflowPollTarget{}
	for _, workspace := range workspaces {
		if workspace == nil {
			continue
		}
		for _, workflow := range workspace.Workflows {
			if workflow == nil || workflow.Integration != "jira" {
				continue
			}
			if workflow.Enabled != nil && !*workflow.Enabled {
				continue
			}
			tracker, ok := s.resolveJiraTrackerForWorkflow(workspace.Name, workflow)
			if !ok || tracker.Token == "" || tracker.BaseURL == "" {
				log.Printf("[poll-jira] workflow %s/%s: no Jira tracker configured — skipping", workspace.Name, workflow.Name)
				continue
			}
			key := trackerKey{baseURL: tracker.BaseURL, username: tracker.Username, token: tracker.Token}
			targetsByTracker[key] = append(targetsByTracker[key], jiraWorkflowPollTarget{workspace: workspace, workflow: workflow, tracker: tracker})
		}
	}
	for _, targets := range targetsByTracker {
		if len(targets) == 0 {
			continue
		}
		tracker := targets[0].tracker
		issues, err := s.queryJiraIssues(tracker, since, jiraWorkflowProjects(targets))
		if err != nil {
			log.Printf("[poll-jira] workflow query failed: %v", err)
			continue
		}
		for _, issue := range issues {
			s.processJiraWorkflowPollItem(issue, targets)
		}
	}
}

func jiraFactoryProjects(factories []*types.FactoryConfig) []string {
	seen := map[string]bool{}
	var projects []string
	for _, factory := range factories {
		for _, project := range factory.Projects {
			project = strings.TrimSpace(project)
			if project == "" || seen[strings.ToLower(project)] {
				continue
			}
			seen[strings.ToLower(project)] = true
			projects = append(projects, project)
		}
	}
	return projects
}

func jiraWorkflowProjects(targets []jiraWorkflowPollTarget) []string {
	seen := map[string]bool{}
	var projects []string
	for _, target := range targets {
		if target.workflow == nil {
			continue
		}
		values := target.workflow.TriggerRepos
		if target.workflow.Trigger != nil && target.workflow.Trigger.Jira != nil && len(target.workflow.Trigger.Jira.Projects) > 0 {
			values = target.workflow.Trigger.Jira.Projects
		}
		for _, project := range values {
			project = strings.TrimSpace(project)
			if project == "" || seen[strings.ToLower(project)] {
				continue
			}
			seen[strings.ToLower(project)] = true
			projects = append(projects, project)
		}
	}
	return projects
}

func (s *Server) processJiraPollItem(issue jiraPollIssue, factories []*types.FactoryConfig, tracker workspaceIssueTracker) {
	payload := jiraPollPayload(issue)
	labels := jiraIssueLabels(issue)
	assignee := jiraIssueAssignee(issue)
	for _, factory := range factories {
		if factory.Integration != "jira" {
			continue
		}
		if !jiraProjectMatches(issue.Fields.Project.Key, factory.Projects) {
			continue
		}
		if factory.TriggerStatus == "" || !strings.EqualFold(issue.Fields.Status.Name, factory.TriggerStatus) {
			continue
		}
		if !labelsAllowed(labels, factory.Labels, factory.ExcludeLabels) || !s.assigneeMatches(assignee, factory.AssignedTo) {
			continue
		}
		if err := s.createClawForJiraIssue(factory, payload, "poll"); err != nil {
			if isFactoryTriggerAlreadyClaimed(err) {
				continue
			}
			log.Printf("[poll-jira] failed to create claw for %s: %v", issue.Key, err)
		}
	}
	_ = tracker
}

func (s *Server) processJiraWorkflowPollItem(issue jiraPollIssue, targets []jiraWorkflowPollTarget) {
	payload := jiraPollPayload(issue)
	labels := jiraIssueLabels(issue)
	assignee := jiraIssueAssignee(issue)
	for _, target := range targets {
		workspace := target.workspace
		workflow := target.workflow
		if workspace == nil || workflow == nil {
			continue
		}
		if !jiraWorkflowMatchesProject(workflow, issue.Fields.Project.Key) {
			continue
		}
		if workflow.TriggerStatus == "" || !strings.EqualFold(issue.Fields.Status.Name, workflow.TriggerStatus) {
			continue
		}
		if workflow.AssignedTo != "" && !assignedToMatches(workflow.AssignedTo, assignee) {
			continue
		}
		if !labelsAllowed(labels, workflow.Labels, workflow.ExcludeLabels) {
			continue
		}
		if err := s.createClawForJiraWorkflow(workspace, workflow, payload, "poll"); err != nil {
			if isFactoryTriggerAlreadyClaimed(err) {
				continue
			}
			log.Printf("[workflow:%s/%s] failed to create claw for Jira issue %s via poll: %v", workspace.Name, workflow.Name, issue.Key, err)
		}
	}
}

func jiraPollPayload(issue jiraPollIssue) jiraWebhookPayload {
	return jiraWebhookPayload{
		WebhookEvent: "jira:issue_updated",
		Timestamp:    time.Now().UnixMilli(),
		Issue:        jiraIssue(issue),
	}
}

// ── GITHUB PRs POLLER ─────────────────────────────────────────────────────────

func (s *Server) pollGitHubPRs(factories []*types.FactoryConfig, since string) {
	repoFactories := map[string][]*types.FactoryConfig{}
	for _, f := range factories {
		if f.Integration != "github" {
			continue
		}
		if f.Enabled != nil && !*f.Enabled {
			continue
		}
		if f.Trigger == nil || f.Trigger.On != "pull_request" {
			continue
		}
		for _, repo := range f.Repos {
			if repo == "" {
				continue
			}
			// Normalize: if glob like "owner/*", skip polling (can't enumerate)
			if strings.HasSuffix(repo, "/*") {
				continue
			}
			repoFactories[repo] = append(repoFactories[repo], f)
		}
	}

	for repo, repoFactories := range repoFactories {
		token := s.resolveGitHubTokenForRepo(repo)
		if token == "" {
			continue
		}

		base := s.githubBaseURL
		if base == "" {
			base = "https://api.github.com"
		}

		prs, err := s.queryGitHubPRs(repo, token, since, base)
		if err != nil {
			log.Printf("[poll-github-prs] query failed for repo %q: %v", repo, err)
			continue
		}

		for _, pr := range prs {
			s.processGitHubPRPollItem(pr, repoFactories, repo, base)
		}
	}
}

type githubPRPollItem struct {
	Number    int    `json:"number"`
	Title     string `json:"title"`
	HTMLURL   string `json:"html_url"`
	UpdatedAt string `json:"updated_at"`
	User      struct {
		Login string `json:"login"`
	} `json:"user"`
	Head struct {
		Ref string `json:"ref"`
		SHA string `json:"sha"`
	} `json:"head"`
	Base struct {
		Ref string `json:"ref"`
	} `json:"base"`
}

func (s *Server) queryGitHubPRs(repo, token, since, base string) ([]githubPRPollItem, error) {
	items, err := githubAPIListWithBase(base, fmt.Sprintf("repos/%s/pulls?state=open&since=%s&sort=updated&direction=desc", repo, since), token)
	if err != nil {
		return nil, err
	}

	var prs []githubPRPollItem
	for _, item := range items {
		m, ok := item.(map[string]interface{})
		if !ok {
			continue
		}
		var pr githubPRPollItem
		b, _ := json.Marshal(m)
		_ = json.Unmarshal(b, &pr)
		prs = append(prs, pr)
	}
	return prs, nil
}

func (s *Server) processGitHubPRPollItem(pr githubPRPollItem, factories []*types.FactoryConfig, repo, base string) {
	prID := fmt.Sprintf("%s#%d", repo, pr.Number)

	// If a non-deleted claw already exists for this PR, skip
	var existingID string
	_ = s.db.QueryRow(
		`SELECT c.id FROM claws c JOIN claw_prs cp ON cp.claw_id = c.id
		 WHERE cp.pr_url=? AND c.status NOT IN ('deleted') LIMIT 1`,
		pr.HTMLURL).Scan(&existingID)
	if existingID != "" {
		return
	}

	// New PR — check if any factory wants it
	for _, factory := range factories {
		if factory.Trigger == nil || factory.Trigger.On != "pull_request" {
			continue
		}
		if !githubRepoMatches(repo, factory.Repos) {
			continue
		}
		if factory.Trigger.Filter != nil {
			f := factory.Trigger.Filter
			if f.Author != "" && !strings.EqualFold(f.Author, pr.User.Login) {
				continue
			}
			if f.BaseBranch != "" && !strings.EqualFold(f.BaseBranch, pr.Base.Ref) {
				continue
			}
		}
		currentLabels, labelsOK := s.githubFactoryPRLabels(factory, repo, pr.Number)
		if !labelsOK {
			continue
		}
		if !labelsAllowed(currentLabels, factory.Labels, factory.ExcludeLabels) {
			continue
		}

		payload := s.buildGitHubPRPollPayload(pr, repo)
		if err := s.createClawForGitHubPR(factory, payload, "poll"); err != nil {
			log.Printf("[poll-github-prs] failed to create claw for %s: %v", prID, err)
		} else {
			log.Printf("[poll-github-prs] created claw for %s via factory %s", prID, factory.Name)
			s.logFactoryEvent(factory.Name, prID, pr.Title, "", "open", "claw_created", "", "poll")
		}
	}
}

func (s *Server) buildGitHubPRPollPayload(pr githubPRPollItem, repo string) githubPRPayload {
	var payload githubPRPayload
	payload.Action = "opened"
	payload.Number = pr.Number
	payload.PullRequest.HTMLURL = pr.HTMLURL
	payload.PullRequest.Title = pr.Title
	payload.PullRequest.User.Login = pr.User.Login
	payload.PullRequest.Head.Ref = pr.Head.Ref
	payload.PullRequest.Head.SHA = pr.Head.SHA
	payload.PullRequest.Base.Ref = pr.Base.Ref
	payload.Repository.FullName = repo
	return payload
}

// ── SHARED HELPERS ────────────────────────────────────────────────────────────

func (s *Server) factoryMatchesWorkspace(factory *types.FactoryConfig, workspace, teamKey, integration string) bool {
	if factory.Workspace != "" && !strings.EqualFold(factory.Workspace, workspace) {
		return false
	}
	if integration == "linear" && factory.Team != "" {
		// For Linear, compare Team against the issue's actual team key (not workspace)
		if !strings.EqualFold(factory.Team, teamKey) {
			return false
		}
	}
	return true
}

func (s *Server) labelsMatch(currentLabels []string, requiredLabels []string, excludedLabels []string) bool {
	return labelsAllowed(currentLabels, requiredLabels, excludedLabels)
}

func (s *Server) assigneeMatches(currentAssignee, filter string) bool {
	filter = strings.ToLower(strings.TrimSpace(filter))
	if filter == "" {
		return true
	}
	switch {
	case filter == "any":
		return currentAssignee != ""
	case filter == "none":
		return currentAssignee == ""
	case strings.HasPrefix(filter, "!"):
		excluded := strings.TrimPrefix(strings.TrimPrefix(filter, "!"), "@")
		return !strings.EqualFold(currentAssignee, excluded)
	default:
		target := strings.TrimPrefix(filter, "@")
		return strings.EqualFold(currentAssignee, target)
	}
}
