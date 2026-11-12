package hub

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log"
	"path"
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
		if kind != "" {
			eventID := workflowV2GitHubEventID(target.RunID, "pull_request", payload.Action,
				verified.Repository, strconv.Itoa(verified.Number), verified.HeadSHA, verified.State,
				payload.PullRequest.UpdatedAt)
			if err := s.reconcileWorkflowV2PullRequest(context.Background(), workflowv2.NewStore(s.db), target, eventID, kind, verified); err != nil {
				log.Printf("[workflow-v2 github] reconcile %s#%d for run %s: %v",
					payload.Repository.FullName, payload.Number, target.RunID, err)
				continue
			}
		} else if verified.HeadSHA != target.HeadSHA || verified.State != target.State {
			// The stored head is stale and this action does not reconcile it;
			// the periodic reconciler updates the PR before mergeability.
			continue
		}
		target.HeadSHA, target.State = verified.HeadSHA, verified.State
		if _, err := workflowv2.NewStore(s.db).ReconcileMergeability(context.Background(), target, verified); err != nil {
			log.Printf("[workflow-v2 github] reconcile mergeability for run %s: %v", target.RunID, err)
		}
	}
}

// reconcileWorkflowV2PullRequest applies a verified PR observation to its v2
// run. A merge or close is first mirrored into the parent task run's PR rows:
// adopted PRs have no claw_prs row and the v1 webhook update only runs for
// factories, so otherwise the summary materialized at the run's terminal state
// would still count the PR as open until the dead-claw sweep.
func (s *Server) reconcileWorkflowV2PullRequest(ctx context.Context, store *workflowv2.Store,
	target workflowv2.DeliveryTarget, eventID, kind string, verified typesv2.VerifiedPullRequest) error {
	if verified.State == "merged" || verified.State == "closed" {
		var taskRunID string
		err := s.db.QueryRowContext(ctx, `SELECT task_run_id FROM workflow_v2_runs WHERE id=?`, target.RunID).Scan(&taskRunID)
		if err == nil && taskRunID != "" {
			err = s.associateTaskRunPR(TaskRunPR{RunID: taskRunID, Repo: verified.Repository, PRNumber: verified.Number,
				URL: verified.URL, State: taskRunPRStateClosed, Merged: verified.State == "merged",
				OccurredAt: verified.Provenance.ObservedAt})
		}
		if err != nil {
			log.Printf("[workflow-v2 github] record %s#%d %s for run %s: %v",
				verified.Repository, verified.Number, verified.State, target.RunID, err)
		}
	}
	_, err := store.ReconcilePullRequest(ctx, target, eventID, kind, verified)
	return err
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
			if target.Repository == payload.Repository.FullName && !seen[target.RunID+"\x00"+target.PRID] {
				seen[target.RunID+"\x00"+target.PRID] = true
				targets = append(targets, target)
			}
		}
	}
	if len(targets) == 0 {
		return
	}
	if err := s.reconcileWorkflowV2GitHubChecks(context.Background(), store, targets, headSHA); err != nil {
		log.Printf("[workflow-v2 github] reconcile check snapshot: %v", err)
	}
}

func (s *Server) reconcileWorkflowV2GitHubChecks(ctx context.Context, store *workflowv2.Store,
	targets []workflowv2.DeliveryTarget, headSHA string) error {
	type githubCheck struct {
		ID          int64  `json:"id"`
		Name        string `json:"name"`
		Status      string `json:"status"`
		Conclusion  string `json:"conclusion"`
		DetailsURL  string `json:"details_url"`
		CompletedAt string `json:"completed_at"`
		App         struct {
			Slug string `json:"slug"`
		} `json:"app"`
		CheckSuite struct {
			ID int64 `json:"id"`
		} `json:"check_suite"`
	}
	type commitStatus struct {
		ID        int64  `json:"id"`
		Context   string `json:"context"`
		State     string `json:"state"`
		TargetURL string `json:"target_url"`
		UpdatedAt string `json:"updated_at"`
	}
	statusesByRepository := map[string][]commitStatus{}
	needsWorkflow, needsStatuses := map[string]bool{}, map[string]bool{}
	for _, target := range targets {
		workspace, err := typesv2.ParseAndValidateWorkspace([]byte(target.WorkspaceYAML))
		if err != nil || workspace.Workspace.CI == nil {
			continue
		}
		for _, pipeline := range workspace.Workspace.CI.Pipelines {
			if pipeline.Repository != target.RepositoryName {
				continue
			}
			if pipeline.CheckName != "" {
				needsStatuses[target.Repository] = needsStatuses[target.Repository] || pipeline.AppSlug == ""
			} else if strings.EqualFold(workspace.Workspace.CI.Connections[pipeline.Connection].Provider, "github_actions") {
				needsWorkflow[target.Repository] = true
			}
		}
	}
	checksByRepository := map[string][]githubCheck{}
	workflowRunsByRepository := map[string]map[int64]struct{ name, path string }{}
	for _, target := range targets {
		if err := ctx.Err(); err != nil {
			return err
		}
		checks, loaded := checksByRepository[target.Repository]
		if !loaded {
			token := s.tokenForRepo(target.Repository)
			if token == "" {
				log.Printf("[workflow-v2 github] no repository-scoped token for CI %s", target.Repository)
				continue
			}
			rawRuns, err := githubAPICollectionWithBaseContext(ctx, s.ghBaseURL(),
				fmt.Sprintf("repos/%s/commits/%s/check-runs?filter=latest&per_page=100", target.Repository, headSHA),
				token, "check_runs")
			if err != nil {
				log.Printf("[workflow-v2 github] reconcile checks for %s@%s: %v", target.Repository, headSHA, err)
				continue
			}
			rawChecks, _ := json.Marshal(rawRuns)
			if err := json.Unmarshal(rawChecks, &checks); err != nil {
				log.Printf("[workflow-v2 github] decode check runs for %s: %v", target.Repository, err)
				continue
			}
			checksByRepository[target.Repository] = checks
			if needsStatuses[target.Repository] {
				rawStatuses, statusErr := githubAPICollectionWithBaseContext(ctx, s.ghBaseURL(),
					fmt.Sprintf("repos/%s/commits/%s/status?per_page=100", target.Repository, headSHA), token, "statuses")
				if statusErr != nil {
					log.Printf("[workflow-v2 github] reconcile commit statuses for %s: %v", target.Repository, statusErr)
				} else {
					encoded, _ := json.Marshal(rawStatuses)
					var statuses []commitStatus
					if err := json.Unmarshal(encoded, &statuses); err != nil {
						log.Printf("[workflow-v2 github] decode commit statuses for %s: %v", target.Repository, err)
					} else {
						statusesByRepository[target.Repository] = statuses
					}
				}
			}
			if needsWorkflow[target.Repository] {
				rawRuns, err = githubAPICollectionWithBaseContext(ctx, s.ghBaseURL(),
					fmt.Sprintf("repos/%s/actions/runs?head_sha=%s&per_page=100", target.Repository, headSHA), token, "workflow_runs")
				if err != nil {
					log.Printf("[workflow-v2 github] reconcile workflow runs for %s@%s: %v", target.Repository, headSHA, err)
				} else {
					encoded, _ := json.Marshal(rawRuns)
					var runs []struct {
						Name         string `json:"name"`
						Path         string `json:"path"`
						CheckSuiteID int64  `json:"check_suite_id"`
					}
					if err := json.Unmarshal(encoded, &runs); err != nil {
						log.Printf("[workflow-v2 github] decode workflow runs for %s: %v", target.Repository, err)
					} else {
						bySuite := map[int64]struct{ name, path string }{}
						for _, run := range runs {
							if run.CheckSuiteID > 0 {
								bySuite[run.CheckSuiteID] = struct{ name, path string }{run.Name, run.Path}
							}
						}
						workflowRunsByRepository[target.Repository] = bySuite
					}
				}
			}
		}
		workflowRuns, workflowsLoaded := workflowRunsByRepository[target.Repository]
		statuses, statusesLoaded := statusesByRepository[target.Repository]
		workspace, err := typesv2.ParseAndValidateWorkspace([]byte(target.WorkspaceYAML))
		if err != nil || workspace.Workspace.CI == nil {
			continue
		}
		evidenceInputs := make([]workflowv2.EvidenceInput, 0, len(checks))
		evidenceScopes := make([]workflowv2.EvidenceScope, 0)
		for pipelineName, pipeline := range workspace.Workspace.CI.Pipelines {
			if pipeline.Repository != target.RepositoryName {
				continue
			}
			connection := workspace.Workspace.CI.Connections[pipeline.Connection]
			if pipeline.CheckName == "" {
				if !strings.EqualFold(connection.Provider, "github_actions") || !workflowsLoaded {
					continue
				}
			} else if pipeline.AppSlug == "" && !statusesLoaded {
				// A partial snapshot must not supersede previously observed statuses.
				continue
			}
			evidenceScopes = append(evidenceScopes, workflowv2.EvidenceScope{RunID: target.RunID,
				PRID: target.PRID, HeadSHA: headSHA, Domain: "ci", Connection: pipeline.Connection,
				Pipeline: pipelineName})
			for _, check := range checks {
				if pipeline.CheckName != "" {
					if !workflowV2GitHubCheckNameMatches(pipeline, check.Name, check.App.Slug) {
						continue
					}
				} else {
					workflowRun, ok := workflowRuns[check.CheckSuite.ID]
					if !strings.EqualFold(check.App.Slug, "github-actions") || !ok ||
						!workflowV2GitHubWorkflowMatches(pipeline.Workflow, workflowRun.name, workflowRun.path) {
						continue
					}
				}
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
			// Commit statuses expose no authoritative App slug. They can only
			// satisfy name-only pipelines, never an app-scoped pipeline.
			if pipeline.CheckName != "" && pipeline.AppSlug == "" {
				for _, status := range statuses {
					if !workflowV2GitHubCheckNameMatches(pipeline, status.Context, "") {
						continue
					}
					observed := parseGitHubTime(status.UpdatedAt)
					rawID := strconv.FormatInt(status.ID, 10)
					evidenceInputs = append(evidenceInputs, workflowv2.EvidenceInput{
						RunID: target.RunID, PRID: target.PRID, HeadSHA: headSHA, Domain: "ci",
						Connection: pipeline.Connection, ExternalID: pipelineName + ":status:" + status.Context,
						Kind: status.Context, Status: status.State, ObservedAt: observed,
						Payload: map[string]interface{}{"pipeline": pipelineName, "details_url": status.TargetURL,
							"github_status": status.State},
						Provenance: typesv2.EvidenceProvenance{Producer: string(workflowv2.ProducerCI),
							Connection: pipeline.Connection, ExternalID: rawID, ObservedAt: observed, Reconciled: true},
					})
				}
			}
		}
		if len(evidenceScopes) > 0 {
			if _, err := store.ReconcileEvidenceSnapshot(ctx, evidenceScopes, evidenceInputs,
				workflowv2.ProducerCI); err != nil {
				log.Printf("[workflow-v2 github] reconcile check snapshot for run %s: %v", target.RunID, err)
			}
		}
	}
	return nil
}

func workflowV2GitHubCheckNameMatches(pipeline typesv2.Pipeline, name, appSlug string) bool {
	matched, err := typesv2.MatchCheckName(pipeline.CheckName, name)
	return err == nil && matched && (pipeline.AppSlug == "" || strings.EqualFold(pipeline.AppSlug, appSlug))
}

func workflowV2GitHubWorkflowMatches(configured, name, workflowPath string) bool {
	configured = strings.TrimSpace(configured)
	workflowPath = strings.TrimSpace(workflowPath)
	if configured == "" || workflowPath == "" {
		return false
	}
	return configured == workflowPath || configured == path.Base(workflowPath) ||
		strings.EqualFold(configured, strings.TrimSpace(name))
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
	token := s.tokenForRepo(payload.Repository.FullName)
	if token == "" {
		log.Printf("[workflow-v2 github] no repository-scoped token for review %s", payload.Repository.FullName)
		return
	}
	review, err := githubAPIWithBaseContext(context.Background(), s.ghBaseURL(), fmt.Sprintf(
		"repos/%s/pulls/%d/reviews/%d", payload.Repository.FullName, payload.PullRequest.Number, payload.Review.ID), token)
	if err != nil {
		log.Printf("[workflow-v2 github] verify review %d on %s#%d: %v", payload.Review.ID,
			payload.Repository.FullName, payload.PullRequest.Number, err)
		return
	}
	user, _ := review["user"].(map[string]interface{})
	login, _ := user["login"].(string)
	userType, _ := user["type"].(string)
	if !s.isHumanGitHubActor(login, userType) {
		return
	}
	commitID, _ := review["commit_id"].(string)
	status, _ := review["state"].(string)
	status = strings.ToLower(strings.TrimSpace(status))
	body, _ := review["body"].(string)
	url, _ := review["html_url"].(string)
	submittedAt, _ := review["submitted_at"].(string)
	observed, observedErr := time.Parse(time.RFC3339Nano, strings.TrimSpace(submittedAt))
	if commitID == "" || status == "" || observedErr != nil {
		log.Printf("[workflow-v2 github] review %d is missing authoritative head, state, or timestamp", payload.Review.ID)
		return
	}
	observed = observed.UTC()
	targets, err := workflowv2.NewStore(s.db).ActiveDeliveryTargets(context.Background(),
		payload.Repository.FullName, payload.PullRequest.Number)
	if err != nil {
		log.Printf("[workflow-v2 github] find review targets: %v", err)
		return
	}
	for _, target := range targets {
		if target.HeadSHA != commitID {
			continue
		}
		if status == "changes_requested" {
			// Persist the typed feedback fact before review evidence can take a
			// policy transition that schedules an include_facts agent task.
			feedbackID := strconv.FormatInt(payload.Review.ID, 10)
			s.applyWorkflowV2GitHubFeedback(target, "review", feedbackID, login, body, url, "", 0, observed)
		}
		connections := workflowV2GitHubReviewConnections(target)
		for _, connection := range connections {
			_, err := workflowv2.NewStore(s.db).RecordEvidence(context.Background(), workflowv2.EvidenceInput{
				RunID: target.RunID, PRID: target.PRID, HeadSHA: target.HeadSHA, Domain: "review",
				Connection: connection, ExternalID: login, Kind: "approval", Status: status,
				ObservedAt: observed, Payload: map[string]interface{}{"review_id": payload.Review.ID,
					"url": url, "body": body, "commit_id": commitID},
				Provenance: typesv2.EvidenceProvenance{Producer: string(workflowv2.ProducerReview),
					Connection: connection, Principal: login,
					ExternalID: strconv.FormatInt(payload.Review.ID, 10), ObservedAt: observed},
			}, workflowv2.ProducerReview)
			if err != nil {
				log.Printf("[workflow-v2 github] record review for run %s: %v", target.RunID, err)
			}
		}
	}
}

func (s *Server) processWorkflowV2GitHubReviewCommentEvent(payload githubPRReviewCommentPayload) {
	if payload.Action != "created" || strings.TrimSpace(payload.Comment.CommitID) == "" ||
		!s.isHumanGitHubActor(payload.Comment.User.Login, payload.Comment.User.Type) {
		return
	}
	s.processWorkflowV2GitHubFeedbackTargets(payload.Repository.FullName, payload.PullRequest.Number,
		"inline_comment", strconv.FormatInt(payload.Comment.ID, 10), payload.Comment.User.Login, payload.Comment.Body,
		payload.Comment.HTMLURL, payload.Comment.Path, payload.Comment.Line, payload.Comment.CommitID,
		parseGitHubTime(payload.Comment.UpdatedAt))
}

func (s *Server) processWorkflowV2GitHubIssueCommentEvent(payload githubIssueCommentPayload) {
	if payload.Action != "created" || !s.isHumanGitHubActor(payload.Comment.User.Login, payload.Comment.User.Type) {
		return
	}
	s.processWorkflowV2GitHubFeedbackTargets(payload.Repository.FullName, payload.Issue.Number,
		"pull_request_comment", strconv.FormatInt(payload.Comment.ID, 10), payload.Comment.User.Login, payload.Comment.Body,
		payload.Comment.HTMLURL, "", 0, "", parseGitHubTime(payload.Comment.UpdatedAt))
}

func (s *Server) processWorkflowV2GitHubFeedbackTargets(repository string, number int, kind, externalID string,
	author, body, url, path string, line int, headSHA string, observed time.Time) {
	targets, err := workflowv2.NewStore(s.db).ActiveDeliveryTargets(context.Background(), repository, number)
	if err != nil {
		log.Printf("[workflow-v2 github] find feedback targets: %v", err)
		return
	}
	for _, target := range targets {
		if headSHA != "" && target.HeadSHA != headSHA {
			continue
		}
		s.applyWorkflowV2GitHubFeedback(target, kind, externalID, author, body, url, path, line, observed)
	}
}

func (s *Server) applyWorkflowV2GitHubFeedback(target workflowv2.DeliveryTarget, kind, externalID string,
	author, body, url, path string, line int, observed time.Time) {
	if observed.IsZero() {
		observed = now().UTC()
	}
	feedback := map[string]interface{}{"kind": kind, "author": author, "body": body, "url": url,
		"repository": target.Repository, "number": target.Number, "pull_request_url": target.URL}
	if path != "" {
		feedback["path"] = path
	}
	if line > 0 {
		feedback["line"] = line
	}
	eventID := workflowV2GitHubFeedbackEventID(target, kind, externalID)
	_, err := workflowv2.NewStore(s.db).ReconcileReviewFeedback(context.Background(), target, eventID, feedback,
		typesv2.EvidenceProvenance{Producer: string(workflowv2.ProducerReview), Principal: author,
			ExternalID: kind + ":" + externalID, ObservedAt: observed})
	if err != nil {
		log.Printf("[workflow-v2 github] apply feedback to run %s: %v", target.RunID, err)
	}
}

func workflowV2GitHubFeedbackEventID(target workflowv2.DeliveryTarget, kind, externalID string) string {
	return workflowV2GitHubEventID(target.RunID, "review_feedback", kind, externalID,
		target.Repository, strconv.Itoa(target.Number))
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
