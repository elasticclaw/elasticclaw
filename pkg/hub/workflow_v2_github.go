package hub

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"

	"github.com/elasticclaw/elasticclaw/pkg/hub/workflowv2"
	typesv2 "github.com/elasticclaw/elasticclaw/pkg/types/v2"
)

func (s *Server) workflowV2PullRequestVerifier() workflowv2.PullRequestVerifier {
	return workflowv2.PullRequestVerifierFunc(s.verifyWorkflowV2PullRequest)
}

// verifyWorkflowV2PullRequest treats the claw URL only as a claim. Repository,
// number, branches, state, and head are resolved through the hub's trusted
// GitHub API connection before the runtime can use them.
func (s *Server) verifyWorkflowV2PullRequest(ctx context.Context, run workflowv2.Run, workspace *typesv2.Workspace,
	claim typesv2.PullRequestClaim) (typesv2.VerifiedPullRequest, error) {
	if err := ctx.Err(); err != nil {
		return typesv2.VerifiedPullRequest{}, err
	}
	_ = run // run identity is bound by SubmitDelivery before this verifier is invoked.
	repository, number, err := parseWorkflowV2GitHubPRURL(claim.URL)
	if err != nil {
		return typesv2.VerifiedPullRequest{}, err
	}
	repositoryName := ""
	repositoryConfig := typesv2.Repository{}
	for name, candidate := range workspace.Repositories {
		if candidate.Repository == repository {
			repositoryName, repositoryConfig = name, candidate
			break
		}
	}
	if repositoryName == "" {
		return typesv2.VerifiedPullRequest{}, fmt.Errorf("repository %q is not in workspace %q", repository, workspace.Name)
	}
	if !strings.EqualFold(strings.TrimSpace(repositoryConfig.Provider), "github") {
		return typesv2.VerifiedPullRequest{}, fmt.Errorf("repository %q uses unsupported provider %q", repositoryName, repositoryConfig.Provider)
	}
	data, err := githubAPIWithBaseContext(ctx, s.ghBaseURL(), fmt.Sprintf("repos/%s/pulls/%d", repository, number), s.tokenForRepo(repository))
	if err != nil {
		return typesv2.VerifiedPullRequest{}, err
	}
	canonicalURL, _ := data["html_url"].(string)
	if canonicalURL != strings.TrimSpace(claim.URL) {
		return typesv2.VerifiedPullRequest{}, fmt.Errorf("GitHub returned canonical URL %q for claim %q", canonicalURL, claim.URL)
	}
	head, _ := data["head"].(map[string]interface{})
	base, _ := data["base"].(map[string]interface{})
	headSHA, _ := head["sha"].(string)
	headRef, _ := head["ref"].(string)
	baseRef, _ := base["ref"].(string)
	state, _ := data["state"].(string)
	merged, _ := data["merged"].(bool)
	if merged {
		state = "merged"
	}
	if state != "open" && state != "closed" && state != "merged" {
		return typesv2.VerifiedPullRequest{}, fmt.Errorf("GitHub returned unsupported PR state %q", state)
	}
	externalID := fmt.Sprint(data["id"])
	if externalID == "<nil>" {
		externalID = repository + "#" + strconv.Itoa(number)
	}
	observed := now().UTC()
	connection := strings.TrimSpace(repositoryConfig.SourceControl)
	if connection == "" {
		connection = "github"
	}
	return typesv2.VerifiedPullRequest{
		URL: canonicalURL, Repository: repository, RepositoryName: repositoryName, Number: number,
		SourceBranch: headRef, BaseBranch: baseRef, HeadSHA: headSHA, State: state, VerifiedAt: observed,
		Provenance: typesv2.EvidenceProvenance{Producer: string(workflowv2.ProducerSourceControl),
			Connection: connection, ExternalID: externalID, ObservedAt: observed, Reconciled: true},
	}, nil
}

func githubAPIWithBaseContext(ctx context.Context, baseURL, path, token string) (map[string]interface{}, error) {
	resp, err := defaultGitHubClient.getContext(ctx, strings.TrimRight(baseURL, "/")+"/"+strings.TrimLeft(path, "/"), token)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode >= http.StatusBadRequest {
		return nil, &githubAPIError{StatusCode: resp.StatusCode, Body: string(resp.Body), RateLimited: resp.rateLimited()}
	}
	var result map[string]interface{}
	if err := json.Unmarshal(resp.Body, &result); err != nil {
		return nil, fmt.Errorf("github API parse error: %w", err)
	}
	return result, nil
}

// githubAPICollectionWithBaseContext follows GitHub's Link pagination for a
// named top-level collection. Every next URL must remain on the configured API
// origin before the repository-scoped token is sent.
func githubAPICollectionWithBaseContext(ctx context.Context, baseURL, requestPath, token, key string) ([]json.RawMessage, error) {
	base, err := url.Parse(strings.TrimRight(baseURL, "/"))
	if err != nil || base.Scheme == "" || base.Host == "" {
		return nil, fmt.Errorf("invalid GitHub API base URL")
	}
	next := strings.TrimRight(baseURL, "/") + "/" + strings.TrimLeft(requestPath, "/")
	visited := map[string]bool{}
	var result []json.RawMessage
	for page := 0; next != ""; page++ {
		if page >= 100 || visited[next] {
			return nil, fmt.Errorf("GitHub API pagination exceeded safe limit")
		}
		visited[next] = true
		parsed, err := url.Parse(next)
		if err != nil || !strings.EqualFold(parsed.Scheme, base.Scheme) || !strings.EqualFold(parsed.Host, base.Host) {
			return nil, fmt.Errorf("GitHub API pagination left configured origin")
		}
		resp, err := defaultGitHubClient.getContext(ctx, next, token)
		if err != nil {
			return nil, err
		}
		if resp.StatusCode >= http.StatusBadRequest {
			return nil, &githubAPIError{StatusCode: resp.StatusCode, Body: string(resp.Body), RateLimited: resp.rateLimited()}
		}
		var document map[string]json.RawMessage
		if err := json.Unmarshal(resp.Body, &document); err != nil {
			return nil, fmt.Errorf("github API parse error: %w", err)
		}
		var pageItems []json.RawMessage
		if err := json.Unmarshal(document[key], &pageItems); err != nil {
			return nil, fmt.Errorf("github API collection %q parse error: %w", key, err)
		}
		result = append(result, pageItems...)
		next = githubNextPage(resp.Header.Get("Link"))
	}
	return result, nil
}

func githubNextPage(link string) string {
	for _, entry := range strings.Split(link, ",") {
		parts := strings.Split(entry, ";")
		if len(parts) < 2 {
			continue
		}
		relNext := false
		for _, parameter := range parts[1:] {
			if strings.TrimSpace(parameter) == `rel="next"` {
				relNext = true
				break
			}
		}
		if relNext {
			return strings.TrimSuffix(strings.TrimPrefix(strings.TrimSpace(parts[0]), "<"), ">")
		}
	}
	return ""
}

func parseWorkflowV2GitHubPRURL(raw string) (string, int, error) {
	parsed, err := url.Parse(strings.TrimSpace(raw))
	if err != nil || parsed.Scheme != "https" || parsed.Host == "" || parsed.RawQuery != "" || parsed.Fragment != "" {
		return "", 0, fmt.Errorf("pull request URL must be an absolute HTTPS GitHub URL without query or fragment")
	}
	parts := strings.Split(strings.Trim(parsed.Path, "/"), "/")
	if len(parts) != 4 || parts[2] != "pull" || parts[0] == "" || parts[1] == "" {
		return "", 0, fmt.Errorf("pull request URL path must be /owner/repository/pull/number")
	}
	number, err := strconv.Atoi(parts[3])
	if err != nil || number <= 0 {
		return "", 0, fmt.Errorf("pull request URL has invalid number %q", parts[3])
	}
	return parts[0] + "/" + parts[1], number, nil
}
