package integrations

import (
	"encoding/json"
	"errors"
	"net/http"
	"regexp"
	"strconv"
)

// Types shared between pkg/hub and this package. They moved here (exported)
// during the phase-2 extraction; pkg/hub keeps aliases so its remaining call
// sites are unchanged.

// WorkspaceIssueTracker is a workspace-scoped issue tracker credential set
// (was pkg/hub's workspaceIssueTracker; the yaml file layout is owned by
// workspace_managed.go in pkg/hub).
type WorkspaceIssueTracker struct {
	BaseURL       string `yaml:"base_url,omitempty" json:"baseUrl,omitempty"`
	Username      string `yaml:"username,omitempty" json:"username,omitempty"`
	Token         string `yaml:"token,omitempty" json:"token,omitempty"`
	WebhookSecret string `yaml:"webhook_secret,omitempty" json:"webhookSecret,omitempty"`
}

// TriggerActor identifies who triggered a workflow/factory run (was
// pkg/hub's triggerActor).
type TriggerActor struct {
	ID    string `json:"id,omitempty"`
	Login string `json:"login,omitempty"`
	Name  string `json:"name,omitempty"`
	Email string `json:"email,omitempty"`
	URL   string `json:"url,omitempty"`
	Type  string `json:"type,omitempty"`
}

// AgentFailureKind classifies an agent-creation failure (was pkg/hub's
// agentFailureKind; the classification rules stay in pkg/hub).
type AgentFailureKind string

// AgentFailureMessage is the user-facing rendering of a classified agent
// failure (was pkg/hub's agentFailureMessage).
type AgentFailureMessage struct {
	Kind        AgentFailureKind
	StatusCode  int
	Title       string
	UserMessage string
	NextStep    string
	SafeDetail  string
}

// AgentFailureFeedback describes a claw-creation failure to report back to
// the originating issue tracker (was pkg/hub's agentFailureFeedback).
type AgentFailureFeedback struct {
	Integration      string
	IssueID          string
	GitHubRepo       string
	GitHubIssueNum   int
	LinearIdentifier string
	TriggerActor     TriggerActor
	AgentStatusError string
	Failure          AgentFailureMessage
	ClawID           string
}

// WorkflowCreateOptions carries the optional inputs for creating a claw from
// a workflow (was pkg/hub's workflowCreateOptions; the creation logic stays
// in pkg/hub's workflow_creator.go until the workflows/ extraction).
type WorkflowCreateOptions struct {
	Inputs               map[string]string
	WorkspaceFiles       map[string]string
	ClawName             string
	GitHubIssueID        string
	LinearIssueID        string
	ShortcutStoryID      string
	JiraIssueID          string
	IssueLabels          []string
	IssueLabelsAvailable bool
	Reason               string
	TriggerActor         *TriggerActor
}

// ErrFactoryTriggerAlreadyClaimed is returned when a factory trigger has
// already been claimed by a concurrent webhook/poll delivery (was pkg/hub's
// errFactoryTriggerAlreadyClaimed; the claim table stays in pkg/hub).
var ErrFactoryTriggerAlreadyClaimed = errors.New("factory trigger already claimed")

// IsFactoryTriggerAlreadyClaimed reports whether err is the trigger-claim
// sentinel.
func IsFactoryTriggerAlreadyClaimed(err error) bool {
	return errors.Is(err, ErrFactoryTriggerAlreadyClaimed)
}

// FactoryTriggerKey builds the dedup key for a factory trigger.
func FactoryTriggerKey(integration, externalID string) string {
	return integration + ":" + externalID
}

var prURLRegex = regexp.MustCompile(`https://github\.com/([a-zA-Z0-9_.-]+/[a-zA-Z0-9_.-]+)/pull/(\d+)`)

// PRRef is a GitHub pull request reference extracted from message text.
type PRRef struct {
	Repo   string
	Number int
	URL    string
}

// ExtractPRs finds GitHub PR URLs in a message body (was pkg/hub's
// extractPRs; pr_watcher.go still calls it from pkg/hub).
func ExtractPRs(content string) []PRRef {
	var results []PRRef
	seen := map[string]bool{}
	for _, m := range prURLRegex.FindAllStringSubmatch(content, -1) {
		url := m[0]
		if seen[url] {
			continue
		}
		seen[url] = true
		num, _ := strconv.Atoi(m[2])
		results = append(results, PRRef{Repo: m[1], Number: num, URL: url})
	}
	return results
}

// jsonOK writes v as a JSON 200 response (mirrors httpserver.JSONOK; kept
// local so integrations does not depend on the httpserver layer).
func jsonOK(w http.ResponseWriter, v interface{}) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(v)
}
