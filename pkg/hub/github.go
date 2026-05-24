package hub

import (
	"context"
	"crypto/rsa"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"fmt"
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

// GitHubTokenProvider generates installation tokens for a GitHub App.
type GitHubTokenProvider struct {
	cfg        *types.GitHubAppConfig
	privateKey *rsa.PrivateKey
}

// NewGitHubTokenProvider creates a provider from hub config.
func NewGitHubTokenProvider(cfg *types.GitHubAppConfig) (*GitHubTokenProvider, error) {
	key, err := parseRSAPrivateKey(cfg.PrivateKeyPEM)
	if err != nil {
		return nil, fmt.Errorf("github app private key: %w", err)
	}
	return &GitHubTokenProvider{cfg: cfg, privateKey: key}, nil
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

	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, "https://api.github.com/app/installations?per_page=100", nil)
	req.Header.Set("Authorization", "Bearer "+appJWT)
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("github list installations: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("github list installations: status %d", resp.StatusCode)
	}

	var installations []githubInstallation
	if err := json.NewDecoder(resp.Body).Decode(&installations); err != nil {
		return nil, fmt.Errorf("decode installations: %w", err)
	}
	return installations, nil
}

// RepoAccess is a repo + permission level used when minting tokens.
type RepoAccess struct {
	Repo        string // "owner/repo"
	Permissions string // "read" or "write"
}

// InstallationToken mints a fresh installation access token scoped to the given repos.
// installationID is looked up automatically if not provided (0).
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

	// Build request body scoped to repos with correct permissions.
	// GitHub token permissions: contents=read means read-only, contents=write means read+write.
	var bodyStr string
	if len(repos) > 0 {
		repoNames := make([]string, 0, len(repos))
		// Track the highest permission needed across all repos
		needsWrite := false
		for _, r := range repos {
			parts := strings.SplitN(r.Repo, "/", 2)
			name := r.Repo
			if len(parts) == 2 {
				name = parts[1]
			}
			repoNames = append(repoNames, name)
			if r.Permissions == "write" {
				needsWrite = true
			}
		}
		contentsPermission := "read"
		if needsWrite {
			contentsPermission = "write"
		}
		b, _ := json.Marshal(map[string]interface{}{
			"repositories": repoNames,
			"permissions": map[string]string{
				"contents":      contentsPermission,
				"pull_requests": contentsPermission,
				"metadata":      "read",
				"checks":        "read", // needed for gh pr checks / CI status
				"statuses":      "read", // needed for commit status checks
			},
		})
		bodyStr = string(b)
	}

	url := fmt.Sprintf("https://api.github.com/app/installations/%d/access_tokens", installationID)
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

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return "", time.Time{}, fmt.Errorf("github api: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusCreated {
		var errBody map[string]interface{}
		json.NewDecoder(resp.Body).Decode(&errBody)
		return "", time.Time{}, fmt.Errorf("github api %d: %v", resp.StatusCode, errBody["message"])
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

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "https://api.github.com/app", nil)
	if err != nil {
		return nil, fmt.Errorf("build request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+appJWT)
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("github get app: %w", err)
	}
	defer resp.Body.Close()

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
