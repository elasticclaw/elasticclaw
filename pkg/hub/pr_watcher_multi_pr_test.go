package hub

import (
	"database/sql"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/elasticclaw/elasticclaw/pkg/types"
)

// multiPRGitHubStub serves repos/{repo}/pulls/{n} responses whose per-PR state
// the test can flip mid-test, so one server can drive a claw through
// "first PR resolved, second still open, second resolves later" sequences.
type multiPRGitHubStub struct {
	mu     sync.Mutex
	states map[int]string // pr number -> "open" | "merged" | "closed"
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
func multiPRFixture(t *testing.T, clawID string, prNumbers ...int) (*Server, *sql.DB, *multiPRGitHubStub, []clawPR) {
	t.Helper()
	stub := &multiPRGitHubStub{states: map[int]string{}}
	gh := httptest.NewServer(stub.handler())
	t.Cleanup(gh.Close)

	s, db := NewTestServerWithConfig(t, &types.HubConfig{}, gh.URL, "", "")
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
	if terminated := s.checkPRMerged(prs[0], "token"); terminated {
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
	if terminated := s.checkPRMerged(prs[1], "token"); !terminated {
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
	if terminated := s.checkPRMerged(prs[0], "token"); terminated {
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
	if terminated := s.checkPRMerged(prs[1], "token"); !terminated {
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

	if terminated := s.checkPRMerged(prs[0], "token"); !terminated {
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
