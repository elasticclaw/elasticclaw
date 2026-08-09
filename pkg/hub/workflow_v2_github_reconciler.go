package hub

import (
	"context"
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

const (
	workflowV2GitHubReconcileInterval = 2 * time.Minute
	workflowV2GitHubReconcileTimeout  = 30 * time.Second
)

// runWorkflowV2GitHubReconciler makes webhook delivery an optimization rather
// than a correctness dependency. It runs once on startup for restart recovery,
// then periodically while respecting the shared GitHub low-priority reserve.
func (s *Server) runWorkflowV2GitHubReconciler(ctx context.Context) {
	reconcile := func() {
		if !defaultGitHubClient.allowLowPriority() {
			return
		}
		if err := s.reconcileWorkflowV2GitHub(ctx); err != nil && ctx.Err() == nil {
			log.Printf("[workflow-v2 github] periodic reconciliation: %v", err)
		}
	}
	reconcile()
	ticker := time.NewTicker(workflowV2GitHubReconcileInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			reconcile()
		}
	}
}

func (s *Server) reconcileWorkflowV2GitHub(ctx context.Context) error {
	store := workflowv2.NewStore(s.db)
	targets, err := store.ActiveDeliveryTargetsAll(ctx)
	if err != nil {
		return fmt.Errorf("list active deliveries: %w", err)
	}
	current := make([]workflowv2.DeliveryTarget, 0, len(targets))
	for _, target := range targets {
		if err := ctx.Err(); err != nil {
			return err
		}
		if !defaultGitHubClient.allowLowPriority() {
			break
		}
		targetCtx, cancel := context.WithTimeout(ctx, workflowV2GitHubReconcileTimeout)
		workspace, parseErr := typesv2.ParseAndValidateWorkspace([]byte(target.WorkspaceYAML))
		if parseErr != nil {
			cancel()
			log.Printf("[workflow-v2 github] parse workspace for run %s: %v", target.RunID, parseErr)
			continue
		}
		verified, verifyErr := s.verifyWorkflowV2PullRequest(targetCtx, workflowv2.Run{}, workspace.Workspace,
			typesv2.PullRequestClaim{URL: target.URL})
		cancel()
		if verifyErr != nil {
			log.Printf("[workflow-v2 github] verify %s#%d for run %s: %v",
				target.Repository, target.Number, target.RunID, verifyErr)
			continue
		}
		verified.ID = target.PRID
		verified.Provenance.Principal = "github-reconciler"
		if kind := workflowV2ReconciledPREventKind(target, verified); kind != "" {
			eventID := workflowV2GitHubEventID(target.RunID, "reconcile", target.Repository,
				strconv.Itoa(target.Number), verified.HeadSHA, verified.State,
				verified.Provenance.ObservedAt.UTC().Format(time.RFC3339Nano))
			targetCtx, cancel = context.WithTimeout(ctx, workflowV2GitHubReconcileTimeout)
			_, reconcileErr := store.ReconcilePullRequest(targetCtx, target, eventID, kind, verified)
			cancel()
			if reconcileErr != nil {
				log.Printf("[workflow-v2 github] reconcile %s#%d for run %s: %v",
					target.Repository, target.Number, target.RunID, reconcileErr)
				continue
			}
		}
		if target.HeadSHA != verified.HeadSHA {
			target.HeadObservedAt = verified.Provenance.ObservedAt.UTC()
		}
		target.HeadSHA = verified.HeadSHA
		target.State = verified.State
		if verified.State == "open" {
			current = append(current, target)
		}
	}

	for _, group := range groupWorkflowV2DeliveryTargets(current, func(target workflowv2.DeliveryTarget) string {
		return target.Repository + "\x00" + target.HeadSHA
	}) {
		if !defaultGitHubClient.allowLowPriority() {
			break
		}
		targetCtx, cancel := context.WithTimeout(ctx, workflowV2GitHubReconcileTimeout)
		err := s.reconcileWorkflowV2GitHubChecks(targetCtx, store, group, group[0].HeadSHA)
		cancel()
		if err != nil && ctx.Err() == nil {
			log.Printf("[workflow-v2 github] reconcile CI for %s@%s: %v",
				group[0].Repository, group[0].HeadSHA, err)
		}
	}

	for _, group := range groupWorkflowV2DeliveryTargets(current, func(target workflowv2.DeliveryTarget) string {
		return target.Repository + "\x00" + strconv.Itoa(target.Number)
	}) {
		if !defaultGitHubClient.allowLowPriority() {
			break
		}
		targetCtx, cancel := context.WithTimeout(ctx, workflowV2GitHubReconcileTimeout)
		err := s.reconcileWorkflowV2GitHubReviews(targetCtx, store, group)
		cancel()
		if err != nil && ctx.Err() == nil {
			log.Printf("[workflow-v2 github] reconcile reviews for %s#%d: %v",
				group[0].Repository, group[0].Number, err)
		}
		if !defaultGitHubClient.allowLowPriority() {
			break
		}
		targetCtx, cancel = context.WithTimeout(ctx, workflowV2GitHubReconcileTimeout)
		err = s.reconcileWorkflowV2GitHubComments(targetCtx, store, group)
		cancel()
		if err != nil && ctx.Err() == nil {
			log.Printf("[workflow-v2 github] reconcile comments for %s#%d: %v",
				group[0].Repository, group[0].Number, err)
		}
	}
	return ctx.Err()
}

func workflowV2ReconciledPREventKind(target workflowv2.DeliveryTarget,
	verified typesv2.VerifiedPullRequest) string {
	switch verified.State {
	case "merged":
		if target.State != verified.State || target.HeadSHA != verified.HeadSHA {
			return "pull_request.merged"
		}
	case "closed":
		if target.State != verified.State || target.HeadSHA != verified.HeadSHA {
			return "pull_request.closed"
		}
	case "open":
		if target.State != verified.State {
			return "pull_request.verified_open"
		}
		if target.HeadSHA != verified.HeadSHA {
			return "pull_request.head_changed"
		}
	default:
	}
	return ""
}

func groupWorkflowV2DeliveryTargets(targets []workflowv2.DeliveryTarget,
	key func(workflowv2.DeliveryTarget) string) [][]workflowv2.DeliveryTarget {
	groups := map[string][]workflowv2.DeliveryTarget{}
	var keys []string
	for _, target := range targets {
		value := key(target)
		if _, ok := groups[value]; !ok {
			keys = append(keys, value)
		}
		groups[value] = append(groups[value], target)
	}
	sort.Strings(keys)
	result := make([][]workflowv2.DeliveryTarget, 0, len(keys))
	for _, value := range keys {
		result = append(result, groups[value])
	}
	return result
}

type workflowV2GitHubReview struct {
	ID          int64  `json:"id"`
	State       string `json:"state"`
	Body        string `json:"body"`
	HTMLURL     string `json:"html_url"`
	CommitID    string `json:"commit_id"`
	SubmittedAt string `json:"submitted_at"`
	User        struct {
		Login string `json:"login"`
		Type  string `json:"type"`
	} `json:"user"`
}

func (s *Server) reconcileWorkflowV2GitHubReviews(ctx context.Context, store *workflowv2.Store,
	targets []workflowv2.DeliveryTarget) error {
	if len(targets) == 0 {
		return nil
	}
	token := s.tokenForRepo(targets[0].Repository)
	if token == "" {
		return fmt.Errorf("no repository-scoped token")
	}
	rawReviews, err := githubAPICollectionWithBaseContext(ctx, s.ghBaseURL(), fmt.Sprintf(
		"repos/%s/pulls/%d/reviews?per_page=100", targets[0].Repository, targets[0].Number), token, "")
	if err != nil {
		return err
	}
	reviewsJSON, _ := json.Marshal(rawReviews)
	var reviews []workflowV2GitHubReview
	if err := json.Unmarshal(reviewsJSON, &reviews); err != nil {
		return fmt.Errorf("decode reviews: %w", err)
	}
	for _, target := range targets {
		connections := workflowV2GitHubReviewConnections(target)
		if len(connections) == 0 {
			continue
		}
		type observedReview struct {
			review   workflowV2GitHubReview
			observed time.Time
		}
		latest := map[string]observedReview{}
		for _, review := range reviews {
			observed, parseErr := time.Parse(time.RFC3339Nano, strings.TrimSpace(review.SubmittedAt))
			status := strings.ToLower(strings.TrimSpace(review.State))
			if parseErr != nil || review.CommitID != target.HeadSHA || status == "" ||
				!s.isHumanGitHubActor(review.User.Login, review.User.Type) {
				continue
			}
			login := strings.TrimSpace(review.User.Login)
			if current, ok := latest[login]; !ok || observed.After(current.observed) {
				latest[login] = observedReview{review: review, observed: observed.UTC()}
			}
		}
		ordered := make([]observedReview, 0, len(latest))
		for _, review := range latest {
			ordered = append(ordered, review)
		}
		sort.Slice(ordered, func(i, j int) bool { return ordered[i].observed.Before(ordered[j].observed) })
		scopes := make([]workflowv2.EvidenceScope, 0, len(connections))
		inputs := make([]workflowv2.EvidenceInput, 0, len(connections)*len(ordered))
		for _, connection := range connections {
			scopes = append(scopes, workflowv2.EvidenceScope{RunID: target.RunID, PRID: target.PRID,
				HeadSHA: target.HeadSHA, Domain: "review", Connection: connection})
			for _, item := range ordered {
				review := item.review
				status := strings.ToLower(strings.TrimSpace(review.State))
				inputs = append(inputs, workflowv2.EvidenceInput{RunID: target.RunID, PRID: target.PRID,
					HeadSHA: target.HeadSHA, Domain: "review", Connection: connection,
					ExternalID: review.User.Login, Kind: "approval", Status: status, ObservedAt: item.observed,
					Payload: map[string]interface{}{"review_id": review.ID, "url": review.HTMLURL,
						"body": review.Body, "commit_id": review.CommitID},
					Provenance: typesv2.EvidenceProvenance{Producer: string(workflowv2.ProducerReview),
						Connection: connection, Principal: review.User.Login,
						ExternalID: strconv.FormatInt(review.ID, 10), ObservedAt: item.observed, Reconciled: true}})
			}
		}
		// Apply feedback oldest-to-newest so the current typed fact is stable
		// even when GitHub returns reviews in an unexpected order.
		for _, item := range ordered {
			if !strings.EqualFold(item.review.State, "changes_requested") {
				continue
			}
			feedback := map[string]interface{}{"kind": "review", "author": item.review.User.Login,
				"body": item.review.Body, "url": item.review.HTMLURL, "repository": target.Repository,
				"number": target.Number, "pull_request_url": target.URL}
			rawID := strconv.FormatInt(item.review.ID, 10)
			eventID := workflowV2GitHubFeedbackEventID(target, "review", rawID)
			if _, err := store.ReconcileReviewFeedback(ctx, target, eventID, feedback, typesv2.EvidenceProvenance{
				Producer: string(workflowv2.ProducerReview), Principal: item.review.User.Login,
				ExternalID: "review:" + rawID, ObservedAt: item.observed, Reconciled: true,
			}); err != nil && ctx.Err() == nil {
				log.Printf("[workflow-v2 github] reconcile feedback for run %s: %v", target.RunID, err)
			}
		}
		if _, err := store.ReconcileEvidenceSnapshot(ctx, scopes, inputs, workflowv2.ProducerReview); err != nil {
			return fmt.Errorf("run %s: %w", target.RunID, err)
		}
	}
	return nil
}

type workflowV2GitHubComment struct {
	ID        int64  `json:"id"`
	Body      string `json:"body"`
	HTMLURL   string `json:"html_url"`
	Path      string `json:"path"`
	Line      int    `json:"line"`
	CommitID  string `json:"commit_id"`
	CreatedAt string `json:"created_at"`
	UpdatedAt string `json:"updated_at"`
	User      struct {
		Login string `json:"login"`
		Type  string `json:"type"`
	} `json:"user"`
}

func (s *Server) reconcileWorkflowV2GitHubComments(ctx context.Context, store *workflowv2.Store,
	targets []workflowv2.DeliveryTarget) error {
	if len(targets) == 0 {
		return nil
	}
	token := s.tokenForRepo(targets[0].Repository)
	if token == "" {
		return fmt.Errorf("no repository-scoped token")
	}
	load := func(requestPath string) ([]workflowV2GitHubComment, error) {
		raw, err := githubAPICollectionWithBaseContext(ctx, s.ghBaseURL(), requestPath, token, "")
		if err != nil {
			return nil, err
		}
		encoded, _ := json.Marshal(raw)
		var comments []workflowV2GitHubComment
		if err := json.Unmarshal(encoded, &comments); err != nil {
			return nil, err
		}
		return comments, nil
	}
	inline, err := load(fmt.Sprintf("repos/%s/pulls/%d/comments?per_page=100",
		targets[0].Repository, targets[0].Number))
	if err != nil {
		return fmt.Errorf("list review comments: %w", err)
	}
	discussion, err := load(fmt.Sprintf("repos/%s/issues/%d/comments?per_page=100",
		targets[0].Repository, targets[0].Number))
	if err != nil {
		return fmt.Errorf("list pull request comments: %w", err)
	}
	for _, target := range targets {
		for _, comment := range inline {
			if comment.CommitID != target.HeadSHA {
				continue
			}
			s.reconcileWorkflowV2GitHubComment(ctx, store, target, comment, "inline_comment")
		}
		for _, comment := range discussion {
			created, parseErr := time.Parse(time.RFC3339Nano, strings.TrimSpace(comment.CreatedAt))
			if parseErr != nil {
				continue
			}
			created = created.UTC()
			if target.HeadObservedAt.IsZero() || created.Before(target.HeadObservedAt) {
				continue
			}
			s.reconcileWorkflowV2GitHubComment(ctx, store, target, comment, "pull_request_comment")
		}
	}
	return nil
}

func (s *Server) reconcileWorkflowV2GitHubComment(ctx context.Context, store *workflowv2.Store,
	target workflowv2.DeliveryTarget, comment workflowV2GitHubComment, kind string) {
	if strings.TrimSpace(comment.Body) == "" ||
		!s.isHumanGitHubActor(comment.User.Login, comment.User.Type) {
		return
	}
	observed, err := time.Parse(time.RFC3339Nano, strings.TrimSpace(comment.UpdatedAt))
	if err != nil {
		return
	}
	observed = observed.UTC()
	feedback := map[string]interface{}{"kind": kind, "author": comment.User.Login, "body": comment.Body,
		"url": comment.HTMLURL, "repository": target.Repository, "number": target.Number,
		"pull_request_url": target.URL}
	if comment.Path != "" {
		feedback["path"] = comment.Path
	}
	if comment.Line > 0 {
		feedback["line"] = comment.Line
	}
	rawID := strconv.FormatInt(comment.ID, 10)
	eventID := workflowV2GitHubFeedbackEventID(target, kind, rawID)
	if _, err := store.ReconcileReviewFeedback(ctx, target, eventID, feedback, typesv2.EvidenceProvenance{
		Producer: string(workflowv2.ProducerReview), Principal: comment.User.Login,
		ExternalID: kind + ":" + rawID, ObservedAt: observed, Reconciled: true,
	}); err != nil && ctx.Err() == nil {
		log.Printf("[workflow-v2 github] reconcile %s for run %s: %v", kind, target.RunID, err)
	}
}
