package hub

import (
	"context"
	"crypto/rsa"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strings"
	"time"

	"github.com/elasticclaw/elasticclaw/pkg/types"
	"github.com/golang-jwt/jwt/v5"
)

// githubInstallation is a GitHub App installation.
type githubInstallation struct {
	ID      int64 `json:"id"`
	Account struct {
		Login string `json:"login"`
	} `json:"account"`
	// Permissions are the scopes granted to this installation (may lag the
	// App's configured permissions until the owner accepts an update).
	Permissions map[string]string `json:"permissions"`
}

// githubAppMeta is the response from GET /app (authenticated as the App).
type githubAppMeta struct {
	Permissions map[string]string `json:"permissions"`
}

// githubTokenResponse is the GitHub API response for an installation access token.
type githubTokenResponse struct {
	Token     string    `json:"token"`
	ExpiresAt time.Time `json:"expires_at"`
}

// githubRepository is the subset of repository metadata needed when expanding
// workspace repository patterns.
type githubRepository struct {
	Name     string `json:"name"`
	FullName string `json:"full_name"`
}

type githubInstallationRepositoriesResponse struct {
	TotalCount   int                `json:"total_count"`
	Repositories []githubRepository `json:"repositories"`
}

// GitHubTokenProvider generates installation tokens for a GitHub App.
type GitHubTokenProvider struct {
	cfg        *types.GitHubAppConfig
	privateKey *rsa.PrivateKey
	apiBaseURL string
	httpClient *http.Client
}

// NewGitHubTokenProvider creates a provider from hub config.
func NewGitHubTokenProvider(cfg *types.GitHubAppConfig) (*GitHubTokenProvider, error) {
	key, err := parseRSAPrivateKey(cfg.PrivateKeyPEM)
	if err != nil {
		return nil, fmt.Errorf("github app private key: %w", err)
	}
	return &GitHubTokenProvider{
		cfg:        cfg,
		privateKey: key,
		apiBaseURL: "https://api.github.com",
		httpClient: &http.Client{Timeout: 30 * time.Second},
	}, nil
}

func (p *GitHubTokenProvider) apiURL(path string) string {
	return strings.TrimRight(p.apiBaseURL, "/") + path
}

func parseRSAPrivateKey(pemStr string) (*rsa.PrivateKey, error) {
	block, _ := pem.Decode([]byte(strings.TrimSpace(pemStr)))
	if block == nil {
		return nil, fmt.Errorf("failed to decode PEM block")
	}
	key, err := x509.ParsePKCS1PrivateKey(block.Bytes)
	if err != nil {
		// Try PKCS8
		keyI, err2 := x509.ParsePKCS8PrivateKey(block.Bytes)
		if err2 != nil {
			return nil, fmt.Errorf("parse private key (PKCS1: %v, PKCS8: %v)", err, err2)
		}
		rsaKey, ok := keyI.(*rsa.PrivateKey)
		if !ok {
			return nil, fmt.Errorf("private key is not RSA")
		}
		return rsaKey, nil
	}
	return key, nil
}

// appJWT generates a signed JWT for authenticating as the GitHub App (9 min validity).
func (p *GitHubTokenProvider) appJWT() (string, error) {
	now := time.Now()
	claims := jwt.RegisteredClaims{
		IssuedAt:  jwt.NewNumericDate(now.Add(-30 * time.Second)), // clock skew tolerance
		ExpiresAt: jwt.NewNumericDate(now.Add(9 * time.Minute)),
		Issuer:    fmt.Sprintf("%d", p.cfg.AppID),
	}
	token := jwt.NewWithClaims(jwt.SigningMethodRS256, claims)
	return token.SignedString(p.privateKey)
}

// FindInstallationForRepos returns the installation ID that has access to all
// the requested repos. It lists all installations of the App and picks the first
// one whose account owns at least one of the repos. If repos is empty, returns
// the first installation found.
func (p *GitHubTokenProvider) FindInstallationForRepos(ctx context.Context, repos []string) (int64, error) {
	installations, err := p.ListInstallations(ctx)
	if err != nil {
		return 0, err
	}
	if len(installations) == 0 {
		return 0, fmt.Errorf("no installations found for GitHub App %d — install the App on your org or repo first", p.cfg.AppID)
	}

	// If no repos specified, return the first installation
	if len(repos) == 0 {
		return installations[0].ID, nil
	}

	// Find the installation whose account owns the repos
	// repos format: "owner/repo" — match by owner login
	for _, inst := range installations {
		owner := strings.ToLower(inst.Account.Login)
		for _, repo := range repos {
			parts := strings.SplitN(repo, "/", 2)
			if len(parts) == 2 && strings.ToLower(parts[0]) == owner {
				return inst.ID, nil
			}
		}
	}

	// Fallback: return first installation and let the token request fail with a clear error
	return installations[0].ID, nil
}

func (p *GitHubTokenProvider) ListInstallations(ctx context.Context) ([]githubInstallation, error) {
	appJWT, err := p.appJWT()
	if err != nil {
		return nil, fmt.Errorf("sign app jwt: %w", err)
	}

	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, p.apiURL("/app/installations?per_page=100"), nil)
	req.Header.Set("Authorization", "Bearer "+appJWT)
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")

	resp, err := p.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("github list installations: %w", err)
	}
	defer resp.Body.Close()
	defaultGitHubClient.observe(resp.StatusCode, resp.Header, nil)

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("github list installations: status %d", resp.StatusCode)
	}

	var installations []githubInstallation
	if err := json.NewDecoder(resp.Body).Decode(&installations); err != nil {
		return nil, fmt.Errorf("decode installations: %w", err)
	}
	return installations, nil
}

// ListInstallationRepositories returns every repository visible to one GitHub
// App installation. GitHub requires an installation token for this endpoint,
// so the provider first mints an unscoped token and then follows pagination.
func (p *GitHubTokenProvider) ListInstallationRepositories(ctx context.Context, installationID int64) ([]githubRepository, error) {
	token, _, err := p.InstallationToken(ctx, installationID, nil)
	if err != nil {
		return nil, fmt.Errorf("create installation token: %w", err)
	}

	const perPage = 100
	var repositories []githubRepository
	for page := 1; ; page++ {
		url := p.apiURL(fmt.Sprintf("/installation/repositories?per_page=%d&page=%d", perPage, page))
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
		if err != nil {
			return nil, fmt.Errorf("build repository list request: %w", err)
		}
		req.Header.Set("Authorization", "Bearer "+token)
		req.Header.Set("Accept", "application/vnd.github+json")
		req.Header.Set("X-GitHub-Api-Version", "2022-11-28")

		resp, err := p.httpClient.Do(req)
		if err != nil {
			return nil, fmt.Errorf("github list installation repositories: %w", err)
		}
		defaultGitHubClient.observe(resp.StatusCode, resp.Header, nil)
		if resp.StatusCode != http.StatusOK {
			var errBody map[string]interface{}
			_ = json.NewDecoder(resp.Body).Decode(&errBody)
			resp.Body.Close()
			return nil, fmt.Errorf("github list installation repositories: status %d: %v", resp.StatusCode, errBody["message"])
		}
		var result githubInstallationRepositoriesResponse
		decodeErr := json.NewDecoder(resp.Body).Decode(&result)
		resp.Body.Close()
		if decodeErr != nil {
			return nil, fmt.Errorf("decode installation repositories: %w", decodeErr)
		}

		repositories = append(repositories, result.Repositories...)
		if len(result.Repositories) < perPage || (result.TotalCount > 0 && len(repositories) >= result.TotalCount) {
			return repositories, nil
		}
	}
}

// RepoAccess is a repo + permission level used when minting tokens.
type RepoAccess struct {
	Repo        string // "owner/repo"
	Permissions string // "read" or "write"
	// ExtraPermissions are granular GitHub App permissions declared by the
	// workspace (issue #697). They are added on top of the default permission
	// set and capped at what the installation actually grants.
	ExtraPermissions map[string]string
}

// mergeRepoExtraPermissions merges src into dst with "write" winning over
// "read" for duplicate keys. Returns the merged map (dst when src is empty).
func mergeRepoExtraPermissions(dst, src map[string]string) map[string]string {
	if len(src) == 0 {
		return dst
	}
	if dst == nil {
		dst = make(map[string]string, len(src))
	}
	for name, level := range src {
		if dst[name] != "write" {
			dst[name] = level
		}
	}
	return dst
}

// collectRepoExtraPermissions merges the granular permissions declared across
// all repos into a single map, mirroring how the scalar level escalates to
// the workspace maximum.
func collectRepoExtraPermissions(repos []RepoAccess) map[string]string {
	var merged map[string]string
	for _, r := range repos {
		merged = mergeRepoExtraPermissions(merged, r.ExtraPermissions)
	}
	return merged
}

// maxScopedInstallationRepos is GitHub's documented limit for the
// `repositories` array on POST /app/installations/{id}/access_tokens.
// Larger claw allowlists still work: we mint a permission-restricted
// installation token (no name list) and git clones prefer single-repo
// tokens via handleGitHubToken ?repo=owner/name.
const maxScopedInstallationRepos = 50

// InstallationToken mints a fresh installation access token scoped to the given repos.
// installationID is looked up automatically if not provided (0).
//
// When repos is empty, GitHub grants the installation's default access to all
// repositories the installation can see — and because no permissions body is
// sent, the token carries *every* permission the installation was granted
// (a superset of any ExtraPermissions the caller's selectors declared).
// Granular permissions therefore only need the explicit merge below for
// non-empty repo lists; see handleGitHubToken's glob branch before narrowing
// this.
//
// When 1 ≤ len(repos) ≤ maxScopedInstallationRepos, the token is restricted to
// that explicit name allowlist.
//
// When len(repos) > maxScopedInstallationRepos, GitHub cannot accept a longer
// name list. We still mint a token with the requested permission levels but
// omit the repositories field (installation-visible repos only). Callers that
// need least-privilege for individual clones should pass a single-element
// repos slice (credential helper ?repo=).
func (p *GitHubTokenProvider) InstallationToken(ctx context.Context, installationID int64, repos []RepoAccess) (string, time.Time, error) {
	// Auto-discover installation ID if not set
	if installationID == 0 {
		var err error
		repoStrs := make([]string, len(repos))
		for i, r := range repos {
			repoStrs[i] = r.Repo
		}
		installationID, err = p.FindInstallationForRepos(ctx, repoStrs)
		if err != nil {
			return "", time.Time{}, fmt.Errorf("find installation: %w", err)
		}
	}

	appJWT, err := p.appJWT()
	if err != nil {
		return "", time.Time{}, fmt.Errorf("sign app jwt: %w", err)
	}

	// Build request body with correct permissions.
	// Installation tokens only receive the permissions listed here (capped by
	// what this installation was granted). Requesting a scope the installation
	// lacks makes POST /access_tokens fail — so optional scopes like workflows
	// are included only after GET /app/installations/{id} confirms write/admin.
	// contents=write alone is not enough to create or update files under
	// .github/workflows/; GitHub requires the workflows scope when available.
	var bodyStr string
	if len(repos) > 0 {
		needsWrite := false
		for _, r := range repos {
			if r.Permissions == "write" {
				needsWrite = true
				break
			}
		}
		contentsPermission := "read"
		if needsWrite {
			contentsPermission = "write"
		}
		perms := map[string]string{
			"contents":      contentsPermission,
			"pull_requests": contentsPermission,
			"metadata":      "read",
			"checks":        "read", // needed for gh pr checks / CI status
			"statuses":      "read", // needed for commit status checks
		}

		// Query the installation's actual granted permissions once so we can add
		// optional scopes (workflows, issues) only when the installation has been
		// granted them. Requesting a scope the installation lacks makes the token
		// mint fail, and we want gh to be able to read issues/comments in other
		// configured repos when the App allows it.
		instPerms, _ := p.installationPermissions(ctx, installationID)

		if needsWrite {
			if level := installationPermissionLevel(instPerms, "workflows"); level == "write" || level == "admin" {
				perms["workflows"] = "write"
			}
		}
		if level := installationPermissionLevel(instPerms, "issues"); level != "" {
			perms["issues"] = level
		}

		// Granular workspace-declared permissions (issue #697): a workspace
		// v2 repository may declare extra GitHub App permissions (e.g.
		// vulnerability_alerts / security_events read so workflows can read
		// Dependabot and code-scanning alerts). Defaults above are unchanged;
		// declared entries only add or override. Each entry is included only
		// when the installation actually grants that permission, and is capped
		// at the installation's level, so installations without the grant keep
		// minting successfully (same safety net as workflows/issues).
		//
		// Levels are canonicalized here (anything other than "write" becomes
		// "read") so garbage persisted in github_repos cannot produce an
		// invalid permissions object and fail the mint for the whole claw.
		// Unknown permission names are gated out by the installation lookup,
		// which only reports names GitHub actually grants.
		if extras := collectRepoExtraPermissions(repos); len(extras) > 0 {
			names := make([]string, 0, len(extras))
			for name := range extras {
				names = append(names, name)
			}
			sort.Strings(names)
			for _, name := range names {
				if name == "metadata" {
					continue // metadata is always read; never widened
				}
				requested := strings.ToLower(strings.TrimSpace(extras[name]))
				if requested != "write" {
					requested = "read"
				}
				granted := installationPermissionLevel(instPerms, name)
				if granted == "" {
					continue // installation lacks this permission; requesting it would fail the mint
				}
				if requested == "write" && granted != "write" && granted != "admin" {
					requested = "read" // cap at installation level
				}
				if perms[name] == "write" {
					continue // never narrow an existing write grant
				}
				perms[name] = requested
			}
		}
		body := map[string]interface{}{
			"permissions": perms,
		}
		if len(repos) <= maxScopedInstallationRepos {
			repoNames := make([]string, 0, len(repos))
			for _, r := range repos {
				parts := strings.SplitN(r.Repo, "/", 2)
				name := r.Repo
				if len(parts) == 2 {
					name = parts[1]
				}
				repoNames = append(repoNames, name)
			}
			body["repositories"] = repoNames
		}
		// else: omit repositories — required for 50+ allowlists; git path uses ?repo=
		b, _ := json.Marshal(body)
		bodyStr = string(b)
	}

	url := p.apiURL(fmt.Sprintf("/app/installations/%d/access_tokens", installationID))
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, strings.NewReader(bodyStr))
	if err != nil {
		return "", time.Time{}, err
	}
	req.Header.Set("Authorization", "Bearer "+appJWT)
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")
	if bodyStr != "" {
		req.Header.Set("Content-Type", "application/json")
	}

	resp, err := p.httpClient.Do(req)
	if err != nil {
		return "", time.Time{}, fmt.Errorf("github api: %w", err)
	}
	defer resp.Body.Close()
	defaultGitHubClient.observe(resp.StatusCode, resp.Header, nil)

	if resp.StatusCode != http.StatusCreated {
		body, _ := io.ReadAll(resp.Body)
		defaultGitHubClient.observe(resp.StatusCode, resp.Header, body)
		return "", time.Time{}, &githubAPIError{StatusCode: resp.StatusCode, Body: string(body), RateLimited: githubIsRateLimited(resp.StatusCode, resp.Header, body)}
	}

	var result githubTokenResponse
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return "", time.Time{}, fmt.Errorf("decode github response: %w", err)
	}
	return result.Token, result.ExpiresAt, nil
}

// CheckAppPermissions queries the GitHub App's configured permissions via
// GET /app (authenticated as the App). It returns a map of permission name ->
// granted level ("read", "write", or "").
func (p *GitHubTokenProvider) CheckAppPermissions(ctx context.Context) (map[string]string, error) {
	appJWT, err := p.appJWT()
	if err != nil {
		return nil, fmt.Errorf("sign app jwt: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, p.apiURL("/app"), nil)
	if err != nil {
		return nil, fmt.Errorf("build request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+appJWT)
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")

	resp, err := p.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("github get app: %w", err)
	}
	defer resp.Body.Close()
	defaultGitHubClient.observe(resp.StatusCode, resp.Header, nil)

	if resp.StatusCode != http.StatusOK {
		var errBody map[string]interface{}
		json.NewDecoder(resp.Body).Decode(&errBody)
		return nil, fmt.Errorf("github get app %d: %v", resp.StatusCode, errBody["message"])
	}

	var meta githubAppMeta
	if err := json.NewDecoder(resp.Body).Decode(&meta); err != nil {
		return nil, fmt.Errorf("decode app meta: %w", err)
	}
	return meta.Permissions, nil
}

// installationPermissionLevel returns the normalized permission level granted to
// this installation for a named GitHub permission, or "" if it is not granted
// or the lookup failed. It accepts "read", "write", or "admin".
func installationPermissionLevel(perms map[string]string, name string) string {
	if perms == nil {
		return ""
	}
	level := strings.ToLower(strings.TrimSpace(perms[name]))
	if level == "read" || level == "write" || level == "admin" {
		return level
	}
	return ""
}

// installationPermissions returns the scopes granted to a specific installation
// via GET /app/installations/{id}.
func (p *GitHubTokenProvider) installationPermissions(ctx context.Context, installationID int64) (map[string]string, error) {
	appJWT, err := p.appJWT()
	if err != nil {
		return nil, fmt.Errorf("sign app jwt: %w", err)
	}

	url := p.apiURL(fmt.Sprintf("/app/installations/%d", installationID))
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, fmt.Errorf("build request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+appJWT)
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")

	resp, err := p.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("github get installation: %w", err)
	}
	defer resp.Body.Close()
	defaultGitHubClient.observe(resp.StatusCode, resp.Header, nil)

	if resp.StatusCode != http.StatusOK {
		var errBody map[string]interface{}
		json.NewDecoder(resp.Body).Decode(&errBody)
		return nil, fmt.Errorf("github get installation %d: %v", resp.StatusCode, errBody["message"])
	}

	var inst githubInstallation
	if err := json.NewDecoder(resp.Body).Decode(&inst); err != nil {
		return nil, fmt.Errorf("decode installation: %w", err)
	}
	return inst.Permissions, nil
}

// FindInstallationForRepo returns the installation ID for the authenticated
// GitHub App that has access to a specific repository. This is the cheapest way
// to pick the right app/installation when the hub has multiple GitHub Apps:
// the API directly returns the installation for the repo, so we don't need to
// list every repository of every installation.
func (p *GitHubTokenProvider) FindInstallationForRepo(ctx context.Context, owner, repo string) (int64, error) {
	appJWT, err := p.appJWT()
	if err != nil {
		return 0, fmt.Errorf("sign app jwt: %w", err)
	}

	url := p.apiURL(fmt.Sprintf("/repos/%s/%s/installation", owner, repo))
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return 0, fmt.Errorf("build request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+appJWT)
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")

	resp, err := p.httpClient.Do(req)
	if err != nil {
		return 0, fmt.Errorf("github find installation for repo: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		var errBody map[string]interface{}
		json.NewDecoder(resp.Body).Decode(&errBody)
		return 0, fmt.Errorf("github find installation for repo %s/%s: status %d: %v", owner, repo, resp.StatusCode, errBody["message"])
	}

	var inst struct {
		ID int64 `json:"id"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&inst); err != nil {
		return 0, fmt.Errorf("decode installation for repo: %w", err)
	}
	if inst.ID == 0 {
		return 0, fmt.Errorf("github find installation for repo %s/%s returned no installation id", owner, repo)
	}
	return inst.ID, nil
}
