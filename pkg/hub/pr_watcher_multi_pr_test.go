package hub

import (
	"database/sql"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/elasticclaw/elasticclaw/pkg/types"
)

// multiPRGitHubStub serves repos/{repo}/pulls/{n} responses whose per-PR state
// the test can flip mid-test, so one server can drive a claw through
// "first PR resolved, second still open, second resolves later" sequences.
// The extra "missing" state answers 404 for permanent-failure scenarios.
type multiPRGitHubStub struct {
	mu     sync.Mutex
	states map[int]string // pr number -> "open" | "merged" | "closed" | "missing"
}

func (m *multiPRGitHubStub) set(prNumber int, state string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.states[prNumber] = state
}

func (m *multiPRGitHubStub) handler() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var prNumber int
		parts := strings.Split(strings.Trim(r.URL.Path, "/"), "/")
		fmt.Sscanf(parts[len(parts)-1], "%d", &prNumber)
		m.mu.Lock()
		state := m.states[prNumber]
		m.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		switch state {
		case "missing":
			w.WriteHeader(http.StatusNotFound)
			fmt.Fprint(w, `{"message":"Not Found"}`)
			return
		case "merged":
			fmt.Fprintf(w, `{"state":"closed","merged":true,"merged_at":"2026-01-02T03:04:05Z","created_at":"2026-01-01T00:00:00Z","draft":false,"head":{"sha":"deadbeef"}}`)
		case "closed":
			fmt.Fprintf(w, `{"state":"closed","merged":false,"created_at":"2026-01-01T00:00:00Z","draft":false,"head":{"sha":"deadbeef"}}`)
		default:
			fmt.Fprintf(w, `{"state":"open","merged":false,"created_at":"2026-01-01T00:00:00Z","draft":false,"head":{"sha":"deadbeef"}}`)
		}
	}
}

// multiPRFixture wires a test server against the mutable GitHub stub and
// inserts one claw tracking prNumbers, returning one clawPR per number.
//
// The server carries a GitHubApps config and the process-wide transport mints
// installation tokens: pollAllPRs returns early when githubAppConfigsForTokens
// is empty and skips rows whose token cannot resolve, so without both every
// poll-loop assertion in this file would be vacuous.
func multiPRFixture(t *testing.T, clawID string, prNumbers ...int) (*Server, *sql.DB, *multiPRGitHubStub, []clawPR) {
	t.Helper()
	resetGitHubClientForTest(t)
	oldTransport := http.DefaultTransport
	var mints int64
	http.DefaultTransport = githubAppAnyTokenTransport{base: oldTransport, mints: &mints}
	t.Cleanup(func() { http.DefaultTransport = oldTransport })

	stub := &multiPRGitHubStub{states: map[int]string{}}
	gh := httptest.NewServer(stub.handler())
	t.Cleanup(gh.Close)

	cfg := &types.HubConfig{GitHubApps: []*types.GitHubAppConfig{{AppID: 1, PrivateKeyPEM: testGitHubAppPEM(t)}}}
	s, db := NewTestServerWithConfig(t, cfg, gh.URL, "", "")
	if _, err := db.Exec(`INSERT INTO claws(id,tenant_id,name,template,status,created_at) VALUES(?,?,?,?,?,?)`, clawID, "test-tenant-id", clawID, "elasticclaw", "connected", now()); err != nil {
		t.Fatal(err)
	}
	var prs []clawPR
	for _, n := range prNumbers {
		prID := fmt.Sprintf("%s-pr-%d", clawID, n)
		prURL := fmt.Sprintf("https://github.com/owner/repo/pull/%d", n)
		if _, err := db.Exec(`INSERT INTO claw_prs(id,claw_id,repo,pr_number,pr_url,created_at) VALUES(?,?,?,?,?,?)`, prID, clawID, "owner/repo", n, prURL, now()); err != nil {
			t.Fatal(err)
		}
		prs = append(prs, clawPR{id: prID, clawID: clawID, repo: "owner/repo", prNumber: n, prURL: prURL})
	}
	return s, db, stub, prs
}

func clawStatusAndPRState(t *testing.T, db *sql.DB, clawID, prID string) (clawStatus, prState string) {
	t.Helper()
	if err := db.QueryRow(`SELECT status FROM claws WHERE id=?`, clawID).Scan(&clawStatus); err != nil {
		t.Fatal(err)
	}
	if err := db.QueryRow(`SELECT state FROM claw_prs WHERE id=?`, prID).Scan(&prState); err != nil {
		t.Fatal(err)
	}
	return clawStatus, prState
}

func TestCheckPRMergedKeepsClawAliveUntilAllPRsResolved(t *testing.T) {
	s, db, stub, prs := multiPRFixture(t, "claw-multi-merge", 1, 2)
	stub.set(1, "merged")
	stub.set(2, "open")

	// PR 1 merges while PR 2 is still open: the claw must keep watching.
	if _, terminated := s.checkPRMerged(prs[0], "token"); terminated {
		t.Fatal("checkPRMerged terminated the claw with another PR still open")
	}
	clawStatus, stateA := clawStatusAndPRState(t, db, "claw-multi-merge", prs[0].id)
	if clawStatus != "connected" {
		t.Fatalf("claw status = %q, want connected", clawStatus)
	}
	if stateA != "merged" {
		t.Fatalf("PR 1 state = %q, want merged", stateA)
	}
	var stateB string
	if err := db.QueryRow(`SELECT state FROM claw_prs WHERE id=?`, prs[1].id).Scan(&stateB); err != nil {
		t.Fatal(err)
	}
	if stateB != "open" {
		t.Fatalf("PR 2 state = %q, want open", stateB)
	}
	var msg string
	if err := db.QueryRow(`SELECT content FROM messages WHERE claw_id=? AND role='hub' ORDER BY created_at DESC LIMIT 1`, "claw-multi-merge").Scan(&msg); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(msg, "Still watching 1 other open PR(s)") {
		t.Fatalf("unexpected hub message after partial merge: %q", msg)
	}

	// PR 2 merges: every tracked PR is resolved, so the claw terminates.
	stub.set(2, "merged")
	if _, terminated := s.checkPRMerged(prs[1], "token"); !terminated {
		t.Fatal("checkPRMerged returned false, want true once all PRs are merged")
	}
	var clawStatusAfter string
	if err := db.QueryRow(`SELECT status FROM claws WHERE id=?`, "claw-multi-merge").Scan(&clawStatusAfter); err != nil {
		t.Fatal(err)
	}
	if clawStatusAfter != "deleted" {
		t.Fatalf("claw status = %q, want deleted", clawStatusAfter)
	}
	var rows int
	if err := db.QueryRow(`SELECT COUNT(*) FROM claw_prs WHERE claw_id=?`, "claw-multi-merge").Scan(&rows); err != nil {
		t.Fatal(err)
	}
	if rows != 0 {
		t.Fatalf("claw_prs rows after termination = %d, want 0", rows)
	}
}

func TestCheckPRMergedClosedWithoutMergeKeepsClawWatching(t *testing.T) {
	s, db, stub, prs := multiPRFixture(t, "claw-multi-close", 1, 2)
	stub.set(1, "closed")
	stub.set(2, "open")

	// PR 1 closed without merge while PR 2 is open: no stop, row marked closed.
	if _, terminated := s.checkPRMerged(prs[0], "token"); terminated {
		t.Fatal("checkPRMerged terminated the claw on a closed PR with another still open")
	}
	clawStatus, stateA := clawStatusAndPRState(t, db, "claw-multi-close", prs[0].id)
	if clawStatus != "connected" {
		t.Fatalf("claw status = %q, want connected", clawStatus)
	}
	if stateA != "closed" {
		t.Fatalf("PR 1 state = %q, want closed", stateA)
	}
	var msg string
	if err := db.QueryRow(`SELECT content FROM messages WHERE claw_id=? AND role='hub' ORDER BY created_at DESC LIMIT 1`, "claw-multi-close").Scan(&msg); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(msg, "closed without being merged") || !strings.Contains(msg, "Still watching 1 other open PR(s)") {
		t.Fatalf("unexpected hub message after unmerged close: %q", msg)
	}

	// PR 2 merges: the closed row counts as resolved, so the claw terminates.
	stub.set(2, "merged")
	if _, terminated := s.checkPRMerged(prs[1], "token"); !terminated {
		t.Fatal("checkPRMerged returned false, want true once the last open PR merged")
	}
	var clawStatusAfter string
	if err := db.QueryRow(`SELECT status FROM claws WHERE id=?`, "claw-multi-close").Scan(&clawStatusAfter); err != nil {
		t.Fatal(err)
	}
	if clawStatusAfter != "deleted" {
		t.Fatalf("claw status = %q, want deleted", clawStatusAfter)
	}
}

// Regression: a claw with a single tracked PR keeps the original contract —
// its one merge terminates the claw in the same call.
func TestCheckPRMergedSinglePRStillTerminates(t *testing.T) {
	s, db, stub, prs := multiPRFixture(t, "claw-single", 1)
	stub.set(1, "merged")

	if _, terminated := s.checkPRMerged(prs[0], "token"); !terminated {
		t.Fatal("checkPRMerged returned false, want true for the only tracked PR merging")
	}
	var clawStatus string
	if err := db.QueryRow(`SELECT status FROM claws WHERE id=?`, "claw-single").Scan(&clawStatus); err != nil {
		t.Fatal(err)
	}
	if clawStatus != "deleted" {
		t.Fatalf("claw status = %q, want deleted", clawStatus)
	}
	var rows int
	if err := db.QueryRow(`SELECT COUNT(*) FROM claw_prs WHERE claw_id=?`, "claw-single").Scan(&rows); err != nil {
		t.Fatal(err)
	}
	if rows != 0 {
		t.Fatalf("claw_prs rows after termination = %d, want 0", rows)
	}
}

// Resolved rows persist in claw_prs but must be excluded from polling.
func TestPollAllPRsSkipsResolvedRows(t *testing.T) {
	s, db, _, prs := multiPRFixture(t, "claw-poll-skip", 1, 2)
	if _, err := db.Exec(`UPDATE claw_prs SET state='merged', merged=1 WHERE id=?`, prs[0].id); err != nil {
		t.Fatal(err)
	}

	// pollAllPRs records how many rows its query selected before any GitHub
	// call; only the still-open row may be visited.
	s.pollAllPRs()
	s.mu.Lock()
	tracked := s.trackedPRCount
	s.mu.Unlock()
	if tracked != 1 {
		t.Fatalf("pollAllPRs visited %d row(s), want 1 (resolved row must be skipped)", tracked)
	}
}

// loadClawPRsByNumber feeds the webhook CI path and must not act on a PR that
// is already resolved.
func TestLoadClawPRsByNumberSkipsResolvedRows(t *testing.T) {
	s, db, _, prs := multiPRFixture(t, "claw-load-skip", 1, 2)
	if _, err := db.Exec(`UPDATE claw_prs SET state='closed' WHERE id=?`, prs[0].id); err != nil {
		t.Fatal(err)
	}

	got := s.loadClawPRsByNumber("owner/repo", 1)
	if len(got) != 0 {
		t.Fatalf("loadClawPRsByNumber returned %d row(s) for a closed PR, want 0", len(got))
	}
	got = s.loadClawPRsByNumber("owner/repo", 2)
	if len(got) != 1 {
		t.Fatalf("loadClawPRsByNumber returned %d row(s) for an open PR, want 1", len(got))
	}
}

// FIX 1: a mention-only row (a PR URL the agent merely mentioned in a message)
// must never block finalization — when every delivered PR is resolved the claw
// terminates even though the mentioned PR is still open.
func TestMentionOnlyPRDoesNotBlockFinalization(t *testing.T) {
	s, db, stub, prs := multiPRFixture(t, "claw-mention-only", 1, 2)
	if _, err := db.Exec(`UPDATE claw_prs SET mention_only=1 WHERE id=?`, prs[1].id); err != nil {
		t.Fatal(err)
	}
	stub.set(1, "merged")
	stub.set(2, "open")

	if _, terminated := s.checkPRMerged(prs[0], "token"); !terminated {
		t.Fatal("checkPRMerged returned terminated=false, want true when the only delivered PR merged")
	}
	var clawStatus string
	if err := db.QueryRow(`SELECT status FROM claws WHERE id=?`, "claw-mention-only").Scan(&clawStatus); err != nil {
		t.Fatal(err)
	}
	if clawStatus != "deleted" {
		t.Fatalf("claw status = %q, want deleted (mention-only PR must not pin the claw)", clawStatus)
	}
}

// FIX 1: a PR the agent mentioned mid-work and then delivered via [DONE] must
// be upgraded to delivered (mention_only=0) so it gates finalization.
func TestDeliveryUpgradesMentionOnlyRow(t *testing.T) {
	s, db, _, _ := multiPRFixture(t, "claw-upgrade")
	const url = "https://github.com/owner/repo/pull/9"

	s.scanMessageForPRs("claw-upgrade", "this depends on "+url, true)
	var mentionOnly int
	if err := db.QueryRow(`SELECT mention_only FROM claw_prs WHERE claw_id=? AND pr_url=?`, "claw-upgrade", url).Scan(&mentionOnly); err != nil {
		t.Fatal(err)
	}
	if mentionOnly != 1 {
		t.Fatalf("mention_only after scan = %d, want 1", mentionOnly)
	}
	if n, err := s.clawOpenPRCount("claw-upgrade"); err != nil || n != 0 {
		t.Fatalf("clawOpenPRCount = %d, %v; want 0 (mention-only must not block)", n, err)
	}

	if failURL := s.registerDonePRURLs("claw-upgrade", []string{url}); failURL != "" {
		t.Fatalf("registerDonePRURLs failed on %q", failURL)
	}
	if err := db.QueryRow(`SELECT mention_only FROM claw_prs WHERE claw_id=? AND pr_url=?`, "claw-upgrade", url).Scan(&mentionOnly); err != nil {
		t.Fatal(err)
	}
	if mentionOnly != 0 {
		t.Fatalf("mention_only after [DONE] delivery = %d, want 0", mentionOnly)
	}
	if n, err := s.clawOpenPRCount("claw-upgrade"); err != nil || n != 1 {
		t.Fatalf("clawOpenPRCount = %d, %v; want 1 (delivered PR must block)", n, err)
	}
}

// FIX 2: a reopened PR resets its resolved row so the watcher polls it again
// and it gates finalization again.
func TestReopenedPRResetMakesRowPolledAgain(t *testing.T) {
	s, db, _, prs := multiPRFixture(t, "claw-reopen", 1, 2)
	if _, err := db.Exec(`UPDATE claw_prs SET state='closed', merged=1, merged_at='2026-01-02T03:04:05Z' WHERE id=?`, prs[0].id); err != nil {
		t.Fatal(err)
	}

	s.pollAllPRs()
	s.mu.Lock()
	tracked := s.trackedPRCount
	s.mu.Unlock()
	if tracked != 1 {
		t.Fatalf("pollAllPRs visited %d row(s) before reopen, want 1", tracked)
	}

	s.resetReopenedClawPR("claw-reopen", prs[0].prURL)

	var state string
	var merged int
	var mergedAt sql.NullString
	if err := db.QueryRow(`SELECT state, merged, merged_at FROM claw_prs WHERE id=?`, prs[0].id).Scan(&state, &merged, &mergedAt); err != nil {
		t.Fatal(err)
	}
	if state != "open" || merged != 0 || mergedAt.Valid {
		t.Fatalf("row after reopen = state=%q merged=%d merged_at=%v, want open/0/NULL", state, merged, mergedAt)
	}
	if n, err := s.clawOpenPRCount("claw-reopen"); err != nil || n != 2 {
		t.Fatalf("clawOpenPRCount = %d, %v; want 2 (reopened PR gates finalization again)", n, err)
	}

	s.pollAllPRs()
	s.mu.Lock()
	tracked = s.trackedPRCount
	s.mu.Unlock()
	if tracked != 2 {
		t.Fatalf("pollAllPRs visited %d row(s) after reopen, want 2 (reopened row must be polled again)", tracked)
	}
}

// FIX 5 safety rule: when the open-PR count query itself fails, checkPRMerged
// must never terminate the claw — the next poll retries.
func TestCheckPRMergedCountErrorDoesNotTerminate(t *testing.T) {
	s, db, stub, prs := multiPRFixture(t, "claw-count-err", 1)
	stub.set(1, "merged")

	// Recreate claw_prs without the mention_only column: every UPDATE the
	// merge path runs still works, but clawOpenPRCount (which filters on
	// mention_only) fails.
	if _, err := db.Exec(`CREATE TABLE claw_prs_slim AS SELECT id, claw_id, repo, pr_number, pr_url, title, state, merged, merged_at, permanent_failure_count, created_at FROM claw_prs`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`DROP TABLE claw_prs`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`ALTER TABLE claw_prs_slim RENAME TO claw_prs`); err != nil {
		t.Fatal(err)
	}
	if _, err := s.clawOpenPRCount("claw-count-err"); err == nil {
		t.Fatal("test setup broken: clawOpenPRCount did not fail")
	}

	if _, terminated := s.checkPRMerged(prs[0], "token"); terminated {
		t.Fatal("checkPRMerged terminated the claw on a failed open-PR count")
	}
	var clawStatus string
	if err := db.QueryRow(`SELECT status FROM claws WHERE id=?`, "claw-count-err").Scan(&clawStatus); err != nil {
		t.Fatal(err)
	}
	if clawStatus != "connected" {
		t.Fatalf("claw status = %q, want connected (unchanged on count error)", clawStatus)
	}
}

// FIX 3: a single-PR claw whose PR is closed without merge is stopped AND its
// claw_prs rows are deleted, so a retried claw under the same id does not come
// back carrying a resolved row that suppresses the agent-idle alert.
func TestSinglePRClosedWithoutMergeDeletesRowsAndStopsClaw(t *testing.T) {
	s, db, stub, prs := multiPRFixture(t, "claw-close-single", 1)
	stub.set(1, "closed")

	if _, terminated := s.checkPRMerged(prs[0], "token"); !terminated {
		t.Fatal("checkPRMerged returned terminated=false, want true for the only delivered PR closing")
	}
	var rows int
	if err := db.QueryRow(`SELECT COUNT(*) FROM claw_prs WHERE claw_id=?`, "claw-close-single").Scan(&rows); err != nil {
		t.Fatal(err)
	}
	if rows != 0 {
		t.Fatalf("claw_prs rows after closed-without-merge stop = %d, want 0", rows)
	}
	// stopAgentWithReason runs in a goroutine and may sleep several seconds
	// when the retry disposition is indeterminate under DB contention.
	deadline := time.Now().Add(15 * time.Second)
	for {
		var status string
		_ = db.QueryRow(`SELECT status FROM claws WHERE id=?`, "claw-close-single").Scan(&status)
		if status != "connected" {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("claw status = %q, want a stopped status", status)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// Per-PR analytics must fire on a NON-final merge: after PR A merges with B
// still open, A's task_run_prs row already records merged=1. A refactor that
// moves the analytics below the all-PRs-resolved gate fails here.
func TestPartialMergeRecordsPerPRAnalytics(t *testing.T) {
	s, db, stub, prs := multiPRFixture(t, "claw-partial-analytics", 1, 2)
	runID, _ := startTaskRunForTest(t, s, "claw-partial-analytics", "partial-analytics")
	associatePRForTest(t, s, runID, "owner/repo", 1, taskRunPRStateOpen)
	associatePRForTest(t, s, runID, "owner/repo", 2, taskRunPRStateOpen)
	stub.set(1, "merged")
	stub.set(2, "open")

	if _, terminated := s.checkPRMerged(prs[0], "token"); terminated {
		t.Fatal("checkPRMerged terminated the claw with PR 2 still open")
	}
	assertTaskRunPR(t, db, runID, "owner/repo", 1, taskRunPRStateClosed, true)
	assertTaskRunPR(t, db, runID, "owner/repo", 2, taskRunPRStateOpen, false)
}

// FIX 7: mergePRForClaw merges every open tracked PR exactly once, skips rows
// already resolved, and keeps the "no tracked PR" early return.
func TestMergePRForClawMergesOpenSkipsResolved(t *testing.T) {
	oldTransport := http.DefaultTransport
	var mints int64
	http.DefaultTransport = githubAppAnyTokenTransport{base: oldTransport, mints: &mints}
	t.Cleanup(func() { http.DefaultTransport = oldTransport })

	var mu sync.Mutex
	mergeCalls := map[int]int{}
	gh := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPut && strings.HasSuffix(r.URL.Path, "/merge") {
			parts := strings.Split(strings.Trim(r.URL.Path, "/"), "/")
			var n int
			fmt.Sscanf(parts[len(parts)-2], "%d", &n)
			mu.Lock()
			mergeCalls[n]++
			mu.Unlock()
			w.Header().Set("Content-Type", "application/json")
			fmt.Fprint(w, `{"merged":true}`)
			return
		}
		http.NotFound(w, r)
	}))
	t.Cleanup(gh.Close)

	cfg := &types.HubConfig{GitHubApps: []*types.GitHubAppConfig{{AppID: 1, PrivateKeyPEM: testGitHubAppPEM(t)}}}
	s, db := NewTestServerWithConfig(t, cfg, gh.URL, "", "")
	for _, clawID := range []string{"claw-merge-batch", "claw-merge-none"} {
		if _, err := db.Exec(`INSERT INTO claws(id,tenant_id,name,template,status,created_at) VALUES(?,?,?,?,?,?)`, clawID, "test-tenant-id", clawID, "elasticclaw", "connected", now()); err != nil {
			t.Fatal(err)
		}
	}
	for n := 1; n <= 4; n++ {
		if _, err := db.Exec(`INSERT INTO claw_prs(id,claw_id,repo,pr_number,pr_url,created_at) VALUES(?,?,?,?,?,?)`,
			fmt.Sprintf("merge-pr-%d", n), "claw-merge-batch", "owner/repo", n, fmt.Sprintf("https://github.com/owner/repo/pull/%d", n), now()); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := db.Exec(`UPDATE claw_prs SET state='merged', merged=1 WHERE id='merge-pr-3'`); err != nil {
		t.Fatal(err)
	}
	// FIX E: a mention-only row is a polling target, never an action target —
	// no merge PUT may be issued for a third-party PR the agent merely linked.
	if _, err := db.Exec(`UPDATE claw_prs SET mention_only=1 WHERE id='merge-pr-4'`); err != nil {
		t.Fatal(err)
	}

	s.mergePRForClaw("claw-merge-batch")
	mu.Lock()
	got := map[int]int{}
	for k, v := range mergeCalls {
		got[k] = v
	}
	mu.Unlock()
	if got[1] != 1 || got[2] != 1 {
		t.Fatalf("merge PUTs = %v, want exactly one for PR 1 and PR 2", got)
	}
	if got[3] != 0 {
		t.Fatalf("merge PUTs = %v, PR 3 is already merged and must be skipped", got)
	}
	if got[4] != 0 {
		t.Fatalf("merge PUTs = %v, PR 4 is mention-only and must never be merged", got)
	}

	// "No tracked PR" early return: nothing merged, nothing injected.
	s.mergePRForClaw("claw-merge-none")
	mu.Lock()
	total := 0
	for _, v := range mergeCalls {
		total += v
	}
	mu.Unlock()
	if total != 2 {
		t.Fatalf("merge PUTs after empty-claw call = %d, want 2 (early return must not merge)", total)
	}
	var msgs int
	if err := db.QueryRow(`SELECT COUNT(*) FROM messages WHERE claw_id='claw-merge-none'`).Scan(&msgs); err != nil {
		t.Fatal(err)
	}
	if msgs != 0 {
		t.Fatalf("messages injected for claw with no tracked PRs = %d, want 0", msgs)
	}
}

// tokenMintOutageTransport serves the GitHub App endpoints like
// githubAppAnyTokenTransport, but fails installation-token minting while fail
// is set — simulating a token outage. Successful mints carry a near-immediate
// expiry so the token is never cached and every poll exercises minting again.
type tokenMintOutageTransport struct {
	base http.RoundTripper
	fail *atomic.Bool
}

func (t tokenMintOutageTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	if r.URL.Host != "api.github.com" {
		return t.base.RoundTrip(r)
	}
	if r.Method == http.MethodGet && r.URL.Path == "/app/installations" {
		return githubAppTokenResponse(http.StatusOK, `[{"id":1,"account":{"login":"owner"}}]`), nil
	}
	if r.Method == http.MethodPost && r.URL.Path == "/app/installations/1/access_tokens" {
		if t.fail.Load() {
			return githubAppTokenResponse(http.StatusInternalServerError, `{"message":"mint outage"}`), nil
		}
		expiry := time.Now().Add(time.Minute).UTC().Format(time.RFC3339)
		return githubAppTokenResponse(http.StatusCreated, fmt.Sprintf(`{"token":"tok","expires_at":%q}`, expiry)), nil
	}
	return githubAppTokenResponse(http.StatusNotFound, `{"message":"not found"}`), nil
}

// FIX A: hitting the token-miss bound closes the row (so it stops blocking
// finalization) but must NEVER touch the claw — token resolution fails for
// transient causes too, and a mid-work agent with nothing delivered would be
// killed. The counter must also reset once the token resolves again, so only
// genuinely consecutive misses accumulate.
func TestTokenMissBoundClosesRowWithoutTouchingClaw(t *testing.T) {
	resetGitHubClientForTest(t)
	fail := &atomic.Bool{}
	fail.Store(true)
	oldTransport := http.DefaultTransport
	http.DefaultTransport = tokenMintOutageTransport{base: oldTransport, fail: fail}
	t.Cleanup(func() { http.DefaultTransport = oldTransport })

	stub := &multiPRGitHubStub{states: map[int]string{}}
	gh := httptest.NewServer(stub.handler())
	t.Cleanup(gh.Close)

	cfg := &types.HubConfig{GitHubApps: []*types.GitHubAppConfig{{AppID: 1, PrivateKeyPEM: testGitHubAppPEM(t)}}}
	s, db := NewTestServerWithConfig(t, cfg, gh.URL, "", "")
	if _, err := db.Exec(`INSERT INTO claws(id,tenant_id,name,template,status,created_at) VALUES(?,?,?,?,?,?)`, "claw-token-miss", "test-tenant-id", "claw-token-miss", "elasticclaw", "connected", now()); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO claw_prs(id,claw_id,repo,pr_number,pr_url,created_at) VALUES(?,?,?,?,?,?)`,
		"pr-token-miss", "claw-token-miss", "owner/repo", 1, "https://github.com/owner/repo/pull/1", now()); err != nil {
		t.Fatal(err)
	}

	readMisses := func() int {
		t.Helper()
		var n int
		if err := db.QueryRow(`SELECT token_miss_count FROM claw_prs WHERE id='pr-token-miss'`).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}

	// One miss short of the bound...
	for i := 0; i < prMergedPermanentFailureLimit-1; i++ {
		s.pollAllPRs()
	}
	if got := readMisses(); got != prMergedPermanentFailureLimit-1 {
		t.Fatalf("token_miss_count = %d, want %d", got, prMergedPermanentFailureLimit-1)
	}

	// ...then the token resolves: the counter resets and the row stays open.
	fail.Store(false)
	s.pollAllPRs()
	if got := readMisses(); got != 0 {
		t.Fatalf("token_miss_count after a successful token resolve = %d, want 0", got)
	}
	var state string
	if err := db.QueryRow(`SELECT state FROM claw_prs WHERE id='pr-token-miss'`).Scan(&state); err != nil {
		t.Fatal(err)
	}
	if state != "open" {
		t.Fatalf("row state after recovered poll = %q, want open", state)
	}

	// A full run of consecutive misses closes the row — and only the row.
	fail.Store(true)
	for i := 0; i < prMergedPermanentFailureLimit; i++ {
		s.pollAllPRs()
	}
	var clawStatus string
	if err := db.QueryRow(`SELECT state FROM claw_prs WHERE id='pr-token-miss'`).Scan(&state); err != nil {
		t.Fatal(err)
	}
	if err := db.QueryRow(`SELECT status FROM claws WHERE id='claw-token-miss'`).Scan(&clawStatus); err != nil {
		t.Fatal(err)
	}
	if state != "closed" {
		t.Fatalf("row state after token-miss bound = %q, want closed", state)
	}
	if clawStatus != "connected" {
		t.Fatalf("claw status after token-miss bound = %q, want connected (the bound must never touch the claw)", clawStatus)
	}
}

// FIX C: a mention-only PR that a human merges resolves its row silently — no
// teardown, no per-PR analytics, no tracker move.
func TestMentionOnlyPRMergeDoesNotTerminateClaw(t *testing.T) {
	s, db, stub, prs := multiPRFixture(t, "claw-mention-merge", 1)
	if _, err := db.Exec(`UPDATE claw_prs SET mention_only=1 WHERE id=?`, prs[0].id); err != nil {
		t.Fatal(err)
	}
	runID, _ := startTaskRunForTest(t, s, "claw-mention-merge", "mention-merge")
	associatePRForTest(t, s, runID, "owner/repo", 1, taskRunPRStateOpen)
	stub.set(1, "merged")

	resolved, terminated := s.checkPRMerged(prs[0], "token")
	if !resolved || terminated {
		t.Fatalf("checkPRMerged = (resolved=%v, terminated=%v), want (true, false) for a mention-only merge", resolved, terminated)
	}
	clawStatus, state := clawStatusAndPRState(t, db, "claw-mention-merge", prs[0].id)
	if clawStatus != "connected" {
		t.Fatalf("claw status = %q, want connected (a stranger merging a mentioned PR must not destroy the sandbox)", clawStatus)
	}
	if state != "merged" {
		t.Fatalf("PR state = %q, want merged (the resolution must persist)", state)
	}
	// Per-PR merge analytics must not fire for a merely-mentioned PR.
	assertTaskRunPR(t, db, runID, "owner/repo", 1, taskRunPRStateOpen, false)
}

// FIX C: the closed-without-merge variant of the same asymmetry — a stray
// mentioned PR being closed must not stop the claw or delete its rows.
func TestMentionOnlyPRCloseDoesNotStopClaw(t *testing.T) {
	s, db, stub, prs := multiPRFixture(t, "claw-mention-close", 1)
	if _, err := db.Exec(`UPDATE claw_prs SET mention_only=1 WHERE id=?`, prs[0].id); err != nil {
		t.Fatal(err)
	}
	stub.set(1, "closed")

	resolved, terminated := s.checkPRMerged(prs[0], "token")
	if !resolved || terminated {
		t.Fatalf("checkPRMerged = (resolved=%v, terminated=%v), want (true, false) for a mention-only close", resolved, terminated)
	}
	clawStatus, state := clawStatusAndPRState(t, db, "claw-mention-close", prs[0].id)
	if clawStatus != "connected" {
		t.Fatalf("claw status = %q, want connected", clawStatus)
	}
	if state != "closed" {
		t.Fatalf("PR state = %q, want closed", state)
	}
	var rows int
	if err := db.QueryRow(`SELECT COUNT(*) FROM claw_prs WHERE claw_id='claw-mention-close'`).Scan(&rows); err != nil {
		t.Fatal(err)
	}
	if rows != 1 {
		t.Fatalf("claw_prs rows = %d, want 1 (mention-only close must not run the stop path that deletes rows)", rows)
	}
}

// FIX C: an unreachable (404) mention-only PR closes its row at the failure
// bound but never escalates to stopping the claw.
func TestUnreachableMentionOnlyPRNeverStopsClaw(t *testing.T) {
	s, db, stub, prs := multiPRFixture(t, "claw-mention-unreachable", 1)
	if _, err := db.Exec(`UPDATE claw_prs SET mention_only=1 WHERE id=?`, prs[0].id); err != nil {
		t.Fatal(err)
	}
	stub.set(1, "missing")

	for i := 0; i < prMergedPermanentFailureLimit; i++ {
		if _, terminated := s.checkPRMerged(prs[0], "token"); terminated {
			t.Fatal("checkPRMerged terminated a claw off an unreachable mention-only PR")
		}
	}
	clawStatus, state := clawStatusAndPRState(t, db, "claw-mention-unreachable", prs[0].id)
	if clawStatus != "connected" {
		t.Fatalf("claw status = %q, want connected", clawStatus)
	}
	if state != "closed" {
		t.Fatalf("PR state = %q, want closed (unreachable row must stop blocking finalization)", state)
	}
}

// A permanently failing (404) PR is closed at the bound, but the claw survives
// because another delivered PR is still open.
func TestPermanentFailureClosesRowButClawSurvivesWithOtherOpenPR(t *testing.T) {
	s, db, stub, prs := multiPRFixture(t, "claw-perm-partial", 1, 2)
	stub.set(1, "missing")
	stub.set(2, "open")

	for i := 0; i < prMergedPermanentFailureLimit; i++ {
		if _, terminated := s.checkPRMerged(prs[0], "token"); terminated {
			t.Fatal("checkPRMerged terminated the claw while another delivered PR is open")
		}
	}
	clawStatus, state := clawStatusAndPRState(t, db, "claw-perm-partial", prs[0].id)
	if clawStatus != "connected" {
		t.Fatalf("claw status = %q, want connected", clawStatus)
	}
	if state != "closed" {
		t.Fatalf("unreachable PR state = %q, want closed", state)
	}
	if n, err := s.clawOpenPRCount("claw-perm-partial"); err != nil || n != 1 {
		t.Fatalf("clawOpenPRCount = %d, %v; want 1 (the healthy PR keeps blocking finalization)", n, err)
	}
}

// FIX B: the reopened-PR reset must not depend on the factory trigger kind.
// A claw created by an issue-triggered factory never enters the pull_request
// factory loop, so the reset has to run before it.
func TestProcessGitHubPREventReopenedResetsRowForNonPRFactory(t *testing.T) {
	s, db, _, prs := multiPRFixture(t, "claw-reopen-issue-factory", 1)
	if _, err := db.Exec(`UPDATE claw_prs SET state='closed' WHERE id=?`, prs[0].id); err != nil {
		t.Fatal(err)
	}
	s.mu.Lock()
	s.hubCfg.Factories = []*types.FactoryConfig{{
		Name:        "issues-factory",
		Integration: "github",
		Template:    "elasticclaw",
		Trigger:     &types.GitHubTrigger{On: "issue"},
	}}
	s.mu.Unlock()

	var payload githubPRPayload
	payload.Action = "reopened"
	payload.Number = 1
	payload.Repository.FullName = "owner/repo"
	payload.PullRequest.HTMLURL = prs[0].prURL
	s.processGitHubPREvent(payload)

	var state string
	if err := db.QueryRow(`SELECT state FROM claw_prs WHERE id=?`, prs[0].id).Scan(&state); err != nil {
		t.Fatal(err)
	}
	if state != "open" {
		t.Fatalf("row state after reopened webhook = %q, want open (reset must run outside the pull_request factory loop)", state)
	}
	if n, err := s.clawOpenPRCount("claw-reopen-issue-factory"); err != nil || n != 1 {
		t.Fatalf("clawOpenPRCount = %d, %v; want 1 (reopened PR gates finalization again)", n, err)
	}
}

// FIX D: a PR registered from pipeline gate stdout (a delivery channel) blocks
// finalization, unlike a PR mentioned in agent turn text.
func TestPipelineGateRegisteredPRBlocksFinalization(t *testing.T) {
	s, db, _, _ := multiPRFixture(t, "claw-gate-registered")
	const url = "https://github.com/owner/repo/pull/31"

	s.scanMessageForPRs("claw-gate-registered", `{"prs":["`+url+`"]}`, false)
	var mentionOnly int
	if err := db.QueryRow(`SELECT mention_only FROM claw_prs WHERE claw_id=? AND pr_url=?`, "claw-gate-registered", url).Scan(&mentionOnly); err != nil {
		t.Fatal(err)
	}
	if mentionOnly != 0 {
		t.Fatalf("mention_only for a gate-registered PR = %d, want 0 (gate stdout is a delivery channel)", mentionOnly)
	}
	if n, err := s.clawOpenPRCount("claw-gate-registered"); err != nil || n != 1 {
		t.Fatalf("clawOpenPRCount = %d, %v; want 1 (gate-registered PR must block finalization)", n, err)
	}
}

// A merge or close must be visible as turn progress: resolved rows survive in
// claw_prs, so only the state/merged columns can register the change.
func TestTurnProgressFingerprintChangesWhenPRMerges(t *testing.T) {
	s, db, _, prs := multiPRFixture(t, "claw-fingerprint", 1)
	before, err := s.turnProgressFingerprint("claw-fingerprint", "same response")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`UPDATE claw_prs SET state='merged', merged=1 WHERE id=?`, prs[0].id); err != nil {
		t.Fatal(err)
	}
	after, err := s.turnProgressFingerprint("claw-fingerprint", "same response")
	if err != nil {
		t.Fatal(err)
	}
	if before == after {
		t.Fatal("turnProgressFingerprint unchanged after a tracked PR merged — the merge is invisible as progress")
	}
}

// A resolved row must not read as "PR out, awaiting humans" to the agent-idle
// alert, or a claw carrying one would have its stuck alert suppressed for life.
func TestAgentIdleHasClawPRsIgnoresResolvedRows(t *testing.T) {
	s, db, _, prs := multiPRFixture(t, "claw-idle-prs", 1)
	if !s.agentIdleHasClawPRs("claw-idle-prs") {
		t.Fatal("agentIdleHasClawPRs = false with an open tracked PR, want true")
	}
	if _, err := db.Exec(`UPDATE claw_prs SET state='merged', merged=1 WHERE id=?`, prs[0].id); err != nil {
		t.Fatal(err)
	}
	if s.agentIdleHasClawPRs("claw-idle-prs") {
		t.Fatal("agentIdleHasClawPRs = true for a state='merged' row, want false")
	}
}
