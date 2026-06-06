package hub

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/elasticclaw/elasticclaw/pkg/types"
)

// newDoneTestServer creates a minimal Server with an in-memory DB and an
// optional GitHub API base URL override (for mocking PR validation).
func newDoneTestServer(t *testing.T, githubBase string) *Server {
	t.Helper()
	db, err := openDB(":memory:")
	if err != nil {
		t.Fatalf("openDB: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	s := &Server{
		db:            db,
		hubCfg:        &types.HubConfig{},
		claws:         map[string]*clawConn{},
		users:         map[string]*userConn{},
		githubBaseURL: githubBase,
	}
	return s
}

// mockGitHubPRServer spins up a test HTTP server that responds to GitHub PR
// API calls. prStates maps "owner/repo/pulls/N" → state string ("open",
// "closed", etc.). Missing entries return 404.
func mockGitHubPRServer(t *testing.T, prStates map[string]string) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// strip leading "/repos/" → "owner/repo/pulls/N"
		const prefix = "/repos/"
		if len(r.URL.Path) <= len(prefix) {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		key := r.URL.Path[len(prefix):]
		state, ok := prStates[key]
		if !ok {
			w.WriteHeader(http.StatusNotFound)
			json.NewEncoder(w).Encode(map[string]string{"message": "Not Found"})
			return
		}
		json.NewEncoder(w).Encode(map[string]interface{}{"state": state, "number": 1})
	}))
	t.Cleanup(srv.Close)
	return srv
}

// --- extractDonePRURLs ---

func TestExtractDonePRURLs(t *testing.T) {
	tests := []struct {
		name     string
		message  string
		wantURLs []string
	}{
		{
			name:     "single PR URL",
			message:  "[DONE] https://github.com/org/repo/pull/123",
			wantURLs: []string{"https://github.com/org/repo/pull/123"},
		},
		{
			name:    "multiple PR URLs same line",
			message: "[DONE] https://github.com/org/repo/pull/1 https://github.com/org/other/pull/2",
			wantURLs: []string{
				"https://github.com/org/repo/pull/1",
				"https://github.com/org/other/pull/2",
			},
		},
		{
			name:     "bare DONE no URL",
			message:  "[DONE]",
			wantURLs: nil,
		},
		{
			name:     "DONE with surrounding text on same line",
			message:  "All done! [DONE] https://github.com/org/repo/pull/42",
			wantURLs: []string{"https://github.com/org/repo/pull/42"},
		},
		{
			name:     "DONE on its own line, URL on next line — not picked up",
			message:  "[DONE]\nhttps://github.com/org/repo/pull/99",
			wantURLs: nil,
		},
		{
			name:     "no DONE token at all",
			message:  "I finished the work https://github.com/org/repo/pull/7",
			wantURLs: nil,
		},
		{
			name:     "DONE buried in multiline message",
			message:  "Here is a summary.\n\n[DONE] https://github.com/org/repo/pull/55\n\nThanks!",
			wantURLs: []string{"https://github.com/org/repo/pull/55"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := extractDonePRURLs(tt.message)
			if len(got) != len(tt.wantURLs) {
				t.Fatalf("got %v, want %v", got, tt.wantURLs)
			}
			for i := range got {
				if got[i] != tt.wantURLs[i] {
					t.Errorf("url[%d]: got %q, want %q", i, got[i], tt.wantURLs[i])
				}
			}
		})
	}
}

// --- validateDonePRs ---

func TestValidateDonePRs_NoURLs(t *testing.T) {
	srv := mockGitHubPRServer(t, nil)
	s := newDoneTestServer(t, srv.URL)
	rejected, reason := s.validateDonePRs("claw1", nil, "tok")
	if !rejected {
		t.Fatal("expected rejection when no URLs provided")
	}
	if reason == "" {
		t.Fatal("expected non-empty reason")
	}
}

func TestValidateDonePRs_SingleOpen(t *testing.T) {
	srv := mockGitHubPRServer(t, map[string]string{
		"org/repo/pulls/42": "open",
	})
	s := newDoneTestServer(t, srv.URL)
	urls := []string{"https://github.com/org/repo/pull/42"}
	rejected, reason := s.validateDonePRs("claw1", urls, "tok")
	if rejected {
		t.Fatalf("expected acceptance, got rejection: %s", reason)
	}
}

func TestValidateDonePRs_MultipleAllOpen(t *testing.T) {
	srv := mockGitHubPRServer(t, map[string]string{
		"org/repo/pulls/1":  "open",
		"org/other/pulls/2": "open",
	})
	s := newDoneTestServer(t, srv.URL)
	urls := []string{
		"https://github.com/org/repo/pull/1",
		"https://github.com/org/other/pull/2",
	}
	rejected, reason := s.validateDonePRs("claw1", urls, "tok")
	if rejected {
		t.Fatalf("expected acceptance, got rejection: %s", reason)
	}
}

func TestValidateDonePRs_OneClosed(t *testing.T) {
	srv := mockGitHubPRServer(t, map[string]string{
		"org/repo/pulls/1": "open",
		"org/repo/pulls/2": "closed",
	})
	s := newDoneTestServer(t, srv.URL)
	urls := []string{
		"https://github.com/org/repo/pull/1",
		"https://github.com/org/repo/pull/2",
	}
	rejected, reason := s.validateDonePRs("claw1", urls, "tok")
	if !rejected {
		t.Fatal("expected rejection when one PR is closed")
	}
	if reason == "" {
		t.Fatal("expected non-empty reason")
	}
}

func TestValidateDonePRs_NotFound(t *testing.T) {
	srv := mockGitHubPRServer(t, map[string]string{}) // nothing registered → 404
	s := newDoneTestServer(t, srv.URL)
	urls := []string{"https://github.com/org/repo/pull/99"}
	rejected, reason := s.validateDonePRs("claw1", urls, "tok")
	if !rejected {
		t.Fatal("expected rejection when PR returns 404")
	}
	if reason == "" {
		t.Fatal("expected non-empty reason")
	}
}

func TestValidateDonePRs_MergedIsNotOpen(t *testing.T) {
	srv := mockGitHubPRServer(t, map[string]string{
		"org/repo/pulls/5": "merged",
	})
	s := newDoneTestServer(t, srv.URL)
	urls := []string{"https://github.com/org/repo/pull/5"}
	rejected, reason := s.validateDonePRs("claw1", urls, "tok")
	if !rejected {
		t.Fatal("expected rejection when PR state is 'merged' not 'open'")
	}
	if reason == "" {
		t.Fatal("expected non-empty reason")
	}
}

// --- handleClawDoneSignal gate blocking ---

func TestHandleClawDoneSignal_BlockedByFailedRequiredGate(t *testing.T) {
	s, db := NewTestServerWithConfig(t, &types.HubConfig{Token: "test-token"}, "", "", "")

	const clawID = "claw-done-gate-block"
	// Insert a claw with a linear issue (tenant "test-tenant-id" is already created by NewTestServerWithConfig)
	_, err := db.Exec(
		`INSERT INTO claws(id, tenant_id, name, template, status, linear_issue_id, created_at) VALUES(?,?,?,?,?,?,datetime('now'))`,
		clawID, "test-tenant-id", "test-claw", "base", "connected", "ENG-123",
	)
	if err != nil {
		t.Fatalf("insert claw: %v", err)
	}

	// Insert a failed required gate
	_, err = db.Exec(`
		INSERT INTO pipeline_gate_results(claw_id, stage_id, output_name, verdict, matched_path, matched_value, required, created_at)
		VALUES(?, ?, ?, ?, ?, ?, ?, datetime('now'))`,
		clawID, "android_validation", "android_validation", "fail", "status", "failed", 1)
	if err != nil {
		t.Fatalf("insert gate result: %v", err)
	}

	// Call handleClawDoneSignal — should be blocked
	s.handleClawDoneSignal(clawID, "[DONE] https://github.com/org/repo/pull/42")

	// Verify no PR was stored
	var count int
	if err := db.QueryRow(`SELECT COUNT(*) FROM claw_prs WHERE claw_id=?`, clawID).Scan(&count); err != nil {
		t.Fatalf("count claw_prs: %v", err)
	}
	if count != 0 {
		t.Fatalf("expected 0 PRs stored, got %d", count)
	}

	// Verify a message was injected to the claw (injectUserMessage uses role='user')
	var msgCount int
	if err := db.QueryRow(`SELECT COUNT(*) FROM messages WHERE claw_id=? AND role='user'`, clawID).Scan(&msgCount); err != nil {
		t.Fatalf("count messages: %v", err)
	}
	if msgCount != 1 {
		t.Fatalf("expected 1 user message, got %d", msgCount)
	}
}

func TestHandleClawDoneSignal_AllowedWhenNoFailedRequiredGate(t *testing.T) {
	s, db := NewTestServerWithConfig(t, &types.HubConfig{Token: "test-token"}, "", "", "")

	const clawID = "claw-done-gate-allow"
	// Insert a claw with a linear issue (tenant "test-tenant-id" is already created by NewTestServerWithConfig)
	_, err := db.Exec(
		`INSERT INTO claws(id, tenant_id, name, template, status, linear_issue_id, created_at) VALUES(?,?,?,?,?,?,datetime('now'))`,
		clawID, "test-tenant-id", "test-claw", "base", "connected", "ENG-124",
	)
	if err != nil {
		t.Fatalf("insert claw: %v", err)
	}

	// Insert a passed required gate
	_, err = db.Exec(`
		INSERT INTO pipeline_gate_results(claw_id, stage_id, output_name, verdict, matched_path, matched_value, required, created_at)
		VALUES(?, ?, ?, ?, ?, ?, ?, datetime('now'))`,
		clawID, "android_validation", "android_validation", "pass", "status", "passed", 1)
	if err != nil {
		t.Fatalf("insert gate result: %v", err)
	}

	// Call handleClawDoneSignal — should proceed (no GH App, so no PR validation)
	s.handleClawDoneSignal(clawID, "[DONE]")

	// Verify no PR was stored (no GH App configured, but no gate blocking either)
	var count int
	if err := db.QueryRow(`SELECT COUNT(*) FROM claw_prs WHERE claw_id=?`, clawID).Scan(&count); err != nil {
		t.Fatalf("count claw_prs: %v", err)
	}
	if count != 0 {
		t.Fatalf("expected 0 PRs stored (no GH App), got %d", count)
	}
}
