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
// repositories the installation can see.
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
		if needsWrite && p.installationHasWorkflowsWrite(ctx, installationID) {
			perms["workflows"] = "write"
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

// installationHasWorkflowsWrite reports whether this installation was granted
// workflows write (or admin). App-level config is not enough: an org may still
// be on an older permission set until they accept the App update. On lookup
// failure, returns false so we omit the scope and still mint a working token.
func (p *GitHubTokenProvider) installationHasWorkflowsWrite(ctx context.Context, installationID int64) bool {
	if installationID == 0 {
		return false
	}
	perms, err := p.installationPermissions(ctx, installationID)
	if err != nil || perms == nil {
		return false
	}
	level := strings.ToLower(strings.TrimSpace(perms["workflows"]))
	return level == "write" || level == "admin"
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
