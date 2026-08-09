package hub

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/elasticclaw/elasticclaw/pkg/hub/workflowv2"
	typesv2 "github.com/elasticclaw/elasticclaw/pkg/types/v2"
)

func (s *Server) processWorkflowV2GitHubPREvent(payload githubPRPayload) {
	targets, err := workflowv2.NewStore(s.db).ActiveDeliveryTargets(context.Background(),
		payload.Repository.FullName, payload.Number)
	if err != nil {
		log.Printf("[workflow-v2 github] find PR targets: %v", err)
		return
	}
	for _, target := range targets {
		workspace, err := typesv2.ParseAndValidateWorkspace([]byte(target.WorkspaceYAML))
		if err != nil {
			log.Printf("[workflow-v2 github] parse workspace for run %s: %v", target.RunID, err)
			continue
		}
		verified, err := s.verifyWorkflowV2PullRequest(context.Background(), workflowv2.Run{}, workspace.Workspace,
			typesv2.PullRequestClaim{URL: target.URL})
		if err != nil {
			log.Printf("[workflow-v2 github] verify %s#%d for run %s: %v",
				payload.Repository.FullName, payload.Number, target.RunID, err)
			continue
		}
		verified.ID = target.PRID
		verified.Provenance.Principal = payload.Sender.Login
		kind := workflowV2PullRequestEventKind(payload.Action, verified.State, verified.HeadSHA != target.HeadSHA)
		if kind == "" {
			continue
		}
		eventID := workflowV2GitHubEventID(target.RunID, "pull_request", payload.Action,
			verified.Repository, strconv.Itoa(verified.Number), verified.HeadSHA, verified.State,
			payload.PullRequest.UpdatedAt)
		if _, err := workflowv2.NewStore(s.db).ReconcilePullRequest(context.Background(), target, eventID, kind, verified); err != nil {
			log.Printf("[workflow-v2 github] reconcile %s#%d for run %s: %v",
				payload.Repository.FullName, payload.Number, target.RunID, err)
		}
	}
}

func workflowV2PullRequestEventKind(action, state string, headChanged bool) string {
	if state == "merged" {
		return "pull_request.merged"
	}
	if state == "closed" {
		return "pull_request.closed"
	}
	switch action {
	case "opened", "reopened", "ready_for_review":
		return "pull_request.verified_open"
	case "synchronize":
		if headChanged {
			return "pull_request.head_changed"
		}
		return "pull_request.updated"
	case "edited", "converted_to_draft":
		return "pull_request.updated"
	default:
		return ""
	}
}

func (s *Server) processWorkflowV2GitHubCheckEvent(event string, payload githubCheckPayload) {
	if payload.Action != "completed" {
		return
	}
	headSHA, numbers, _ := payload.checkEventSummary(event)
	if headSHA == "" {
		return
	}
	targets := make([]workflowv2.DeliveryTarget, 0)
	seen := map[string]bool{}
	store := workflowv2.NewStore(s.db)
	for _, number := range numbers {
		matched, err := store.ActiveDeliveryTargets(context.Background(), payload.Repository.FullName, number)
		if err != nil {
			log.Printf("[workflow-v2 github] find CI targets: %v", err)
			return
		}
		for _, target := range matched {
			if target.HeadSHA == headSHA && !seen[target.RunID+"\x00"+target.PRID] {
				seen[target.RunID+"\x00"+target.PRID] = true
				targets = append(targets, target)
			}
		}
	}
	if len(targets) == 0 {
		matched, err := store.ActiveDeliveryTargetsByHead(context.Background(), headSHA)
		if err != nil {
			log.Printf("[workflow-v2 github] find CI targets by head: %v", err)
			return
		}
		for _, target := range matched {
			if !seen[target.RunID+"\x00"+target.PRID] {
				seen[target.RunID+"\x00"+target.PRID] = true
				targets = append(targets, target)
			}
		}
	}
	if len(targets) == 0 {
		return
	}
	type githubCheck struct {
		ID          int64  `json:"id"`
		Name        string `json:"name"`
		Status      string `json:"status"`
		Conclusion  string `json:"conclusion"`
		DetailsURL  string `json:"details_url"`
		CompletedAt string `json:"completed_at"`
	}
	checksByRepository := map[string][]githubCheck{}
	for _, target := range targets {
		checks, loaded := checksByRepository[target.Repository]
		if !loaded {
			token := s.tokenForRepo(target.Repository)
			if token == "" {
				log.Printf("[workflow-v2 github] no repository-scoped token for CI %s", target.Repository)
				continue
			}
			data, err := githubAPIWithBaseContext(context.Background(), s.ghBaseURL(),
				fmt.Sprintf("repos/%s/commits/%s/check-runs?per_page=100", target.Repository, headSHA), token)
			if err != nil {
				log.Printf("[workflow-v2 github] reconcile checks for %s@%s: %v", target.Repository, headSHA, err)
				continue
			}
			rawRuns, _ := json.Marshal(data["check_runs"])
			if err := json.Unmarshal(rawRuns, &checks); err != nil {
				log.Printf("[workflow-v2 github] decode check runs for %s: %v", target.Repository, err)
				continue
			}
			checksByRepository[target.Repository] = checks
		}
		workspace, err := typesv2.ParseAndValidateWorkspace([]byte(target.WorkspaceYAML))
		if err != nil || workspace.Workspace.CI == nil {
			continue
		}
		evidenceInputs := make([]workflowv2.EvidenceInput, 0, len(checks))
		for pipelineName, pipeline := range workspace.Workspace.CI.Pipelines {
			if pipeline.Repository != target.RepositoryName {
				continue
			}
			connection := workspace.Workspace.CI.Connections[pipeline.Connection]
			if !strings.EqualFold(connection.Provider, "github_actions") {
				continue
			}
			for _, check := range checks {
				observed := parseGitHubTime(check.CompletedAt)
				rawExternalID := strconv.FormatInt(check.ID, 10)
				if check.ID == 0 {
					rawExternalID = check.Name
				}
				externalID := pipelineName + ":" + rawExternalID
				evidenceInputs = append(evidenceInputs, workflowv2.EvidenceInput{
					RunID: target.RunID, PRID: target.PRID, HeadSHA: headSHA, Domain: "ci",
					Connection: pipeline.Connection, ExternalID: externalID, Kind: check.Name,
					Status: workflowV2GitHubCheckStatus(check.Status, check.Conclusion), ObservedAt: observed,
					Payload: map[string]interface{}{"pipeline": pipelineName, "details_url": check.DetailsURL,
						"github_status": check.Status, "github_conclusion": check.Conclusion},
					Provenance: typesv2.EvidenceProvenance{Producer: string(workflowv2.ProducerCI),
						Connection: pipeline.Connection, ExternalID: rawExternalID, ObservedAt: observed, Reconciled: true},
				})
			}
		}
		if len(evidenceInputs) > 0 {
			if _, err := store.ReconcileEvidenceSet(context.Background(), evidenceInputs, workflowv2.ProducerCI); err != nil {
				log.Printf("[workflow-v2 github] reconcile check snapshot for run %s: %v", target.RunID, err)
			}
		}
	}
}

func workflowV2GitHubCheckStatus(status, conclusion string) string {
	if !strings.EqualFold(status, "completed") {
		return "pending"
	}
	switch strings.ToLower(strings.TrimSpace(conclusion)) {
	case "success", "neutral", "skipped":
		return "success"
	case "":
		return "pending"
	default:
		return strings.ToLower(strings.TrimSpace(conclusion))
	}
}

func (s *Server) processWorkflowV2GitHubReviewEvent(payload githubPRReviewPayload) {
	if payload.Action != "submitted" && payload.Action != "edited" && payload.Action != "dismissed" {
		return
	}
	if !s.isHumanGitHubActor(payload.Review.User.Login, payload.Review.User.Type) {
		return
	}
	targets, err := workflowv2.NewStore(s.db).ActiveDeliveryTargets(context.Background(),
		payload.Repository.FullName, payload.PullRequest.Number)
	if err != nil {
		log.Printf("[workflow-v2 github] find review targets: %v", err)
		return
	}
	for _, target := range targets {
		status := strings.ToLower(strings.TrimSpace(payload.Review.State))
		if payload.Action == "dismissed" {
			status = "dismissed"
		}
		observed := now().UTC()
		if status == "changes_requested" {
			// Persist the typed feedback fact before review evidence can take a
			// policy transition that schedules an include_facts agent task.
			s.applyWorkflowV2GitHubFeedback(target, "review", payload.Review.ID, payload.Review.User.Login,
				payload.Review.Body, payload.Review.HTMLURL, "", 0)
		}
		connections := workflowV2GitHubReviewConnections(target)
		for _, connection := range connections {
			_, err := workflowv2.NewStore(s.db).RecordEvidence(context.Background(), workflowv2.EvidenceInput{
				RunID: target.RunID, PRID: target.PRID, HeadSHA: target.HeadSHA, Domain: "review",
				Connection: connection, ExternalID: payload.Review.User.Login, Kind: "approval", Status: status,
				ObservedAt: observed, Payload: map[string]interface{}{"review_id": payload.Review.ID,
					"url": payload.Review.HTMLURL, "body": payload.Review.Body},
				Provenance: typesv2.EvidenceProvenance{Producer: string(workflowv2.ProducerReview),
					Connection: connection, Principal: payload.Review.User.Login,
					ExternalID: strconv.FormatInt(payload.Review.ID, 10), ObservedAt: observed},
			}, workflowv2.ProducerReview)
			if err != nil {
				log.Printf("[workflow-v2 github] record review for run %s: %v", target.RunID, err)
			}
		}
	}
}

func (s *Server) processWorkflowV2GitHubReviewCommentEvent(payload githubPRReviewCommentPayload) {
	if payload.Action != "created" || !s.isHumanGitHubActor(payload.Comment.User.Login, payload.Comment.User.Type) {
		return
	}
	s.processWorkflowV2GitHubFeedbackTargets(payload.Repository.FullName, payload.PullRequest.Number,
		"inline_comment", payload.Comment.ID, payload.Comment.User.Login, payload.Comment.Body,
		payload.Comment.HTMLURL, payload.Comment.Path, payload.Comment.Line)
}

func (s *Server) processWorkflowV2GitHubIssueCommentEvent(payload githubIssueCommentPayload) {
	if payload.Action != "created" || !s.isHumanGitHubActor(payload.Comment.User.Login, payload.Comment.User.Type) {
		return
	}
	s.processWorkflowV2GitHubFeedbackTargets(payload.Repository.FullName, payload.Issue.Number,
		"pull_request_comment", payload.Comment.ID, payload.Comment.User.Login, payload.Comment.Body,
		payload.Comment.HTMLURL, "", 0)
}

func (s *Server) processWorkflowV2GitHubFeedbackTargets(repository string, number int, kind string, externalID int64,
	author, body, url, path string, line int) {
	targets, err := workflowv2.NewStore(s.db).ActiveDeliveryTargets(context.Background(), repository, number)
	if err != nil {
		log.Printf("[workflow-v2 github] find feedback targets: %v", err)
		return
	}
	for _, target := range targets {
		s.applyWorkflowV2GitHubFeedback(target, kind, externalID, author, body, url, path, line)
	}
}

func (s *Server) applyWorkflowV2GitHubFeedback(target workflowv2.DeliveryTarget, kind string, externalID int64,
	author, body, url, path string, line int) {
	observed := now().UTC()
	feedback := map[string]interface{}{"kind": kind, "author": author, "body": body, "url": url,
		"repository": target.Repository, "number": target.Number, "pull_request_url": target.URL}
	if path != "" {
		feedback["path"] = path
	}
	if line > 0 {
		feedback["line"] = line
	}
	eventID := workflowV2GitHubEventID(target.RunID, "review_feedback", kind,
		strconv.FormatInt(externalID, 10), target.Repository, strconv.Itoa(target.Number))
	_, err := workflowv2.NewStore(s.db).ApplyReviewFeedback(context.Background(), target, eventID, feedback,
		typesv2.EvidenceProvenance{Producer: string(workflowv2.ProducerReview), Principal: author,
			ExternalID: strconv.FormatInt(externalID, 10), ObservedAt: observed})
	if err != nil {
		log.Printf("[workflow-v2 github] apply feedback to run %s: %v", target.RunID, err)
	}
}

func workflowV2GitHubReviewConnections(target workflowv2.DeliveryTarget) []string {
	workspace, err := typesv2.ParseAndValidateWorkspace([]byte(target.WorkspaceYAML))
	if err != nil || workspace.Workspace.ReviewSystems == nil {
		return nil
	}
	repository := workspace.Workspace.Repositories[target.RepositoryName]
	var connections []string
	for name, connection := range workspace.Workspace.ReviewSystems.Connections {
		if strings.EqualFold(connection.Provider, "github") &&
			(connection.SourceControl == "" || connection.SourceControl == repository.SourceControl) {
			connections = append(connections, name)
		}
	}
	sort.Strings(connections)
	return connections
}

func workflowV2GitHubEventID(runID string, parts ...string) string {
	hash := sha256.New()
	_, _ = hash.Write([]byte(runID))
	for _, part := range parts {
		_, _ = hash.Write([]byte{0})
		_, _ = hash.Write([]byte(part))
	}
	return "github:" + hex.EncodeToString(hash.Sum(nil))
}

func parseGitHubTime(value string) time.Time {
	if parsed, err := time.Parse(time.RFC3339Nano, strings.TrimSpace(value)); err == nil {
		return parsed.UTC()
	}
	return now().UTC()
}
