package hub

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strconv"
	"strings"
	"testing"

	"github.com/elasticclaw/elasticclaw/pkg/types"
)

func TestExpandRepositoryAccess(t *testing.T) {
	available := []githubRepository{
		{Name: "api-infra-prod", FullName: "Acme/api-infra-prod"},
		{Name: "web-infra-dev", FullName: "Acme/web-infra-dev"},
		{Name: "website", FullName: "Acme/website"},
		{Name: "api-infra-prod", FullName: "Other/api-infra-prod"},
	}
	selectors := []types.GitHubRepoAccess{
		{Repo: "*-infra-*", Permissions: "read"},
		{Repo: "acme/api-*", Permissions: "write"},
		{Repo: "Acme/website", Permissions: "read"},
	}

	got, err := expandRepositoryAccess(selectors, available)
	if err != nil {
		t.Fatalf("expandRepositoryAccess: %v", err)
	}
	want := []types.GitHubRepoAccess{
		{Repo: "Acme/api-infra-prod", Permissions: "write"},
		{Repo: "Acme/web-infra-dev", Permissions: "read"},
		{Repo: "Acme/website", Permissions: "read"},
		{Repo: "Other/api-infra-prod", Permissions: "read"},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("expanded repositories = %#v, want %#v", got, want)
	}
}

func TestExpandRepositoryAccessAllRepositories(t *testing.T) {
	available := []githubRepository{
		{Name: "zeta", FullName: "acme/zeta"},
		{Name: "alpha", FullName: "acme/alpha"},
	}
	got, err := expandRepositoryAccess([]types.GitHubRepoAccess{{Repo: "*", Permissions: "write"}}, available)
	if err != nil {
		t.Fatalf("expandRepositoryAccess: %v", err)
	}
	want := []types.GitHubRepoAccess{
		{Repo: "acme/alpha", Permissions: "write"},
		{Repo: "acme/zeta", Permissions: "write"},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("expanded repositories = %#v, want %#v", got, want)
	}
}

func TestExpandRepositoryAccessRejectsUnmatchedPattern(t *testing.T) {
	_, err := expandRepositoryAccess(
		[]types.GitHubRepoAccess{{Repo: "*-infra-*", Permissions: "read"}},
		[]githubRepository{{Name: "website", FullName: "acme/website"}},
	)
	if err == nil || !strings.Contains(err.Error(), `repository pattern "*-infra-*" matched no repositories`) {
		t.Fatalf("error = %v, want unmatched pattern error", err)
	}
}

func TestHasRepositoryGlob(t *testing.T) {
	if hasRepositoryGlob([]types.GitHubRepoAccess{{Repo: "acme/api"}}) {
		t.Fatal("exact repository unexpectedly detected as a glob")
	}
	if !hasRepositoryGlob([]types.GitHubRepoAccess{{Repo: "acme/api-?"}}) {
		t.Fatal("repository pattern was not detected")
	}
}

func TestRepoAccessMatchesSelector(t *testing.T) {
	cases := []struct {
		repo     string
		selector RepoAccess
		want     bool
	}{
		{"acme/api", RepoAccess{Repo: "acme/api"}, true},
		{"acme/api", RepoAccess{Repo: "ACME/API"}, true},
		{"acme/api", RepoAccess{Repo: "other/api"}, false},
		{"acme/icedq-kots", RepoAccess{Repo: "acme/*"}, true},
		{"other/icedq-kots", RepoAccess{Repo: "acme/*"}, false},
		{"acme/icedq-kots", RepoAccess{Repo: "*-kots"}, true},
		{"acme/support-sandbox", RepoAccess{Repo: "*-kots"}, false},
	}
	for _, tc := range cases {
		got := repoAccessMatchesSelector(tc.repo, tc.selector)
		if got != tc.want {
			t.Fatalf("repoAccessMatchesSelector(%q, %q) = %v, want %v", tc.repo, tc.selector.Repo, got, tc.want)
		}
	}
}

func TestEffectiveRepoAccess(t *testing.T) {
	selectors := []RepoAccess{
		{Repo: "replicated-collab/support-sandbox", Permissions: "write"},
		{Repo: "replicated-collab/*", Permissions: "read"},
	}

	got := effectiveRepoAccess("replicated-collab/support-sandbox", selectors)
	if got == nil || got.Permissions != "write" {
		t.Fatalf("exact match should prefer write, got %#v", got)
	}

	got = effectiveRepoAccess("replicated-collab/icedq-kots", selectors)
	if got == nil || got.Permissions != "read" {
		t.Fatalf("glob match should return read, got %#v", got)
	}

	got = effectiveRepoAccess("other-org/icedq-kots", selectors)
	if got != nil {
		t.Fatalf("unmatched repo should return nil, got %#v", got)
	}
}

func TestListInstallationRepositoriesPaginates(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/app/installations/42/access_tokens":
			w.WriteHeader(http.StatusCreated)
			_, _ = w.Write([]byte(`{"token":"installation-token","expires_at":"2030-01-01T00:00:00Z"}`))
		case r.Method == http.MethodGet && r.URL.Path == "/installation/repositories":
			if r.Header.Get("Authorization") != "Bearer installation-token" {
				http.Error(w, "missing installation token", http.StatusUnauthorized)
				return
			}
			page, _ := strconv.Atoi(r.URL.Query().Get("page"))
			repositories := make([]githubRepository, 0, 100)
			start, count := 0, 100
			if page == 2 {
				start, count = 100, 1
			}
			for i := start; i < start+count; i++ {
				repositories = append(repositories, githubRepository{
					Name:     fmt.Sprintf("repo-%03d", i),
					FullName: fmt.Sprintf("acme/repo-%03d", i),
				})
			}
			_ = json.NewEncoder(w).Encode(githubInstallationRepositoriesResponse{
				TotalCount:   101,
				Repositories: repositories,
			})
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	provider := newTestGitHubTokenProvider(t, server)
	repositories, err := provider.ListInstallationRepositories(context.Background(), 42)
	if err != nil {
		t.Fatalf("ListInstallationRepositories: %v", err)
	}
	if len(repositories) != 101 {
		t.Fatalf("repository count = %d, want 101", len(repositories))
	}
	if repositories[100].FullName != "acme/repo-100" {
		t.Fatalf("last repository = %q, want acme/repo-100", repositories[100].FullName)
	}
}

func TestListInstallationRepositoriesIncludesGitHubErrorMessage(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/app/installations/42/access_tokens":
			w.WriteHeader(http.StatusCreated)
			_, _ = w.Write([]byte(`{"token":"installation-token","expires_at":"2030-01-01T00:00:00Z"}`))
		case r.Method == http.MethodGet && r.URL.Path == "/installation/repositories":
			w.WriteHeader(http.StatusForbidden)
			_, _ = w.Write([]byte(`{"message":"Resource not accessible by integration"}`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	provider := newTestGitHubTokenProvider(t, server)
	_, err := provider.ListInstallationRepositories(context.Background(), 42)
	if err == nil || !strings.Contains(err.Error(), "status 403: Resource not accessible by integration") {
		t.Fatalf("error = %v, want GitHub status and message", err)
	}
}

func newTestGitHubTokenProvider(t *testing.T, server *httptest.Server) *GitHubTokenProvider {
	t.Helper()
	privateKey, err := rsa.GenerateKey(rand.Reader, 1024)
	if err != nil {
		t.Fatalf("generate private key: %v", err)
	}
	return &GitHubTokenProvider{
		cfg:        &types.GitHubAppConfig{AppID: 123},
		privateKey: privateKey,
		apiBaseURL: server.URL,
		httpClient: server.Client(),
	}
}

func TestWorkflowCreationStoresRepositorySelectors(t *testing.T) {
	privateKey, err := rsa.GenerateKey(rand.Reader, 1024)
	if err != nil {
		t.Fatalf("generate private key: %v", err)
	}
	privateKeyPEM := string(pem.EncodeToMemory(&pem.Block{
		Type:  "RSA PRIVATE KEY",
		Bytes: x509.MarshalPKCS1PrivateKey(privateKey),
	}))

	github := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/app/installations":
			_, _ = w.Write([]byte(`[{"id":42,"account":{"login":"acme"}}]`))
		case r.Method == http.MethodPost && r.URL.Path == "/app/installations/42/access_tokens":
			w.WriteHeader(http.StatusCreated)
			_, _ = w.Write([]byte(`{"token":"installation-token","expires_at":"2030-01-01T00:00:00Z"}`))
		case r.Method == http.MethodGet && r.URL.Path == "/installation/repositories":
			_ = json.NewEncoder(w).Encode(githubInstallationRepositoriesResponse{
				TotalCount: 2,
				Repositories: []githubRepository{
					{Name: "api-infra-prod", FullName: "acme/api-infra-prod"},
					{Name: "website", FullName: "acme/website"},
				},
			})
		default:
			http.NotFound(w, r)
		}
	}))
	defer github.Close()

	t.Setenv("ELASTICCLAW_HUB_CONFIG", t.TempDir()+"/hub.yaml")
	t.Setenv("ELASTICCLAW_NOOP_PROVIDER", "1")
	s, db := NewTestServerWithConfig(t, &types.HubConfig{
		Token:     "test-token",
		ClawToken: "test-claw-token",
		Providers: map[string]types.ProviderConfig{"noop": {Type: "noop"}},
		GitHubApps: []*types.GitHubAppConfig{{
			AppID:         123,
			PrivateKeyPEM: privateKeyPEM,
		}},
	}, github.URL, "", "")
	workspace := &types.WorkspaceConfig{
		Name: "engineering",
		Repositories: []types.GitHubRepoAccess{
			{Repo: "*-infra-*", Permissions: "write"},
			{Repo: "acme/website", Permissions: "read"},
		},
		Files: map[string]string{
			"elasticclaw-config.yaml": "schema_version: v1\nname: engineering\nprovider: noop\n",
		},
	}
	workflow := &types.WorkflowConfig{Name: "infra-update", Provider: "noop"}

	clawID, _, err := s.createClawFromWorkflowContext(context.Background(), workspace, workflow, nil, "test")
	if err != nil {
		t.Fatalf("createClawFromWorkflow: %v", err)
	}
	var repositoriesJSON string
	if err := db.QueryRow(`SELECT github_repos FROM claws WHERE id=?`, clawID).Scan(&repositoriesJSON); err != nil {
		t.Fatalf("read claw repositories: %v", err)
	}
	var repositories []types.GitHubRepoAccess
	if err := json.Unmarshal([]byte(repositoriesJSON), &repositories); err != nil {
		t.Fatalf("decode claw repositories: %v", err)
	}
	want := []types.GitHubRepoAccess{
		{Repo: "*-infra-*", Permissions: "write"},
		{Repo: "acme/website", Permissions: "read"},
	}
	if !reflect.DeepEqual(repositories, want) {
		t.Fatalf("stored repositories = %#v, want %#v", repositories, want)
	}

	canceledCtx, cancel := context.WithCancel(context.Background())
	cancel()
	_, _, err = s.createClawFromWorkflowContext(canceledCtx, workspace, workflow, nil, "canceled test")
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled workflow creation error = %v, want context.Canceled", err)
	}
	var clawCount int
	if err := db.QueryRow(`SELECT COUNT(*) FROM claws`).Scan(&clawCount); err != nil {
		t.Fatalf("count claws: %v", err)
	}
	if clawCount != 1 {
		t.Fatalf("claw count after canceled expansion = %d, want 1", clawCount)
	}
}
