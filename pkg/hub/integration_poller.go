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
// events. It evaluates factory filters against current state and creates claws
// when no existing claw is found for the entity.
func (s *Server) startIntegrationPoller() {
	go func() {
		ticker := time.NewTicker(30 * time.Second)
		defer ticker.Stop()
		for range ticker.C {
			s.pollTick()
		}
	}()
}

// pollTick queries all four integration platforms for recently updated items
// and processes any trigger transitions that webhooks may have missed.
func (s *Server) pollTick() {
	now := time.Now().UTC()
	since := now.Add(-2 * time.Minute).Format(time.RFC3339)

	factories := s.resolveFactories()
	if len(factories) == 0 {
		log.Printf("[poll] tick: no factories configured")
		return
	}

	s.mu.RLock()
	integrations := s.hubCfg.Integrations
	s.mu.RUnlock()

	log.Printf("[poll] tick: %d factories loaded, evaluating integrations", len(factories))

	// === LINEAR ===
	if integrations != nil && len(integrations.Linear) > 0 {
		s.pollLinear(factories, integrations.Linear, since)
	}

	// === SHORTCUT ===
	if integrations != nil && len(integrations.Shortcut) > 0 {
		s.pollShortcut(factories, integrations.Shortcut, since)
	}

	// === GITHUB ISSUES ===
	if integrations != nil && len(integrations.GitHubIssues) > 0 {
		s.pollGitHubIssues(factories, integrations.GitHubIssues, since)
	}

	// === GITHUB PRs ===
	// Use factories with integration=="github" to discover repos
	s.pollGitHubPRs(factories, since)
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
		if agnostic, ok := workspaceFactories[""]; ok {
			wsFactories = append(wsFactories, agnostic...)
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
			label := struct{ Name string `json:"name"` }{Name: l.Name}
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

	// If a non-deleted claw already exists, skip regardless of state
	if s.clawExistsForLinearIssue(entityID) {
		return
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
		if !s.labelsMatch(currentLabels, factory.Labels) {
			continue
		}
		if !s.assigneeMatches(currentAssignee, factory.AssignedTo) {
			continue
		}

		// Build synthetic webhook payload and create claw
		payload := s.buildLinearPollPayload(issue)
		if err := s.createClawForIssue(factory, payload); err != nil {
			log.Printf("[poll-linear] failed to create claw for %s: %v", entityID, err)
		} else {
			log.Printf("[poll-linear] created claw for %s via factory %s", entityID, factory.Name)
			s.logFactoryEvent(factory.Name, entityID, issue.Title, "", currentStatus, "claw_created", "", "poll")
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
		label := struct{ Name string `json:"name"` }{Name: l.Name}
		payload.Data.Labels = append(payload.Data.Labels, label)
	}
	if issue.Assignee != nil {
		payload.Data.Assignee = &struct{ Name string `json:"name"` }{Name: issue.Assignee.Name}
	}
	return payload
}

func (s *Server) clawExistsForLinearIssue(issueID string) bool {
	var existingID string
	_ = s.db.QueryRow(
		`SELECT id FROM claws WHERE linear_issue_id = ? AND status NOT IN ('deleted') LIMIT 1`,
		issueID).Scan(&existingID)
	return existingID != ""
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
		if agnostic, ok := workspaceFactories[""]; ok {
			wsFactories = append(wsFactories, agnostic...)
		}
		if len(wsFactories) == 0 {
			continue
		}

		stories, err := s.queryShortcutStories(sc.Token, since)
		if err != nil {
			log.Printf("[poll-shortcut] query failed for workspace %q: %v", ws, err)
			continue
		}

		for _, story := range stories {
			s.processShortcutPollItem(story, wsFactories, ws, sc.Token)
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

func (s *Server) queryShortcutStories(token, since string) ([]shortcutPollStory, error) {
	body := map[string]interface{}{
		"updated_at_start": since,
	}
	jsonBody, _ := json.Marshal(body)
	req, _ := http.NewRequest("POST", "https://api.app.shortcut.com/api/v3/stories/search", bytes.NewReader(jsonBody))
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

	var result struct {
		Data []shortcutPollStory `json:"data"`
	}
	if err := json.Unmarshal(respBody, &result); err != nil {
		return nil, fmt.Errorf("parse shortcut response: %w", err)
	}
	return result.Data, nil
}

func (s *Server) processShortcutPollItem(story shortcutPollStory, factories []*types.FactoryConfig, workspace, token string) {
	storyID := fmt.Sprintf("sc-%d", story.ID)
	currentStateName := s.shortcutStateName(token, story.WorkflowStateID)
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
		data, err := shortcutAPI(fmt.Sprintf("members/%s", story.OwnerIDs[0]), token)
		if err == nil {
			if name, ok := data["mention_name"].(string); ok && name != "" {
				currentAssignee = name
			} else if name, ok := data["name"].(string); ok && name != "" {
				currentAssignee = name
			}
		}
	}

	// If a non-deleted claw already exists, skip regardless of state
	if s.clawExistsForShortcutStory(storyID) {
		return
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
		if !s.labelsMatch(currentLabels, factory.Labels) {
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
		if err := s.createClawForShortcutStory(factory, action, storyID, token); err != nil {
			log.Printf("[poll-shortcut] failed to create claw for %s: %v", storyID, err)
		} else {
			log.Printf("[poll-shortcut] created claw for %s via factory %s", storyID, factory.Name)
			s.logFactoryEvent(factory.Name, storyID, story.Name, "", currentStateName, "claw_created", "", "poll")
		}
	}
}

func (s *Server) clawExistsForShortcutStory(storyID string) bool {
	var existingID string
	_ = s.db.QueryRow(
		`SELECT id FROM claws WHERE linear_issue_id = ? AND status NOT IN ('deleted') LIMIT 1`,
		storyID).Scan(&existingID)
	return existingID != ""
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
		// Use workspace as repo if no explicit repos configured
		repos := []string{f.Workspace}
		if len(f.Repos) > 0 {
			repos = f.Repos
		}
		for _, repo := range repos {
			if repo == "" {
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

		// Fetch events for AllowedLabelers checks
		events, _ := s.queryGitHubIssueEvents(repo, token, since, base)

		for _, issue := range issues {
			s.processGitHubIssuesPollItem(issue, events, repoFactories, repo, token, base)
		}
	}
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

func (s *Server) queryGitHubIssueEvents(repo, token, since, base string) ([]githubIssueEvent, error) {
	items, err := githubAPIListWithBase(base, fmt.Sprintf("repos/%s/issues/events?since=%s", repo, since), token)
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

func (s *Server) processGitHubIssuesPollItem(issue githubIssuesPollItem, events []githubIssueEvent, factories []*types.FactoryConfig, repo, token, base string) {
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

	// If a non-deleted claw already exists, skip regardless of state
	if s.clawExistsForGitHubIssue(issueID) {
		log.Printf("[poll-github-issues] %s: claw already exists — skipping", issueID)
		return
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

		if !s.labelsMatch(currentLabels, factory.Labels) {
			continue
		}
		if !s.assigneeMatches(currentAssignee, factory.AssignedTo) {
			continue
		}

		// AllowedLabelers check
		if len(factory.AllowedLabelers) > 0 {
			allowed := false
			for _, event := range events {
				if event.Event != "labeled" || event.Issue == nil || event.Issue.Number != issue.Number {
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
		if err := s.createClawForGitHubIssue(factory, payload); err != nil {
			log.Printf("[poll-github-issues] failed to create claw for %s: %v", issueID, err)
		} else {
			log.Printf("[poll-github-issues] created claw for %s via factory %s", issueID, factory.Name)
			s.logFactoryEvent(factory.Name, issueID, issue.Title, "", currentStatus, "claw_created", "", "poll")
		}
	}
}

func (s *Server) buildGitHubIssuesPollPayload(issue githubIssuesPollItem, repo string) githubIssuesWebhookPayload {
	var payload githubIssuesWebhookPayload
	payload.Action = "opened" // synthetic action for polling
	payload.Issue.Number = issue.Number
	payload.Issue.Title = issue.Title
	payload.Issue.Body = issue.Body
	payload.Issue.HTMLURL = issue.HTMLURL
	payload.Issue.State = issue.State
	payload.Issue.User.Login = issue.User.Login
	for _, l := range issue.Labels {
		label := struct{ Name string `json:"name"` }{Name: l.Name}
		payload.Issue.Labels = append(payload.Issue.Labels, label)
	}
	if issue.Assignee != nil {
		payload.Issue.Assignee = &struct{ Login string `json:"login"` }{Login: issue.Assignee.Login}
	}
	payload.Repository.FullName = repo
	return payload
}

func (s *Server) clawExistsForGitHubIssue(issueID string) bool {
	var existingID string
	_ = s.db.QueryRow(
		`SELECT id FROM claws WHERE github_issue_id = ? AND status NOT IN ('deleted') LIMIT 1`,
		issueID).Scan(&existingID)
	return existingID != ""
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

		payload := s.buildGitHubPRPollPayload(pr, repo)
		if err := s.createClawForGitHubPR(factory, payload); err != nil {
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

func (s *Server) labelsMatch(currentLabels []string, requiredLabels []string) bool {
	if len(requiredLabels) == 0 {
		return true
	}
	currentSet := map[string]bool{}
	for _, l := range currentLabels {
		currentSet[strings.ToLower(strings.TrimSpace(l))] = true
	}
	for _, required := range requiredLabels {
		required = strings.ToLower(strings.TrimSpace(required))
		if required == "" {
			continue
		}
		if !currentSet[required] {
			return false
		}
	}
	return true
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
