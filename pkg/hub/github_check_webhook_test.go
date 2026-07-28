package hub

import (
	"database/sql"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/elasticclaw/elasticclaw/pkg/types"
)

// checkWebhookFixture wires a server that (a) accepts signed GitHub webhooks
// with the shared test secret, (b) can resolve a GitHub App installation token
// without reaching api.github.com, and (c) answers PR/check-run reads from a
// stub whose call count is observable.
func checkWebhookFixture(t *testing.T, clawID, headSHA, checkRunsJSON string) (*Server, *sql.DB, clawPR, *atomic.Int32) {
	t.Helper()

	oldTransport := http.DefaultTransport
	http.DefaultTransport = githubAppTokenTransport{base: oldTransport}
	t.Cleanup(func() { http.DefaultTransport = oldTransport })

	var calls atomic.Int32
	gh := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		if strings.Contains(r.URL.Path, "/check-runs") {
			_, _ = w.Write([]byte(`{"check_runs":` + checkRunsJSON + `}`))
			return
		}
		_, _ = w.Write([]byte(`{"head":{"sha":"` + headSHA + `"},"state":"open"}`))
	}))
	t.Cleanup(gh.Close)

	cfg := &types.HubConfig{
		ClawToken:  "test-claw-token",
		GitHubApps: []*types.GitHubAppConfig{{AppID: 1, PrivateKeyPEM: testGitHubAppPEM(t)}},
		Factories: []*types.FactoryConfig{{
			Name:          "github-prs",
			Integration:   "github",
			Template:      "elasticclaw",
			Provider:      "noop",
			Repos:         []string{"owner/repo"},
			WebhookSecret: "test-webhook-secret",
			Trigger:       &types.GitHubTrigger{On: "pull_request"},
		}},
		Providers: map[string]types.ProviderConfig{"noop": {Type: "noop"}},
	}
	s, db := NewTestServerWithConfig(t, cfg, gh.URL, "", "")

	if _, err := db.Exec(`INSERT INTO claws(id,tenant_id,name,template,provider,status,created_at) VALUES(?,?,?,?,?,?,?)`,
		clawID, "test-tenant-id", clawID, "elasticclaw", "noop", "connected", now()); err != nil {
		t.Fatal(err)
	}
	pr := clawPR{id: "pr-" + clawID, clawID: clawID, repo: "owner/repo", prNumber: 42, prURL: "https://github.com/owner/repo/pull/42"}
	if _, err := db.Exec(`INSERT INTO claw_prs(id,claw_id,repo,pr_number,pr_url,created_at) VALUES(?,?,?,?,?,?)`,
		pr.id, pr.clawID, pr.repo, pr.prNumber, pr.prURL, now()); err != nil {
		t.Fatal(err)
	}
	return s, db, pr, &calls
}

func checkWebhookBody(event, action, headSHA string, prNumbers ...int) map[string]interface{} {
	prs := []interface{}{}
	for _, n := range prNumbers {
		prs = append(prs, map[string]interface{}{"number": n})
	}
	sub := map[string]interface{}{
		"name":          "verify",
		"status":        "completed",
		"conclusion":    "success",
		"head_sha":      headSHA,
		"pull_requests": prs,
	}
	key := "check_run"
	if event == "check_suite" {
		key = "check_suite"
		delete(sub, "name")
	}
	return map[string]interface{}{
		"action":     action,
		key:          sub,
		"repository": map[string]interface{}{"full_name": "owner/repo"},
	}
}

// waitForGHCalls waits until the stub GitHub has served at least want requests.
// The webhook handler runs under safeGo, so HTTP 200 returns before the work.
func waitForGHCalls(t *testing.T, calls *atomic.Int32, want int32) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for calls.Load() < want {
		if time.Now().After(deadline) {
			t.Fatalf("GitHub calls = %d, want >= %d", calls.Load(), want)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func greenMessageCount(t *testing.T, db *sql.DB, clawID string) int {
	t.Helper()
	n := 0
	for _, m := range ciMessages(t, db, clawID) {
		if strings.Contains(m, "All CI checks passed on PR #42") {
			n++
		}
	}
	return n
}

const allGreenCheckRuns = `[{"name":"verify","status":"completed","conclusion":"success"},
                            {"name":"gitleaks","status":"completed","conclusion":"success"}]`

// T1: a completed check_run for a fully green head SHA wakes the idle claw.
func TestCheckWebhookGreenWakesIdleClawOnce(t *testing.T) {
	const clawID = "claw-check-webhook-green"
	const headSHA = "04cc3f49aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	s, db, pr, _ := checkWebhookFixture(t, clawID, headSHA, allGreenCheckRuns)

	postSignedGitHubWebhook(t, s, "check_run", checkWebhookBody("check_run", "completed", headSHA, 42))

	waitForMessageContains(t, db, clawID, "hub", []string{"All CI checks passed on PR #42", "04cc3f4"})
	if got := greenMessageCount(t, db, clawID); got != 1 {
		t.Fatalf("green messages = %d, want 1", got)
	}
	sha, conclusion := ciWatermark(t, db, pr.id)
	if sha != headSHA || conclusion != ciConclusionSuccess {
		t.Fatalf("watermark = (%q,%q), want (%q,success)", sha, conclusion, headSHA)
	}
}

// T8: check_suite takes the same path as check_run.
func TestCheckSuiteWebhookGreenWakesIdleClaw(t *testing.T) {
	const clawID = "claw-check-suite-green"
	const headSHA = "5ee5ee5ee5ee5ee5ee5ee5ee5ee5ee5ee5ee5ee5"
	s, db, pr, _ := checkWebhookFixture(t, clawID, headSHA, allGreenCheckRuns)

	postSignedGitHubWebhook(t, s, "check_suite", checkWebhookBody("check_suite", "completed", headSHA, 42))

	waitForMessageContains(t, db, clawID, "hub", []string{"All CI checks passed on PR #42"})
	if got := greenMessageCount(t, db, clawID); got != 1 {
		t.Fatalf("green messages = %d, want 1", got)
	}
	if sha, conclusion := ciWatermark(t, db, pr.id); sha != headSHA || conclusion != ciConclusionSuccess {
		t.Fatalf("watermark = (%q,%q), want (%q,success)", sha, conclusion, headSHA)
	}
}

// T2: a redelivered event must not produce a second wake-up.
func TestCheckWebhookDuplicateDeliveryNotifiesOnce(t *testing.T) {
	const clawID = "claw-check-webhook-dup"
	const headSHA = "abcdef0123456789abcdef0123456789abcdef01"
	s, db, _, calls := checkWebhookFixture(t, clawID, headSHA, allGreenCheckRuns)

	body := checkWebhookBody("check_run", "completed", headSHA, 42)
	postSignedGitHubWebhook(t, s, "check_run", body)
	waitForMessageContains(t, db, clawID, "hub", []string{"All CI checks passed on PR #42"})
	postSignedGitHubWebhook(t, s, "check_run", body)
	// 2 calls for the first delivery; the second stops after the head-SHA read
	// because the stored watermark already records the success verdict.
	waitForGHCalls(t, calls, 3)

	if got := greenMessageCount(t, db, clawID); got != 1 {
		t.Fatalf("green messages = %d, want 1 (claim must hold across deliveries)", got)
	}
}

// T3: the anti-double-notify regression test. The webhook claims the verdict;
// a poller tick that still holds the pre-claim clawPR value must stay silent.
func TestCheckWebhookThenPollerDoesNotDoubleNotify(t *testing.T) {
	const clawID = "claw-check-webhook-poller"
	const headSHA = "beefbeefbeefbeefbeefbeefbeefbeefbeefbeef"
	s, db, stalePR, _ := checkWebhookFixture(t, clawID, headSHA, allGreenCheckRuns)

	postSignedGitHubWebhook(t, s, "check_run", checkWebhookBody("check_run", "completed", headSHA, 42))
	waitForMessageContains(t, db, clawID, "hub", []string{"All CI checks passed on PR #42"})

	// stalePR carries the empty watermark the poller loaded before the webhook
	// landed: only the conditional UPDATE can stop it.
	s.checkCIStatus(stalePR, "token")

	if got := greenMessageCount(t, db, clawID); got != 1 {
		t.Fatalf("green messages = %d, want 1 (poller re-notified after webhook claim)", got)
	}
}

// T4: one check completing green while a sibling still runs is not a verdict.
func TestCheckWebhookSinglePassingCheckDoesNotAnnounceGreen(t *testing.T) {
	const clawID = "claw-check-webhook-partial"
	const headSHA = "1111111111111111111111111111111111111111"
	partial := `[{"name":"verify","status":"completed","conclusion":"success"},
	             {"name":"e2e","status":"in_progress","conclusion":""}]`
	s, db, pr, calls := checkWebhookFixture(t, clawID, headSHA, partial)

	postSignedGitHubWebhook(t, s, "check_run", checkWebhookBody("check_run", "completed", headSHA, 42))
	waitForGHCalls(t, calls, 2)

	if msgs := ciMessages(t, db, clawID); len(msgs) != 0 {
		t.Fatalf("messages = %v, want none while CI is still running", msgs)
	}
	if sha, conclusion := ciWatermark(t, db, pr.id); sha != "" || conclusion != "" {
		t.Fatalf("watermark = (%q,%q), want empty — advancing it hides the real verdict", sha, conclusion)
	}
}

// A non-green conclusion in the set must never be reported as passing.
func TestCheckWebhookNonGreenConclusionDoesNotAnnounceGreen(t *testing.T) {
	const clawID = "claw-check-webhook-red"
	const headSHA = "2222222222222222222222222222222222222222"
	red := `[{"name":"verify","status":"completed","conclusion":"success"},
	         {"name":"e2e","status":"completed","conclusion":"cancelled","details_url":"https://ci/e2e"}]`
	s, db, pr, calls := checkWebhookFixture(t, clawID, headSHA, red)

	postSignedGitHubWebhook(t, s, "check_run", checkWebhookBody("check_run", "completed", headSHA, 42))
	waitForGHCalls(t, calls, 2)
	waitForMessageContains(t, db, clawID, "user", []string{"CI failed on PR #42"})

	if got := greenMessageCount(t, db, clawID); got != 0 {
		t.Fatalf("green messages = %d, want 0 for a cancelled check", got)
	}
	if _, conclusion := ciWatermark(t, db, pr.id); conclusion != ciConclusionFailure {
		t.Fatalf("conclusion = %q, want failure", conclusion)
	}
}

// T5: non-terminal actions must not spend GitHub API quota.
func TestCheckWebhookNonCompletedActionMakesNoAPICalls(t *testing.T) {
	const clawID = "claw-check-webhook-created"
	const headSHA = "3333333333333333333333333333333333333333"
	s, db, _, calls := checkWebhookFixture(t, clawID, headSHA, allGreenCheckRuns)

	postSignedGitHubWebhook(t, s, "check_run", checkWebhookBody("check_run", "created", headSHA, 42))
	time.Sleep(200 * time.Millisecond)

	if got := calls.Load(); got != 0 {
		t.Fatalf("GitHub calls = %d, want 0 for action=created", got)
	}
	if msgs := ciMessages(t, db, clawID); len(msgs) != 0 {
		t.Fatalf("messages = %v, want none", msgs)
	}
}

// T6: fork PRs arrive with an empty pull_requests array.
func TestCheckWebhookEmptyPullRequestsIsInert(t *testing.T) {
	const clawID = "claw-check-webhook-nopr"
	const headSHA = "4444444444444444444444444444444444444444"
	s, db, _, calls := checkWebhookFixture(t, clawID, headSHA, allGreenCheckRuns)

	postSignedGitHubWebhook(t, s, "check_run", checkWebhookBody("check_run", "completed", headSHA))
	time.Sleep(200 * time.Millisecond)

	if got := calls.Load(); got != 0 {
		t.Fatalf("GitHub calls = %d, want 0 without pull_requests", got)
	}
	if msgs := ciMessages(t, db, clawID); len(msgs) != 0 {
		t.Fatalf("messages = %v, want none", msgs)
	}
}

// T7: a PR nobody tracks must resolve to nothing, quietly.
func TestCheckWebhookUntrackedPRIsIgnored(t *testing.T) {
	const clawID = "claw-check-webhook-untracked"
	const headSHA = "5555555555555555555555555555555555555555"
	s, db, _, calls := checkWebhookFixture(t, clawID, headSHA, allGreenCheckRuns)

	postSignedGitHubWebhook(t, s, "check_run", checkWebhookBody("check_run", "completed", headSHA, 999))
	time.Sleep(200 * time.Millisecond)

	if got := calls.Load(); got != 0 {
		t.Fatalf("GitHub calls = %d, want 0 for an untracked PR", got)
	}
	if msgs := ciMessages(t, db, clawID); len(msgs) != 0 {
		t.Fatalf("messages = %v, want none", msgs)
	}
}

// An unauthenticated path that can wake agents would be a security hole.
func TestCheckWebhookInvalidSignatureRejected(t *testing.T) {
	const clawID = "claw-check-webhook-badsig"
	const headSHA = "6666666666666666666666666666666666666666"
	s, db, _, calls := checkWebhookFixture(t, clawID, headSHA, allGreenCheckRuns)

	payload, err := json.Marshal(checkWebhookBody("check_run", "completed", headSHA, 42))
	if err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest(http.MethodPost, "/api/integrations/github/webhook", strings.NewReader(string(payload)))
	req.Header.Set("X-GitHub-Event", "check_run")
	req.Header.Set("X-Hub-Signature-256", signGitHubWebhookForReviewFeedback(payload, "wrong-secret"))
	rr := httptest.NewRecorder()
	s.Handler().ServeHTTP(rr, req)

	if rr.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", rr.Code)
	}
	time.Sleep(200 * time.Millisecond)
	if got := calls.Load(); got != 0 {
		t.Fatalf("GitHub calls = %d, want 0 for an unsigned delivery", got)
	}
	if msgs := ciMessages(t, db, clawID); len(msgs) != 0 {
		t.Fatalf("messages = %v, want none", msgs)
	}
}
