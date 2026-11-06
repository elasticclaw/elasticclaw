package hub

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/elasticclaw/elasticclaw/pkg/types"
)

type prDiscoveryTransport struct {
	failPath        string
	requireDeadline bool
	unscopedToken   bool

	mu    sync.Mutex
	calls int
	fail  bool
	prs   []map[string]interface{}
}

func (m *prDiscoveryTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.calls++
	if m.requireDeadline {
		deadline, ok := r.Context().Deadline()
		if !ok || time.Until(deadline) > 60*time.Second {
			return nil, errors.New("missing total deadline")
		}
	}
	if r.URL.Path == m.failPath {
		return nil, errors.New("metadata failure")
	}
	if m.fail {
		return nil, errors.New("temporary failure")
	}
	if r.URL.Path == "/installation/repositories" {
		return githubAppTokenResponse(http.StatusOK, `{"total_count":2,"repositories":[{"name":"repo","full_name":"owner/repo"},{"name":"other","full_name":"owner/other"}]}`), nil
	}
	if m.unscopedToken && r.Method == http.MethodPost && r.URL.Path == "/app/installations/1/access_tokens" {
		body, _ := io.ReadAll(r.Body)
		if bytes.Contains(body, []byte("*")) {
			// GitHub rejects repository patterns in token scopes.
			return githubAppTokenResponse(http.StatusUnprocessableEntity, `{"message":"invalid repositories"}`), nil
		}
		token := "installation-token"
		if bytes.Contains(body, []byte(`"repositories"`)) {
			token = "repo-token"
		}
		return githubAppTokenResponse(http.StatusCreated, `{"token":"`+token+`","expires_at":"2030-01-01T00:00:00Z"}`), nil
	}
	if strings.HasPrefix(r.URL.Path, "/app/") {
		return (githubAppTokenTransport{}).RoundTrip(r)
	}
	var body interface{} = []interface{}{}
	if r.URL.Path == "/repos/owner/repo/pulls" {
		body = m.prs
	} else {
		for _, pr := range m.prs {
			if r.URL.Path == fmt.Sprintf("/repos/owner/repo/pulls/%v", pr["number"]) {
				body = pr
				break
			}
		}
	}
	data, err := json.Marshal(body)
	if err != nil {
		return nil, err
	}
	return githubAppTokenResponse(http.StatusOK, string(data)), nil
}

func discoveryServer(t *testing.T) (*Server, *prDiscoveryTransport) {
	t.Helper()
	t.Setenv("ELASTICCLAW_HUB_CONFIG", filepath.Join(t.TempDir(), "hub.yaml"))
	resetGitHubClientForTest(t)
	mock := &prDiscoveryTransport{}
	old := http.DefaultTransport
	http.DefaultTransport = mock
	t.Cleanup(func() { http.DefaultTransport = old })
	s, _ := NewTestServerWithConfig(t, &types.HubConfig{
		GitHubApps: []*types.GitHubAppConfig{{AppID: 1, URL: "https://github.com/apps/fw-agent-factory", PrivateKeyPEM: testGitHubAppPEM(t)}},
	}, "", "", "")
	return s, mock
}

func discoveryRun(t *testing.T, s *Server, claw string, started time.Time) string {
	t.Helper()
	_, err := s.db.Exec(`INSERT INTO claws(id,tenant_id,name,template,status,created_at,pipeline_stage,github_repos)
		VALUES(?,?,?,'elasticclaw','connected',?,'open_pr','[{"repo":"owner/repo","permissions":"write"}]')`,
		claw, "test-tenant-id", claw, started)
	if err != nil {
		t.Fatal(err)
	}
	run, _, err := s.ensureTaskRunForClaw(claw, TaskRunStart{
		TenantID: "test-tenant-id", RunKind: taskRunKindPRTask, OwnerType: taskRunOwnerFactory,
		IssueID: "NEXT-1754", AnalyticsEnabled: true, RequiresPR: true, StartedAt: started,
		EventKey: claw + ":start",
	})
	if err != nil {
		t.Fatal(err)
	}
	return run
}

func discoveryPR(number int, created, merged time.Time) map[string]interface{} {
	pr := map[string]interface{}{
		"user":   map[string]interface{}{"login": "fw-agent-factory[bot]"},
		"number": number, "title": "Translations (next-1754)", "state": "open",
		"created_at": created.UTC().Format(time.RFC3339),
		"head":       map[string]interface{}{"ref": "NEXT-1754-meal-guide", "sha": ""},
	}
	if !merged.IsZero() {
		pr["state"], pr["merged"] = "closed", true
		pr["merged_at"], pr["closed_at"] = merged.UTC().Format(time.RFC3339), merged.UTC().Format(time.RFC3339)
	}
	return pr
}

func TestPRDiscoveryIssueToken(t *testing.T) {
	for _, tc := range []struct {
		text string
		want bool
	}{
		{"Fix (next-1754)", true}, {"feature/NEXT-1754-meal-guide", true},
		{"NEXT-17540", false}, {"NEXT-175", false}, {"ANEXT-1754", false},
		{"NEXT-1754a", false}, {"NEXT-1754", true},
	} {
		if got := issueTokenMatches(tc.text, "NEXT-1754"); got != tc.want {
			t.Errorf("%q: got %v, want %v", tc.text, got, tc.want)
		}
	}
	if issueTokenMatches("anything", "") || issueTokenMatches("NEXT-1754", "NEXT-175") {
		t.Fatal("empty or partial issue matched")
	}
}

func TestPRDiscoveryLiveRegistersAndTerminatesOnMerge(t *testing.T) {
	for _, alreadyMerged := range []bool{false, true} {
		t.Run(fmt.Sprint(alreadyMerged), func(t *testing.T) {
			s, mock := discoveryServer(t)
			started := time.Now().Add(-time.Hour).Truncate(time.Second)
			run := discoveryRun(t, s, "claw-discovery-live", started)
			merged := started.Add(20 * time.Minute)
			mock.prs = []map[string]interface{}{discoveryPR(1281, started.Add(time.Minute), time.Time{})}
			if alreadyMerged {
				mock.prs[0] = discoveryPR(1281, started.Add(time.Minute), merged)
			}
			backdateAgentIdleResumeBaseline(t, s)
			s.discoverClawPRs("claw-discovery-live", run)
			if got := idleResumeMessages(t, s.db, "claw-discovery-live"); got != 0 {
				t.Fatalf("discovery injected %d unnecessary resumes", got)
			}
			var pr clawPR
			var mention int
			err := s.db.QueryRow("SELECT id,claw_id,repo,pr_number,pr_url,mention_only FROM claw_prs WHERE claw_id=?", "claw-discovery-live").
				Scan(&pr.id, &pr.clawID, &pr.repo, &pr.prNumber, &pr.prURL, &mention)
			if err != nil || mention != 0 {
				t.Fatalf("delivered PR: mention=%d err=%v", mention, err)
			}
			// A second idle pass is idempotent.
			s.discoverClawPRs("claw-discovery-live", run)
			mock.prs[0] = discoveryPR(1281, started.Add(time.Minute), merged)
			resolved, terminated := s.checkPRMerged(pr, "repo-token")
			if !resolved || !terminated {
				t.Fatalf("merge: resolved=%v terminated=%v", resolved, terminated)
			}
			var status, failure, clawStatus string
			if err := s.db.QueryRow("SELECT status,failure_type FROM task_run_summaries WHERE run_id=?", run).Scan(&status, &failure); err != nil {
				t.Fatal(err)
			}
			_ = s.db.QueryRow("SELECT status FROM claws WHERE id=?", pr.clawID).Scan(&clawStatus)
			if status != taskRunStatusClean || failure != "" || clawStatus != "deleted" {
				t.Fatalf("status=%s failure=%s claw=%s", status, failure, clawStatus)
			}
		})
	}
}

func TestPRDiscoveryTerminalBackfill(t *testing.T) {
	s, mock := discoveryServer(t)
	started := time.Now().Add(-time.Hour).Truncate(time.Second)
	run := discoveryRun(t, s, "claw-discovery-terminal", started)
	stop := started.Add(40 * time.Minute)
	recordTaskRunEventForTest(t, s, TaskRunEvent{RunID: run, EventKey: "stop", EventType: taskRunEventManualStopBeforeDelivery,
		FailureType: taskRunFailureManualStopDelivery, WarningType: taskRunWarningHumanManualStopResume,
		ActorType: taskRunActorHuman, Source: taskRunSourceDashboard, OccurredAt: stop})
	var status, failure string
	_ = s.db.QueryRow("SELECT status,failure_type FROM task_run_summaries WHERE run_id=?", run).Scan(&status, &failure)
	if status != taskRunStatusFailed || failure != taskRunFailureManualStopDelivery {
		t.Fatalf("before backfill: %s %s", status, failure)
	}
	other := discoveryRun(t, s, "claw-discovery-other", stop.Add(time.Second))
	associatePRForTest(t, s, other, "owner/repo", 4, taskRunPRStateOpen)
	mock.prs = []map[string]interface{}{
		discoveryPR(1, started.Add(time.Minute), started.Add(20*time.Minute)),
		discoveryPR(2, started.Add(-time.Second), started.Add(20*time.Minute)),
		discoveryPR(3, stop.Add(time.Second), stop.Add(time.Minute)),
		discoveryPR(4, started.Add(time.Minute), started.Add(20*time.Minute)),
	}
	s.reconcileUnreportedPRs("")
	s.reconcileUnreportedPRs("")
	var count, merged, openedAt, mergedAt int64
	if err := s.db.QueryRow("SELECT status,failure_type,pr_count,merged_pr_count,pr_opened_at,merged_at FROM task_run_summaries WHERE run_id=?", run).
		Scan(&status, &failure, &count, &merged, &openedAt, &mergedAt); err != nil {
		t.Fatal(err)
	}
	if status != taskRunStatusClean || failure != "" || count != 1 || merged != 1 ||
		openedAt != started.Add(time.Minute).UnixMilli() || mergedAt != started.Add(20*time.Minute).UnixMilli() {
		t.Fatalf("backfill: %s %s count=%d merged=%d opened=%d mergedAt=%d", status, failure, count, merged, openedAt, mergedAt)
	}
	p := TaskRunPR{RunID: run, Repo: "OWNER/REPO", PRNumber: 4, State: "open", Exclusive: true}
	if err := s.associateTaskRunPR(p); err == nil {
		t.Fatal("exclusive association accepted a PR owned by another run")
	}
}

func TestPRDiscoveryIdleReminder(t *testing.T) {
	s, _ := discoveryServer(t)
	run := discoveryRun(t, s, "claw-discovery-reminder", time.Now().Add(-time.Hour))
	idle := 12 * time.Minute
	if got := s.agentIdleResumeMessageForClaw("missing-claw", idle); got != agentIdleResumeMessage(idle) {
		t.Fatal("missing-run message changed")
	}
	if !strings.Contains(s.agentIdleResumeMessageForClaw("claw-discovery-reminder", idle), "[DONE]") {
		t.Fatal("missing reminder")
	}
	if _, err := s.db.Exec("UPDATE task_runs SET requires_pr=0 WHERE id=?", run); err != nil {
		t.Fatal(err)
	}
	if got := s.agentIdleResumeMessageForClaw("claw-discovery-reminder", idle); got != agentIdleResumeMessage(idle) {
		t.Fatal("non-PR run message changed")
	}
	_, _ = s.db.Exec("UPDATE task_runs SET requires_pr=1 WHERE id=?", run)
	_, _ = s.db.Exec(`INSERT INTO claw_prs(id,claw_id,repo,pr_number,pr_url,created_at) VALUES('reminder-pr','claw-discovery-reminder','owner/repo',1,'https://github.com/owner/repo/pull/1',?)`, now())
	if got := s.agentIdleResumeMessageForClaw("claw-discovery-reminder", idle); got != agentIdleResumeMessage(idle) {
		t.Fatal("tracked-PR message changed")
	}
}

func TestPRDiscoveryRespectsQuota(t *testing.T) {
	s, mock := discoveryServer(t)
	run := discoveryRun(t, s, "claw-discovery-quota", time.Now().Add(-time.Hour))
	mock.prs = []map[string]interface{}{discoveryPR(1, time.Now().Add(-time.Minute), time.Time{})}
	defaultGitHubClient.mu.Lock()
	prevHaveLimit, prevRemaining, prevReset := defaultGitHubClient.haveLimit, defaultGitHubClient.remaining, defaultGitHubClient.reset
	defaultGitHubClient.haveLimit = true
	defaultGitHubClient.remaining = 0
	defaultGitHubClient.reset = time.Now().Add(time.Hour)
	defaultGitHubClient.mu.Unlock()
	t.Cleanup(func() {
		defaultGitHubClient.mu.Lock()
		defaultGitHubClient.haveLimit, defaultGitHubClient.remaining, defaultGitHubClient.reset = prevHaveLimit, prevRemaining, prevReset
		defaultGitHubClient.mu.Unlock()
	})
	s.discoverClawPRs("claw-discovery-quota", run)
	var count int
	_ = s.db.QueryRow("SELECT COUNT(*) FROM claw_prs").Scan(&count)
	if count != 0 {
		t.Fatal("discovery ran below quota reserve")
	}
}

func TestPRDiscoveryTerminalOpenAndClosedOutcomes(t *testing.T) {
	for _, closed := range []bool{false, true} {
		t.Run(fmt.Sprint(closed), func(t *testing.T) {
			s, mock := discoveryServer(t)
			started := time.Now().Add(-time.Hour).Truncate(time.Second)
			run := discoveryRun(t, s, "claw-discovery-outcome", started)
			recordTaskRunEventForTest(t, s, TaskRunEvent{RunID: run, EventKey: "stop",
				EventType: taskRunEventManualStopBeforeDelivery, FailureType: taskRunFailureManualStopDelivery,
				ActorType: taskRunActorSystem, OccurredAt: started.Add(40 * time.Minute)})
			// Soft-deleted claws retain their repository access for reconciliation.
			_, _ = s.db.Exec("UPDATE claws SET status='deleted' WHERE id='claw-discovery-outcome'")
			pr := discoveryPR(1, started.Add(time.Minute), time.Time{})
			wantState, wantPhase := taskRunPRStateOpen, taskRunPhaseWaitingForMerge
			if closed {
				pr["state"], pr["closed_at"] = "closed", started.Add(20*time.Minute).UTC().Format(time.RFC3339)
				wantState, wantPhase = taskRunPRStateClosed, taskRunPhaseTerminal
			}
			// Match only the head branch, not the title.
			pr["title"] = "Meal guide translations"
			mock.prs = []map[string]interface{}{pr}
			s.reconcileUnreportedPRs("")
			var state, phase string
			var closedAt int64
			if err := s.db.QueryRow("SELECT state,closed_at FROM task_run_prs WHERE run_id=?", run).Scan(&state, &closedAt); err != nil {
				t.Fatal(err)
			}
			_ = s.db.QueryRow("SELECT phase FROM task_run_summaries WHERE run_id=?", run).Scan(&phase)
			if state != wantState || phase != wantPhase || (closed && closedAt != started.Add(20*time.Minute).UnixMilli()) {
				t.Fatalf("state=%s phase=%s closedAt=%d", state, phase, closedAt)
			}
		})
	}
}

func TestPRDiscoveryManualStopBeforeMergeStillCountsAsIntervention(t *testing.T) {
	s, _ := discoveryServer(t)
	started := time.Now().Add(-time.Hour).Truncate(time.Second)
	run := discoveryRun(t, s, "claw-discovery-intervention", started)
	recordTaskRunEventForTest(t, s, TaskRunEvent{RunID: run, EventKey: "stop",
		EventType: taskRunEventManualStopBeforeDelivery, FailureType: taskRunFailureManualStopDelivery,
		WarningType: taskRunWarningHumanManualStopResume, ActorType: taskRunActorHuman,
		Source: taskRunSourceDashboard, OccurredAt: started.Add(10 * time.Minute)})
	if err := s.associateTaskRunPR(TaskRunPR{RunID: run, Repo: "owner/repo", PRNumber: 1, State: "closed",
		Merged: true, OpenedAt: started.Add(time.Minute), OccurredAt: started.Add(20 * time.Minute)}); err != nil {
		t.Fatal(err)
	}
	var status, failure string
	_ = s.db.QueryRow("SELECT status,failure_type FROM task_run_summaries WHERE run_id=?", run).Scan(&status, &failure)
	if status != taskRunStatusWarning || failure != "" {
		t.Fatalf("status=%s failure=%s", status, failure)
	}
}

func TestPRDiscoveryRejectsHumanAndAbandonedLivePRs(t *testing.T) {
	s, mock := discoveryServer(t)
	started := time.Now().Add(-time.Hour).Truncate(time.Second)
	run := discoveryRun(t, s, "claw-filter", started)
	human := discoveryPR(1, started.Add(time.Minute), time.Time{})
	human["user"] = map[string]interface{}{"login": "human"}
	unknown := discoveryPR(2, started.Add(time.Minute), time.Time{})
	delete(unknown, "user")
	abandoned := discoveryPR(3, started.Add(time.Minute), time.Time{})
	abandoned["state"], abandoned["closed_at"] = "closed", started.Add(2*time.Minute).Format(time.RFC3339)
	mock.prs = []map[string]interface{}{human, unknown, abandoned}
	s.discoverClawPRs("claw-filter", run)
	var count int
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM claw_prs`).Scan(&count); err != nil || count != 0 {
		t.Fatalf("count=%d err=%v", count, err)
	}
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM task_run_prs`).Scan(&count); err != nil || count != 0 {
		t.Fatalf("associations=%d err=%v", count, err)
	}
}

func TestPRDiscoveryTerminalScannedOnceAndFailuresRetry(t *testing.T) {
	s, mock := discoveryServer(t)
	started := time.Now().Add(-time.Hour)
	run := discoveryRun(t, s, "claw-once", started)
	recordTaskRunEventForTest(t, s, TaskRunEvent{RunID: run, EventKey: "stop", EventType: taskRunEventManualStopBeforeDelivery, OccurredAt: started.Add(30 * time.Minute)})
	mock.fail = true
	s.reconcileUnreportedPRs("")
	var scanned int
	_ = s.db.QueryRow(`SELECT pr_discovery_scanned FROM task_runs WHERE id=?`, run).Scan(&scanned)
	if scanned != 0 {
		t.Fatal("failed scan marked complete")
	}
	mock.fail = false
	s.reconcileUnreportedPRs("")
	_ = s.db.QueryRow(`SELECT pr_discovery_scanned FROM task_runs WHERE id=?`, run).Scan(&scanned)
	if scanned != 1 {
		t.Fatal("successful empty scan not marked")
	}
	calls := mock.calls
	s.reconcileUnreportedPRs("")
	if calls != mock.calls {
		t.Fatal("scanned run relisted")
	}
}

func TestPRDiscoveryAmbiguousOverlappingRuns(t *testing.T) {
	s, mock := discoveryServer(t)
	started := time.Now().Add(-time.Hour)
	first := discoveryRun(t, s, "claw-overlap-first", started)
	discoveryRun(t, s, "claw-overlap-second", started.Add(time.Minute))
	mock.prs = []map[string]interface{}{discoveryPR(1, started.Add(2*time.Minute), time.Time{})}
	r, err := s.prDiscoveryRun(first)
	if err != nil {
		t.Fatal(err)
	}
	prs, err := s.discoverRunPRs(r)
	if err != nil || len(prs) != 0 {
		t.Fatalf("prs=%v err=%v", prs, err)
	}
}

func TestPRDiscoveryExplicitAssociationWins(t *testing.T) {
	s, _ := discoveryServer(t)
	started := time.Now().Add(-time.Hour)
	inferred := discoveryRun(t, s, "claw-inferred", started)
	explicit := discoveryRun(t, s, "claw-explicit", started.Add(30*time.Minute))
	if err := s.associateTaskRunPR(TaskRunPR{RunID: inferred, Repo: "owner/repo", PRNumber: 1, Exclusive: true}); err != nil {
		t.Fatal(err)
	}
	if failed := s.insertClawPRsAtomic("claw-inferred", []prMentionCandidate{{repo: "owner/repo", number: 1, url: "https://github.com/owner/repo/pull/1", inferred: true}}, false, "done"); failed != "" {
		t.Fatal(failed)
	}
	if err := s.associateTaskRunPR(TaskRunPR{RunID: explicit, Repo: "OWNER/REPO", PRNumber: 1}); err != nil {
		t.Fatal(err)
	}
	var count int
	_ = s.db.QueryRow(`SELECT COUNT(*) FROM task_run_prs WHERE pr_number=1`).Scan(&count)
	if count != 1 {
		t.Fatalf("PR counted %d times", count)
	}
	_ = s.db.QueryRow(`SELECT pr_count FROM task_run_summaries WHERE run_id=?`, inferred).Scan(&count)
	if count != 0 {
		t.Fatal("old summary still counts inferred PR")
	}
	_ = s.db.QueryRow(`SELECT COUNT(*) FROM claw_prs WHERE claw_id='claw-inferred'`).Scan(&count)
	if count != 0 {
		t.Fatal("old watcher row retained")
	}
	if err := s.associateTaskRunPR(TaskRunPR{RunID: inferred, Repo: "owner/repo", PRNumber: 1, PreserveInference: true, State: taskRunPRStateClosed, Merged: true}); err != nil {
		t.Fatal(err)
	}
	recordTaskRunEventForTest(t, s, TaskRunEvent{RunID: inferred, EventKey: "late-merge", EventType: taskRunEventPRMerged, Detail: map[string]any{"repo": "owner/repo", "prNumber": 1}})
	_ = s.db.QueryRow(`SELECT COUNT(*) FROM task_run_prs WHERE pr_number=1`).Scan(&count)
	if count != 1 {
		t.Fatal("late provider event resurrected displaced association")
	}
}

func waitPRDiscovery(t *testing.T, s *Server, id string) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		s.prDiscoveryMu.Lock()
		running := s.prDiscoveryInFlight[id]
		s.prDiscoveryMu.Unlock()
		if !running {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatal("discovery did not finish")
}

func TestPRDiscoveryRetriesIndependentlyOfIdleResume(t *testing.T) {
	s, mock := discoveryServer(t)
	at := time.Now()
	discoveryRun(t, s, "claw-retry", at.Add(-time.Hour))
	cc := idleTestConn("claw-retry", 30*time.Minute)
	cc.noProgressPaused = true
	s.claws["claw-retry"] = cc
	_, err := s.db.Exec(`UPDATE claws SET idle_resume_count=99,idle_resume_at=? WHERE id='claw-retry'`, at.UnixMilli())
	if err != nil {
		t.Fatal(err)
	}
	mock.fail = true
	s.scheduleIdlePRDiscovery(at)
	waitPRDiscovery(t, s, "claw-retry")
	if mock.calls == 0 {
		t.Fatal("no discovery attempt")
	}
	calls := mock.calls
	s.scheduleIdlePRDiscovery(at.Add(time.Minute))
	if mock.calls != calls {
		t.Fatal("retried too soon")
	}
	mock.fail = false
	mock.prs = []map[string]interface{}{discoveryPR(1, at.Add(-time.Minute), time.Time{})}
	s.scheduleIdlePRDiscovery(at.Add(10 * time.Minute))
	waitPRDiscovery(t, s, "claw-retry")
	var count int
	_ = s.db.QueryRow(`SELECT COUNT(*) FROM claw_prs WHERE claw_id='claw-retry'`).Scan(&count)
	if count != 1 {
		t.Fatal("transient failure was not retried")
	}
}

func TestPRDiscoveryIdleResumeDoesNotCallGitHub(t *testing.T) {
	s, mock := discoveryServer(t)
	discoveryRun(t, s, "claw-db-only", time.Now().Add(-time.Hour))
	backdateAgentIdleResumeBaseline(t, s)
	s.checkAgentIdleResume(time.Now(), "claw-db-only", idleTestConn("claw-db-only", 30*time.Minute))
	if mock.calls != 0 {
		t.Fatalf("idle resume made %d GitHub calls", mock.calls)
	}
}

func TestPRDiscoveryMultiPRRegistrationAtomic(t *testing.T) {
	s, mock := discoveryServer(t)
	started := time.Now().Add(-time.Hour)
	run := discoveryRun(t, s, "claw-atomic", started)
	mock.prs = []map[string]interface{}{discoveryPR(1, started.Add(time.Minute), time.Time{}), discoveryPR(2, started.Add(2*time.Minute), time.Time{})}
	// Reject the second insert: the first must never become visible either.
	_, err := s.db.Exec(`CREATE TRIGGER reject_second_discovery_pr BEFORE INSERT ON claw_prs WHEN NEW.pr_number=2 BEGIN SELECT RAISE(ABORT,'test failure'); END`)
	if err != nil {
		t.Fatal(err)
	}
	s.discoverClawPRs("claw-atomic", run)
	var count int
	_ = s.db.QueryRow(`SELECT COUNT(*) FROM claw_prs`).Scan(&count)
	if count != 0 {
		t.Fatalf("partial watcher set: %d", count)
	}
	_, err = s.db.Exec(`DROP TRIGGER reject_second_discovery_pr`)
	if err != nil {
		t.Fatal(err)
	}
	s.discoverClawPRs("claw-atomic", run)
	_ = s.db.QueryRow(`SELECT COUNT(*) FROM claw_prs`).Scan(&count)
	if count != 2 {
		t.Fatalf("wanted full watcher set, got %d", count)
	}
}

func TestPRDiscoveryMissingRepoTokenContinues(t *testing.T) {
	s, mock := discoveryServer(t)
	started := time.Now().Add(-time.Hour)
	run := discoveryRun(t, s, "claw-token-skip", started)
	_, err := s.db.Exec(`UPDATE claws SET github_repos='[{"repo":"missing/repo"},{"repo":"owner/repo"}]' WHERE id='claw-token-skip'`)
	if err != nil {
		t.Fatal(err)
	}
	s.ghTokenCache = map[string]cachedGitHubToken{githubTokenCacheKey([]RepoAccess{{Repo: "missing/repo", Permissions: "read"}}): {expiresAt: time.Now().Add(time.Hour)}}
	mock.prs = []map[string]interface{}{discoveryPR(1, started.Add(time.Minute), time.Time{})}
	r, err := s.prDiscoveryRun(run)
	if err != nil {
		t.Fatal(err)
	}
	prs, complete, err := s.discoverRunPRsWithCompletion(r)
	if err != nil || len(prs) != 1 || complete {
		t.Fatalf("prs=%d complete=%v err=%v", len(prs), complete, err)
	}
	r.repos = `[{"repo":"missing/repo"}]`
	if _, err := s.discoverRunPRs(r); err == nil {
		t.Fatal("expected error when no repository can be queried")
	}
}

func TestPRDiscoveryPageLimitIsIncomplete(t *testing.T) {
	s, mock := discoveryServer(t)
	started := time.Now().Add(-time.Hour)
	run := discoveryRun(t, s, "claw-pages", started)
	for i := 1; i <= 100; i++ {
		pr := discoveryPR(i, started.Add(time.Minute), time.Time{})
		if i != 1 {
			pr["title"] = "Other ticket"
			pr["head"] = map[string]interface{}{"ref": "other-ticket"}
		}
		mock.prs = append(mock.prs, pr)
	}
	r, err := s.prDiscoveryRun(run)
	if err != nil {
		t.Fatal(err)
	}
	r.finished = started.Add(30 * time.Minute).UnixMilli()
	prs, complete, err := s.discoverRunPRsWithCompletion(r)
	if err != nil || len(prs) == 0 || complete {
		t.Fatalf("page limit: candidates=%d complete=%v err=%v", len(prs), complete, err)
	}
	if prs, err := s.discoverRunPRs(r); err == nil || len(prs) != 0 {
		t.Fatalf("incomplete discovery published %d PRs: %v", len(prs), err)
	}
}

func TestPRDiscoveryFetchesAllMetadataBeforeRegistration(t *testing.T) {
	s, mock := discoveryServer(t)
	started := time.Now().Add(-time.Hour)
	run := discoveryRun(t, s, "claw-metadata", started)
	mock.prs = []map[string]interface{}{discoveryPR(1, started.Add(time.Minute), time.Time{}), discoveryPR(2, started.Add(2*time.Minute), time.Time{})}
	mock.requireDeadline = true
	mock.failPath = "/repos/owner/repo/pulls/2/reviews"
	s.discoverClawPRs("claw-metadata", run)
	var count int
	_ = s.db.QueryRow(`SELECT COUNT(*) FROM claw_prs`).Scan(&count)
	if count != 0 {
		t.Fatal("watcher armed before all metadata was fetched")
	}
	mock.failPath = ""
	s.discoverClawPRs("claw-metadata", run)
	_ = s.db.QueryRow(`SELECT COUNT(*) FROM claw_prs`).Scan(&count)
	if count != 2 {
		t.Fatal("metadata requests missing total deadline or registration failed")
	}
}

func TestPRDiscoveryWorkspaceBotIdentity(t *testing.T) {
	s, _ := discoveryServer(t)
	if !s.isDiscoveryAppBot("legacy-workspace", "fw-agent-factory[bot]") {
		t.Fatal("workspace without app settings did not inherit hub app")
	}
	err := writeManagedYAML(workspaceGitHubAppsPath("own-workspace"), workspaceGitHubAppsFile{GitHubApps: map[string]workspaceGitHubApp{"own": {AppID: 2, URL: "https://github.com/apps/own-app"}}})
	if err != nil {
		t.Fatal(err)
	}
	if !s.isDiscoveryAppBot("own-workspace", "own-app[bot]") || s.isDiscoveryAppBot("own-workspace", "fw-agent-factory[bot]") {
		t.Fatal("bot identity escaped workspace scope")
	}
}

func TestPRDiscoveryTerminalPartialTokenFailureRetries(t *testing.T) {
	for _, outcome := range []string{"open", "merged", "closed"} {
		t.Run(outcome, func(t *testing.T) {
			s, mock := discoveryServer(t)
			started := time.Now().Add(-time.Hour).Truncate(time.Second)
			run := discoveryRun(t, s, "claw-partial", started)
			stop := started.Add(30 * time.Minute)
			recordTaskRunEventForTest(t, s, TaskRunEvent{RunID: run, EventKey: "stop", EventType: taskRunEventManualStopBeforeDelivery, OccurredAt: stop})
			if _, err := s.db.Exec(`UPDATE claws SET github_repos='[{"repo":"owner/repo"},{"repo":"missing/repo"}]' WHERE id='claw-partial'`); err != nil {
				t.Fatal(err)
			}
			key := githubTokenCacheKey([]RepoAccess{{Repo: "missing/repo", Permissions: "read"}})
			s.ghTokenCache = map[string]cachedGitHubToken{key: {expiresAt: time.Now().Add(time.Hour)}}
			pr := discoveryPR(1, started.Add(time.Minute), time.Time{})
			if outcome == "merged" {
				pr = discoveryPR(1, started.Add(time.Minute), started.Add(20*time.Minute))
			} else if outcome == "closed" {
				pr["state"], pr["closed_at"] = "closed", started.Add(20*time.Minute).Format(time.RFC3339)
			}
			mock.prs = []map[string]interface{}{pr}
			s.reconcileUnreportedPRs("")
			var scanned, count int
			var phase string
			var finished int64
			if err := s.db.QueryRow(`SELECT tr.pr_discovery_scanned,ts.phase,ts.finished_at,(SELECT COUNT(*) FROM task_run_prs p WHERE p.run_id=tr.id)
				FROM task_runs tr JOIN task_run_summaries ts ON ts.run_id=tr.id WHERE tr.id=?`, run).Scan(&scanned, &phase, &finished, &count); err != nil {
				t.Fatal(err)
			}
			if scanned != 0 || count != 0 || phase != taskRunPhaseTerminal || finished != stop.UnixMilli() {
				t.Fatalf("incomplete discovery mutated run: scanned=%d count=%d phase=%s finished=%d", scanned, count, phase, finished)
			}
			delete(s.ghTokenCache, key)
			calls := mock.calls
			s.reconcileUnreportedPRs("")
			if mock.calls <= calls {
				t.Fatal("incomplete discovery was not retried")
			}
			if err := s.db.QueryRow(`SELECT pr_discovery_scanned,(SELECT COUNT(*) FROM task_run_prs WHERE run_id=?) FROM task_runs WHERE id=?`, run, run).Scan(&scanned, &count); err != nil {
				t.Fatal(err)
			}
			if scanned != 1 || count != 1 {
				t.Fatalf("retry: scanned=%d count=%d", scanned, count)
			}
		})
	}
}

func TestTaskRunPRDiscoveryMigration(t *testing.T) {
	s, _ := discoveryServer(t)
	for _, statement := range []string{`ALTER TABLE task_runs DROP COLUMN pr_discovery_scanned`, `ALTER TABLE task_run_prs DROP COLUMN inferred`} {
		if _, err := s.db.Exec(statement); err != nil {
			t.Fatal(err)
		}
	}
	if err := migrate(s.db); err != nil {
		t.Fatal(err)
	}
	for _, statement := range []string{`SELECT pr_discovery_scanned FROM task_runs LIMIT 0`, `SELECT inferred FROM task_run_prs LIMIT 0`} {
		rows, err := s.db.Query(statement)
		if err != nil {
			t.Fatal(err)
		}
		rows.Close()
	}
}

func TestPRDiscoveryExplicitDeliveryPathsReplaceInference(t *testing.T) {
	for _, path := range []string{"message-scanner", "done-batch"} {
		t.Run(path, func(t *testing.T) {
			s, _ := discoveryServer(t)
			started := time.Now().Add(-time.Hour)
			inferred := discoveryRun(t, s, "claw-inferred-delivery", started)
			explicit := discoveryRun(t, s, "claw-explicit-delivery", started.Add(30*time.Minute))
			url := "https://github.com/owner/repo/pull/1"
			if err := s.associateTaskRunPR(TaskRunPR{RunID: inferred, Repo: "owner/repo", PRNumber: 1, URL: url, Exclusive: true}); err != nil {
				t.Fatal(err)
			}
			if failed := s.insertClawPRsAtomic("claw-inferred-delivery", []prMentionCandidate{{repo: "owner/repo", number: 1, url: url, inferred: true}}, false, "done"); failed != "" {
				t.Fatal(failed)
			}
			switch path {
			case "message-scanner":
				s.scanMessageForPRs("claw-explicit-delivery", url, false, "gate")
			case "done-batch":
				if failed := s.insertClawPRsAtomic("claw-explicit-delivery", []prMentionCandidate{{repo: "owner/repo", number: 1, url: url}}, false, "done"); failed != "" {
					t.Fatal(failed)
				}
			}
			var run string
			var count, inferredFlag int
			if err := s.db.QueryRow(`SELECT COUNT(*),run_id,inferred FROM task_run_prs WHERE pr_number=1`).Scan(&count, &run, &inferredFlag); err != nil {
				t.Fatal(err)
			}
			if count != 1 || run != explicit || inferredFlag != 0 {
				t.Fatalf("count=%d run=%s inferred=%d", count, run, inferredFlag)
			}
		})
	}
}

func TestPRDiscoverySharedAcrossRunsProviderMerge(t *testing.T) {
	for _, path := range []string{"watcher-refresh", "merge-event", "close-event"} {
		t.Run(path, func(t *testing.T) {
			s, _ := discoveryServer(t)
			started := time.Now().Add(-2 * time.Hour)
			first := discoveryRun(t, s, "shared-first", started)
			second := discoveryRun(t, s, "shared-second", started.Add(time.Hour))
			for _, run := range []string{first, second} {
				if err := s.associateTaskRunPR(TaskRunPR{RunID: run, Repo: "owner/repo", PRNumber: 7, State: taskRunPRStateOpen}); err != nil {
					t.Fatal(err)
				}
			}
			wantMerged, wantStatus := 1, taskRunStatusClean
			switch path {
			case "watcher-refresh":
				if err := s.associateTaskRunPR(TaskRunPR{RunID: second, Repo: "owner/repo", PRNumber: 7,
					PreserveInference: true, State: taskRunPRStateClosed, Merged: true}); err != nil {
					t.Fatal(err)
				}
			case "merge-event", "close-event":
				eventType := taskRunEventPRMerged
				if path == "close-event" {
					eventType = taskRunEventPRClosedUnmerged
					wantMerged = 0
				}
				recordTaskRunEventForTest(t, s, TaskRunEvent{RunID: second, EventKey: path, EventType: eventType,
					Detail: map[string]any{"repo": "owner/repo", "prNumber": 7}})
			}
			var merged int
			var state, phase, status string
			if err := s.db.QueryRow(`SELECT state,merged FROM task_run_prs WHERE run_id=?`, second).Scan(&state, &merged); err != nil {
				t.Fatal(err)
			}
			if err := s.db.QueryRow(`SELECT phase,status FROM task_run_summaries WHERE run_id=?`, second).Scan(&phase, &status); err != nil {
				t.Fatal(err)
			}
			if state != taskRunPRStateClosed || merged != wantMerged || phase != taskRunPhaseTerminal || status != wantStatus {
				t.Fatalf("state=%s merged=%d phase=%s status=%s", state, merged, phase, status)
			}
		})
	}
}

func TestPRDiscoveryTerminalNothingToDiscoverScanned(t *testing.T) {
	for _, setup := range []string{
		`UPDATE task_runs SET issue_id='' WHERE id=?`,
		`UPDATE claws SET github_repos='[]' WHERE task_run_id=?`,
		`UPDATE claws SET github_repos='' WHERE task_run_id=?`,
	} {
		t.Run(setup, func(t *testing.T) {
			s, mock := discoveryServer(t)
			started := time.Now().Add(-time.Hour)
			run := discoveryRun(t, s, "nothing-to-discover", started)
			recordTaskRunEventForTest(t, s, TaskRunEvent{RunID: run, EventKey: "stop", EventType: taskRunEventManualStopBeforeDelivery,
				OccurredAt: started.Add(30 * time.Minute)})
			if _, err := s.db.Exec(setup, run); err != nil {
				t.Fatal(err)
			}
			s.reconcileUnreportedPRs("")
			var scanned int
			if err := s.db.QueryRow(`SELECT pr_discovery_scanned FROM task_runs WHERE id=?`, run).Scan(&scanned); err != nil {
				t.Fatal(err)
			}
			if scanned != 1 || mock.calls != 0 {
				t.Fatalf("scanned=%d github calls=%d", scanned, mock.calls)
			}
		})
	}
}

func TestPRDiscoveryExplicitDonePromotesExistingAssociation(t *testing.T) {
	for _, path := range []string{"existing-row", "insert-race"} {
		t.Run(path, func(t *testing.T) {
			s, mock := discoveryServer(t)
			started := time.Now().Add(-time.Hour)
			run := discoveryRun(t, s, "late-done", started)
			s.hubCfg.Factories = []*types.FactoryConfig{{Name: "discovery"}}
			if _, err := s.db.Exec(`UPDATE claws SET tags='["factory:discovery"]' WHERE id='late-done'`); err != nil {
				t.Fatal(err)
			}
			mock.prs = []map[string]interface{}{discoveryPR(1, started.Add(time.Minute), time.Time{})}
			s.discoverClawPRs("late-done", run)
			var inferred, opened int
			if err := s.db.QueryRow(`SELECT inferred FROM task_run_prs WHERE run_id=?`, run).Scan(&inferred); err != nil || inferred != 1 {
				t.Fatalf("initial inference=%d err=%v", inferred, err)
			}
			row := prMentionCandidate{repo: "owner/repo", number: 1, url: "https://github.com/owner/repo/pull/1"}
			// A mention alone is not explicit delivery.
			if failed := s.insertClawPRsAtomic("late-done", []prMentionCandidate{row}, true, "done"); failed != "" {
				t.Fatal(failed)
			}
			if err := s.db.QueryRow(`SELECT inferred FROM task_run_prs WHERE run_id=?`, run).Scan(&inferred); err != nil || inferred != 1 {
				t.Fatalf("mention inference=%d err=%v", inferred, err)
			}
			if path == "insert-race" {
				if _, err := s.db.Exec(`DELETE FROM claw_prs WHERE claw_id='late-done'`); err != nil {
					t.Fatal(err)
				}
				// Simulate another writer publishing the row after the existence check.
				if _, err := s.db.Exec(`CREATE TRIGGER concurrent_discovery_pr BEFORE INSERT ON claw_prs
			WHEN NOT EXISTS(SELECT 1 FROM claw_prs WHERE claw_id=NEW.claw_id AND pr_url=NEW.pr_url)
			BEGIN
				INSERT INTO claw_prs(id,claw_id,repo,pr_number,pr_url,mention_only,created_at)
				VALUES('concurrent-pr',NEW.claw_id,NEW.repo,NEW.pr_number,NEW.pr_url,1,NEW.created_at);
				SELECT RAISE(IGNORE);
			END`); err != nil {
					t.Fatal(err)
				}
			}
			for i := 0; i < 2; i++ {
				if failed := s.insertClawPRsAtomic("late-done", []prMentionCandidate{row}, false, "done"); failed != "" {
					t.Fatal(failed)
				}
				if err := s.db.QueryRow(`SELECT inferred FROM task_run_prs WHERE run_id=?`, run).Scan(&inferred); err != nil || inferred != 0 {
					t.Fatalf("delivered inference=%d err=%v", inferred, err)
				}
				if err := s.db.QueryRow(`SELECT COUNT(*) FROM factory_analytics WHERE claw_id='late-done' AND action='pr_opened' AND detail='owner/repo#1'`).Scan(&opened); err != nil || opened != 1 {
					t.Fatalf("opened events=%d err=%v", opened, err)
				}
			}
		})
	}
}

func TestPRDiscoveryIdlePrunesStaleAttempts(t *testing.T) {
	s, _ := discoveryServer(t)
	at := time.Now()
	s.prDiscoveryAttempts = map[string]time.Time{
		"removed-claw": at.Add(-24 * time.Hour),
		"retry-due":    at.Add(-prDiscoveryRetryInterval),
		"recent":       at.Add(-time.Minute),
	}
	s.scheduleIdlePRDiscovery(at)
	if len(s.prDiscoveryAttempts) != 1 || s.prDiscoveryAttempts["recent"].IsZero() {
		t.Fatalf("remaining attempts: %v", s.prDiscoveryAttempts)
	}
}

func TestPRDiscoveryTokenLockCancellationAndBackgroundWait(t *testing.T) {
	s, mock := discoveryServer(t)
	repos := []RepoAccess{{Repo: "owner/repo", Permissions: "read"}}
	s.ghTokenCache = map[string]cachedGitHubToken{githubTokenCacheKey(repos): {token: "cached-token", expiresAt: time.Now().Add(time.Hour)}}
	s.ghTokenMu.Lock()
	locked := true
	defer func() {
		if locked {
			s.ghTokenMu.Unlock()
		}
	}()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	cancelled := make(chan string, 1)
	go func() { cancelled <- s.resolveGitHubTokenWithReposContext(ctx, repos) }()
	select {
	case token := <-cancelled:
		if token != "" || ctx.Err() == nil {
			t.Fatalf("cancelled token=%q err=%v", token, ctx.Err())
		}
	case <-time.After(time.Second):
		t.Fatal("discovery did not cancel while waiting for token mutex")
	}
	background := make(chan string, 1)
	go func() { background <- s.resolveGitHubTokenWithRepos(repos) }()
	select {
	case <-background:
		t.Fatal("background resolver bypassed token mutex")
	case <-time.After(20 * time.Millisecond):
	}
	s.ghTokenMu.Unlock()
	locked = false
	select {
	case token := <-background:
		if token != "cached-token" {
			t.Fatalf("background token=%q", token)
		}
	case <-time.After(time.Second):
		t.Fatal("background resolver did not resume")
	}
	if mock.calls != 0 {
		t.Fatalf("cache hit made %d network calls", mock.calls)
	}
}

func TestPRDiscoveryDoneSignalConfirmsDiscoveredPR(t *testing.T) {
	for _, urls := range [][]string{
		{"https://github.com/owner/repo/pull/1"},
		{"https://github.com/owner/repo/pull/1", "https://github.com/owner/repo/pull/2"},
	} {
		t.Run(fmt.Sprintf("%d-urls", len(urls)), func(t *testing.T) {
			s, mock := discoveryServer(t)
			started := time.Now().Add(-time.Hour)
			run := discoveryRun(t, s, "done-after-discovery", started)
			s.hubCfg.Factories = []*types.FactoryConfig{{Name: "discovery"}}
			if _, err := s.db.Exec(`UPDATE claws SET tags='["factory:discovery"]' WHERE id='done-after-discovery'`); err != nil {
				t.Fatal(err)
			}
			mock.prs = []map[string]interface{}{
				discoveryPR(1, started.Add(time.Minute), time.Time{}),
				discoveryPR(2, started.Add(2*time.Minute), time.Time{}),
			}
			s.discoverClawPRs("done-after-discovery", run)
			if failed := s.registerDonePRURLs("done-after-discovery", urls); failed != "" {
				t.Fatalf("registerDonePRURLs failed on %s", failed)
			}
			var inferred, opened int
			if err := s.db.QueryRow(`SELECT inferred FROM task_run_prs WHERE run_id=? AND pr_number=1`, run).Scan(&inferred); err != nil || inferred != 0 {
				t.Fatalf("PR #1 inferred=%d err=%v, want explicit after [DONE]", inferred, err)
			}
			if err := s.db.QueryRow(`SELECT COUNT(*) FROM factory_analytics WHERE claw_id='done-after-discovery' AND action='pr_opened' AND detail='owner/repo#1'`).Scan(&opened); err != nil || opened != 1 {
				t.Fatalf("pr_opened events=%d err=%v, want 1", opened, err)
			}
		})
	}
}

func TestPRDiscoveryLivePartialResultRetriesOnSchedule(t *testing.T) {
	s, mock := discoveryServer(t)
	at := time.Now().Truncate(time.Second)
	discoveryRun(t, s, "claw-partial-live", at.Add(-time.Hour))
	s.claws["claw-partial-live"] = idleTestConn("claw-partial-live", 30*time.Minute)
	if _, err := s.db.Exec(`UPDATE claws SET github_repos='[{"repo":"owner/repo"},{"repo":"missing/repo"}]' WHERE id='claw-partial-live'`); err != nil {
		t.Fatal(err)
	}
	key := githubTokenCacheKey([]RepoAccess{{Repo: "missing/repo", Permissions: "read"}})
	s.ghTokenCache = map[string]cachedGitHubToken{key: {expiresAt: at.Add(time.Hour)}}
	mock.prs = []map[string]interface{}{discoveryPR(1, at.Add(-time.Minute), time.Time{})}
	s.scheduleIdlePRDiscovery(at)
	waitPRDiscovery(t, s, "claw-partial-live")
	var count int
	if err := s.db.QueryRow(`SELECT (SELECT COUNT(*) FROM claw_prs)+(SELECT COUNT(*) FROM task_run_prs)`).Scan(&count); err != nil || count != 0 {
		t.Fatalf("partial result persisted: count=%d err=%v", count, err)
	}
	calls := mock.calls
	s.scheduleIdlePRDiscovery(at.Add(time.Minute))
	waitPRDiscovery(t, s, "claw-partial-live")
	if mock.calls != calls {
		t.Fatal("incomplete discovery retried too soon")
	}
	delete(s.ghTokenCache, key)
	s.scheduleIdlePRDiscovery(at.Add(prDiscoveryRetryInterval))
	waitPRDiscovery(t, s, "claw-partial-live")
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM claw_prs`).Scan(&count); err != nil || count != 1 {
		t.Fatalf("complete retry: count=%d err=%v", count, err)
	}
}

func TestPRDiscoveryTerminalReportedPRSkipsScan(t *testing.T) {
	s, mock := discoveryServer(t)
	started := time.Now().Add(-time.Hour)
	run := discoveryRun(t, s, "reported-pr", started)
	associatePRForTest(t, s, run, "owner/repo", 1, taskRunPRStateClosed)
	recordTaskRunEventForTest(t, s, TaskRunEvent{RunID: run, EventKey: "stop", EventType: taskRunEventManualStopBeforeDelivery, OccurredAt: started.Add(30 * time.Minute)})
	s.reconcileUnreportedPRs("")
	if mock.calls != 0 {
		t.Fatalf("reported PR spent quota: %d calls", mock.calls)
	}
}

func TestPRDiscoveryMentionOnlyDoesNotGateDiscoveryOrReminder(t *testing.T) {
	for _, scheduled := range []bool{false, true} {
		t.Run(fmt.Sprint(scheduled), func(t *testing.T) {
			s, mock := discoveryServer(t)
			at := time.Now()
			run := discoveryRun(t, s, "mention-discovery", at.Add(-time.Hour))
			if _, err := s.db.Exec(`INSERT INTO claw_prs(id,claw_id,repo,pr_number,pr_url,mention_only,created_at)
				VALUES('mention','mention-discovery','owner/repo',99,'https://github.com/owner/repo/pull/99',1,?)`, at); err != nil {
				t.Fatal(err)
			}
			if !strings.Contains(s.agentIdleResumeMessageForClaw("mention-discovery", 30*time.Minute), "[DONE]") {
				t.Error("mention-only row suppressed delivery reminder")
			}
			mock.prs = []map[string]interface{}{discoveryPR(1, at.Add(-time.Minute), time.Time{})}
			if scheduled {
				s.claws["mention-discovery"] = idleTestConn("mention-discovery", 30*time.Minute)
				s.scheduleIdlePRDiscovery(at)
				waitPRDiscovery(t, s, "mention-discovery")
			} else {
				s.discoverClawPRs("mention-discovery", run)
			}
			var count int
			if err := s.db.QueryRow(`SELECT COUNT(*) FROM claw_prs WHERE claw_id='mention-discovery' AND mention_only=0`).Scan(&count); err != nil || count != 1 {
				t.Fatalf("mention-only row gated discovery: count=%d err=%v", count, err)
			}
		})
	}
}

func TestPRDiscoveryDeliveryConfirmsWithoutChangingAgentSHA(t *testing.T) {
	for _, path := range []string{"scanner", "done-single", "done-multi"} {
		t.Run(path, func(t *testing.T) {
			s, mock := discoveryServer(t)
			started := time.Now().Add(-time.Hour)
			run := discoveryRun(t, s, "confirm-discovery", started)
			s.hubCfg.Factories = []*types.FactoryConfig{{Name: "discovery"}}
			if _, err := s.db.Exec(`UPDATE claws SET tags='["factory:discovery"]' WHERE id='confirm-discovery'`); err != nil {
				t.Fatal(err)
			}
			mock.prs = []map[string]interface{}{discoveryPR(1, started.Add(time.Minute), time.Time{}), discoveryPR(2, started.Add(2*time.Minute), time.Time{})}
			s.discoverClawPRs("confirm-discovery", run)
			if _, err := s.db.Exec(`UPDATE task_run_prs SET last_agent_head_sha='agent-sha' WHERE run_id=?`, run); err != nil {
				t.Fatal(err)
			}
			if _, err := s.db.Exec(`UPDATE claw_prs SET last_ci_sha='human-sha' WHERE claw_id='confirm-discovery'`); err != nil {
				t.Fatal(err)
			}
			urls := []string{"https://github.com/owner/repo/pull/1"}
			if path == "done-multi" {
				urls = append(urls, "https://github.com/owner/repo/pull/2")
			}
			// Mentions must not confirm inferred delivery.
			s.scanMessageForPRs("confirm-discovery", urls[0], true, "mention")
			var inferred int
			if err := s.db.QueryRow(`SELECT inferred FROM task_run_prs WHERE run_id=? AND pr_number=1`, run).Scan(&inferred); err != nil || inferred != 1 {
				t.Fatalf("mention confirmed delivery: inferred=%d err=%v", inferred, err)
			}
			for i := 0; i < 2; i++ {
				if path == "scanner" {
					s.scanMessageForPRs("confirm-discovery", urls[0], false, "gate")
				} else if failed := s.registerDonePRURLs("confirm-discovery", urls); failed != "" {
					t.Fatal(failed)
				}
				var sha string
				var opened int
				if err := s.db.QueryRow(`SELECT inferred,last_agent_head_sha FROM task_run_prs WHERE run_id=? AND pr_number=1`, run).Scan(&inferred, &sha); err != nil {
					t.Fatal(err)
				}
				if err := s.db.QueryRow(`SELECT COUNT(*) FROM factory_analytics WHERE claw_id='confirm-discovery' AND action='pr_opened' AND detail='owner/repo#1'`).Scan(&opened); err != nil {
					t.Fatal(err)
				}
				if inferred != 0 || sha != "agent-sha" || opened != 1 {
					t.Fatalf("confirmation: inferred=%d agentSHA=%q opened=%d", inferred, sha, opened)
				}
			}
		})
	}
}

func TestTaskRunPRMissingAssociationLogsSkip(t *testing.T) {
	for _, path := range []string{"refresh", "merge-event"} {
		t.Run(path, func(t *testing.T) {
			s, _ := discoveryServer(t)
			run := discoveryRun(t, s, "missing-association", time.Now().Add(-time.Hour))
			var output bytes.Buffer
			previous := log.Writer()
			log.SetOutput(&output)
			defer log.SetOutput(previous)
			if path == "refresh" {
				if err := s.associateTaskRunPR(TaskRunPR{RunID: run, Repo: "owner/repo", PRNumber: 42, PreserveInference: true, Merged: true, State: taskRunPRStateClosed}); err != nil {
					t.Fatal(err)
				}
			} else {
				recordTaskRunEventForTest(t, s, TaskRunEvent{RunID: run, EventKey: "merge", EventType: taskRunEventPRMerged, Detail: map[string]any{"repo": "owner/repo", "prNumber": 42}})
			}
			if got := output.String(); !strings.Contains(got, "skipping PR") || !strings.Contains(got, run) || !strings.Contains(got, "owner/repo#42") || !strings.Contains(got, "no associated PR") {
				t.Fatalf("missing skip context: %q", got)
			}
			var count int
			if err := s.db.QueryRow(`SELECT COUNT(*) FROM task_run_prs`).Scan(&count); err != nil || count != 0 {
				t.Fatalf("skip created association: count=%d err=%v", count, err)
			}
		})
	}
}

func TestPRDiscoveryTerminalQueryFailure(t *testing.T) {
	s, _ := discoveryServer(t)
	if err := s.db.Close(); err != nil {
		t.Fatal(err)
	}
	var output bytes.Buffer
	previous := log.Writer()
	log.SetOutput(&output)
	defer log.SetOutput(previous)
	if cursor := s.reconcileUnreportedPRs("previous"); cursor != "" {
		t.Fatalf("query error cursor=%q", cursor)
	}
	if !strings.Contains(output.String(), "[pr-discovery] query terminal runs:") {
		t.Fatalf("query error not logged: %q", output.String())
	}
}

func TestPRDiscoveryWebhookRefreshPreservesInference(t *testing.T) {
	for _, path := range []string{"closed-live", "closed-dead", "ready"} {
		t.Run(path, func(t *testing.T) {
			s, _ := discoveryServer(t)
			started := time.Now().Add(-time.Hour)
			run := discoveryRun(t, s, "webhook-inferred", started)
			s.hubCfg.Factories = []*types.FactoryConfig{{Name: "discovery", Integration: "github", Repos: []string{"owner/repo"}, Trigger: &types.GitHubTrigger{On: "pull_request"}}}
			url := "https://github.com/owner/repo/pull/1"
			if err := s.associateTaskRunPR(TaskRunPR{RunID: run, Repo: "owner/repo", PRNumber: 1, URL: url, Exclusive: true}); err != nil {
				t.Fatal(err)
			}
			if failed := s.insertClawPRsAtomic("webhook-inferred", []prMentionCandidate{{repo: "owner/repo", number: 1, url: url, inferred: true}}, false, "done"); failed != "" {
				t.Fatal(failed)
			}
			if path == "closed-dead" {
				if _, err := s.db.Exec(`UPDATE claws SET status='deleted' WHERE id='webhook-inferred'`); err != nil {
					t.Fatal(err)
				}
			}
			payload := githubPRPayload{Action: "closed", Number: 1}
			payload.Repository.FullName = "owner/repo"
			payload.PullRequest.HTMLURL = url
			payload.PullRequest.Merged = true
			payload.PullRequest.MergedAt = started.Add(20 * time.Minute).UTC().Format(time.RFC3339)
			wantState := taskRunPRStateClosed
			if path == "ready" {
				payload.Action = "ready_for_review"
				payload.PullRequest.UpdatedAt = started.Add(10 * time.Minute).UTC().Format(time.RFC3339)
				wantState = taskRunPRStateOpen
			}
			s.processGitHubPREvent(payload)
			var inferred, ready int
			var state string
			if err := s.db.QueryRow(`SELECT inferred,state,ready_at FROM task_run_prs WHERE run_id=?`, run).Scan(&inferred, &state, &ready); err != nil {
				t.Fatal(err)
			}
			if inferred != 1 || state != wantState || (path == "ready" && ready == 0) {
				t.Fatalf("provider refresh: inferred=%d state=%s ready=%d", inferred, state, ready)
			}
		})
	}
}

func TestPRDiscoveryTerminalInferredOnlyRunIsRescanned(t *testing.T) {
	s, mock := discoveryServer(t)
	started := time.Now().Add(-time.Hour)
	run := discoveryRun(t, s, "partial-backfill", started)
	associatePRForTest(t, s, run, "owner/repo", 1, taskRunPRStateClosed)
	// A partial backfill leaves only inferred rows and the run unscanned.
	if _, err := s.db.Exec(`UPDATE task_run_prs SET inferred=1 WHERE run_id=?`, run); err != nil {
		t.Fatal(err)
	}
	recordTaskRunEventForTest(t, s, TaskRunEvent{RunID: run, EventKey: "stop", EventType: taskRunEventManualStopBeforeDelivery, OccurredAt: started.Add(30 * time.Minute)})
	s.reconcileUnreportedPRs("")
	if mock.calls == 0 {
		t.Fatal("run with only inferred PRs was not rescanned")
	}
}

func TestTaskRunPRRefreshKeepsHeadFields(t *testing.T) {
	s, _ := discoveryServer(t)
	run := discoveryRun(t, s, "head-fields", time.Now().Add(-time.Hour))
	if err := s.associateTaskRunPR(TaskRunPR{RunID: run, Repo: "owner/repo", PRNumber: 1, URL: "https://github.com/owner/repo/pull/1",
		HeadSHA: "abc", HeadBranch: "NEXT-1-x", BaseBranch: "main", State: taskRunPRStateOpen}); err != nil {
		t.Fatal(err)
	}
	if err := s.associateTaskRunPR(TaskRunPR{RunID: run, Repo: "owner/repo", PRNumber: 1, URL: "https://github.com/owner/repo/pull/1", State: taskRunPRStateOpen}); err != nil {
		t.Fatal(err)
	}
	var sha, head, base string
	if err := s.db.QueryRow(`SELECT head_sha, head_branch, base_branch FROM task_run_prs WHERE run_id=?`, run).Scan(&sha, &head, &base); err != nil {
		t.Fatal(err)
	}
	if sha != "abc" || head != "NEXT-1-x" || base != "main" {
		t.Fatalf("refresh blanked head fields: sha=%q head=%q base=%q", sha, head, base)
	}
}

func TestPRDiscoveryPromotesSilentlyMergedMentionAndTerminates(t *testing.T) {
	s, mock := discoveryServer(t)
	started := time.Now().Add(-time.Hour).Truncate(time.Second)
	run := discoveryRun(t, s, "claw-mention-merged", started)
	merged := started.Add(20 * time.Minute)
	mock.prs = []map[string]interface{}{discoveryPR(1281, started.Add(time.Minute), merged)}
	// The agent linked the PR mid-work; the watcher resolved the merge silently.
	if _, err := s.db.Exec(`INSERT INTO claw_prs(id,claw_id,repo,pr_number,pr_url,mention_only,state,merged,created_at)
		VALUES('mention-merged','claw-mention-merged','owner/repo',1281,'https://github.com/owner/repo/pull/1281',1,'merged',1,?)`, started); err != nil {
		t.Fatal(err)
	}
	s.discoverClawPRs("claw-mention-merged", run)
	var pr clawPR
	var mention int
	var state string
	if err := s.db.QueryRow("SELECT id,claw_id,repo,pr_number,pr_url,mention_only,state FROM claw_prs WHERE claw_id=?", "claw-mention-merged").
		Scan(&pr.id, &pr.clawID, &pr.repo, &pr.prNumber, &pr.prURL, &mention, &state); err != nil {
		t.Fatal(err)
	}
	if mention != 0 || state != "open" {
		t.Fatalf("promoted row not re-armed for polling: mention=%d state=%s", mention, state)
	}
	if resolved, terminated := s.checkPRMerged(pr, "repo-token"); !resolved || !terminated {
		t.Fatalf("merge: resolved=%v terminated=%v", resolved, terminated)
	}
	var status string
	if err := s.db.QueryRow("SELECT status FROM task_run_summaries WHERE run_id=?", run).Scan(&status); err != nil || status != taskRunStatusClean {
		t.Fatalf("run status=%s err=%v", status, err)
	}
}

func TestPRDiscoveryExpandsCloneFalsePatterns(t *testing.T) {
	s, mock := discoveryServer(t)
	mock.unscopedToken = true
	started := time.Now().Add(-time.Hour).Truncate(time.Second)
	run := discoveryRun(t, s, "claw-glob", started)
	if _, err := s.db.Exec(`UPDATE claws SET github_repos=? WHERE id='claw-glob'`,
		`[{"repo":"owner/repo","permissions":"write"},{"repo":"owner/oth*","permissions":"read","clone":false}]`); err != nil {
		t.Fatal(err)
	}
	mock.prs = []map[string]interface{}{discoveryPR(1281, started.Add(time.Minute), time.Time{})}
	s.discoverClawPRs("claw-glob", run)
	var count int
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM claw_prs WHERE claw_id='claw-glob' AND mention_only=0`).Scan(&count); err != nil || count != 1 {
		t.Fatalf("clone:false pattern made discovery incomplete: count=%d err=%v", count, err)
	}
}

func TestPRDiscoveryConcurrentExplicitDeliveryKeepsWatcher(t *testing.T) {
	s, mock := discoveryServer(t)
	started := time.Now().Add(-time.Hour)
	runA := discoveryRun(t, s, "claw-a", started)
	runB := discoveryRun(t, s, "claw-b", started)
	if _, err := s.db.Exec(`UPDATE task_runs SET issue_id='NEXT-9' WHERE id=?`, runB); err != nil {
		t.Fatal(err)
	}
	mock.prs = []map[string]interface{}{discoveryPR(1, started.Add(time.Minute), time.Time{})}
	s.discoverClawPRs("claw-a", runA)
	var inferred int
	if err := s.db.QueryRow(`SELECT inferred FROM task_run_prs WHERE run_id=?`, runA).Scan(&inferred); err != nil || inferred != 1 {
		t.Fatalf("setup inference=%d err=%v", inferred, err)
	}
	row := prMentionCandidate{repo: "owner/repo", number: 1, url: "https://github.com/owner/repo/pull/1"}
	var once sync.Once
	s.prCommittedHook = func(clawID string) {
		if clawID == "claw-a" {
			// B delivers explicitly between A's confirm commit and A's association.
			once.Do(func() {
				if failed := s.insertClawPRsAtomic("claw-b", []prMentionCandidate{row}, false, "done"); failed != "" {
					t.Error(failed)
				}
			})
		}
	}
	if failed := s.insertClawPRsAtomic("claw-a", []prMentionCandidate{row}, false, "done"); failed != "" {
		t.Fatal(failed)
	}
	for _, claw := range []string{"claw-a", "claw-b"} {
		var n int
		if err := s.db.QueryRow(`SELECT COUNT(*) FROM claw_prs WHERE claw_id=? AND mention_only=0`, claw).Scan(&n); err != nil || n != 1 {
			t.Fatalf("%s watcher rows=%d err=%v", claw, n, err)
		}
	}
	if err := s.db.QueryRow(`SELECT inferred FROM task_run_prs WHERE run_id=?`, runA).Scan(&inferred); err != nil || inferred != 0 {
		t.Fatalf("run A association inferred=%d err=%v", inferred, err)
	}
}

func TestPRDiscoveryTerminalBackfillIsAtomic(t *testing.T) {
	s, mock := discoveryServer(t)
	started := time.Now().Add(-time.Hour).Truncate(time.Second)
	run := discoveryRun(t, s, "claw-atomic", started)
	recordTaskRunEventForTest(t, s, TaskRunEvent{RunID: run, EventKey: "stop", EventType: taskRunEventManualStopBeforeDelivery, OccurredAt: started.Add(30 * time.Minute)})
	mock.prs = []map[string]interface{}{discoveryPR(1, started.Add(time.Minute), time.Time{}), discoveryPR(2, started.Add(2*time.Minute), time.Time{})}
	if _, err := s.db.Exec(`CREATE TRIGGER fail_second_pr BEFORE INSERT ON task_run_prs WHEN NEW.pr_number=2
		BEGIN SELECT RAISE(ABORT, 'second association failed'); END`); err != nil {
		t.Fatal(err)
	}
	s.reconcileUnreportedPRs("")
	var scanned, count int
	var phase string
	if err := s.db.QueryRow(`SELECT tr.pr_discovery_scanned,ts.phase,(SELECT COUNT(*) FROM task_run_prs p WHERE p.run_id=tr.id)
		FROM task_runs tr JOIN task_run_summaries ts ON ts.run_id=tr.id WHERE tr.id=?`, run).Scan(&scanned, &phase, &count); err != nil {
		t.Fatal(err)
	}
	if scanned != 0 || count != 0 || phase != taskRunPhaseTerminal {
		t.Fatalf("partial backfill left the run unretryable: scanned=%d count=%d phase=%s", scanned, count, phase)
	}
	if _, err := s.db.Exec(`DROP TRIGGER fail_second_pr`); err != nil {
		t.Fatal(err)
	}
	s.reconcileUnreportedPRs("")
	if err := s.db.QueryRow(`SELECT pr_discovery_scanned,(SELECT COUNT(*) FROM task_run_prs WHERE run_id=?) FROM task_runs WHERE id=?`, run, run).Scan(&scanned, &count); err != nil {
		t.Fatal(err)
	}
	if scanned != 1 || count != 2 {
		t.Fatalf("retry: scanned=%d count=%d", scanned, count)
	}
}
