package hub

import (
	"context"
	"crypto/rsa"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/elasticclaw/elasticclaw/pkg/types"
	"github.com/golang-jwt/jwt/v5"
)

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

// NewGitHubTokenProvider creates a provider, loading the private key from config.
func NewGitHubTokenProvider(cfg *types.GitHubAppConfig) (*GitHubTokenProvider, error) {
	pem, err := loadPrivateKeyPEM(cfg)
	if err != nil {
		return nil, fmt.Errorf("github app private key: %w", err)
	}
	key, err := parseRSAPrivateKey(pem)
	if err != nil {
		return nil, fmt.Errorf("github app private key parse: %w", err)
	}
	return &GitHubTokenProvider{cfg: cfg, privateKey: key}, nil
}

func loadPrivateKeyPEM(cfg *types.GitHubAppConfig) (string, error) {
	if cfg.PrivateKeyPEM != "" {
		return cfg.PrivateKeyPEM, nil
	}
	if cfg.PrivateKeyFile != "" {
		data, err := os.ReadFile(cfg.PrivateKeyFile)
		if err != nil {
			return "", fmt.Errorf("read private key file %s: %w", cfg.PrivateKeyFile, err)
		}
		return string(data), nil
	}
	return "", fmt.Errorf("no private_key_pem or private_key_file configured")
}

func parseRSAPrivateKey(pemStr string) (*rsa.PrivateKey, error) {
	block, _ := pem.Decode([]byte(pemStr))
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

// appJWT generates a signed JWT for authenticating as the GitHub App (10 min validity).
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

// InstallationToken mints a fresh installation access token scoped to the given repos.
// If repos is empty, the token is scoped to all repos the installation has access to.
func (p *GitHubTokenProvider) InstallationToken(ctx context.Context, installationID int64, repos []string) (string, time.Time, error) {
	appJWT, err := p.appJWT()
	if err != nil {
		return "", time.Time{}, fmt.Errorf("sign app jwt: %w", err)
	}

	// Build request body
	var body string
	if len(repos) > 0 {
		// Extract just the repo name (not owner/repo) — GitHub API wants repo names
		repoNames := make([]string, 0, len(repos))
		for _, r := range repos {
			parts := strings.SplitN(r, "/", 2)
			if len(parts) == 2 {
				repoNames = append(repoNames, parts[1])
			} else {
				repoNames = append(repoNames, r)
			}
		}
		b, _ := json.Marshal(map[string]interface{}{"repositories": repoNames})
		body = string(b)
	}

	url := fmt.Sprintf("https://api.github.com/app/installations/%d/access_tokens", installationID)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, strings.NewReader(body))
	if err != nil {
		return "", time.Time{}, err
	}
	req.Header.Set("Authorization", "Bearer "+appJWT)
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")
	if body != "" {
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
