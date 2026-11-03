package hub

import (
	"encoding/json"
	"errors"
	"fmt"
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
	defaultGitHubClient.haveLimit = true
	defaultGitHubClient.remaining = 0
	defaultGitHubClient.reset = time.Now().Add(time.Hour)
	defaultGitHubClient.mu.Unlock()
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
	if failed := s.insertClawPRsAtomic("claw-inferred", []prMentionCandidate{{repo: "owner/repo", number: 1, url: "https://github.com/owner/repo/pull/1", inferred: true}}, false); failed != "" {
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
	if err := s.associateTaskRunPR(TaskRunPR{RunID: inferred, Repo: "owner/repo", PRNumber: 1, PreserveInference: true, State: taskRunPRStateClosed, Merged: true}); err == nil {
		t.Fatal("stale watcher resurrected displaced association")
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

func TestPRDiscoveryPageLimitKeepsCandidates(t *testing.T) {
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
	prs, err := s.discoverRunPRs(r)
	if err != nil || len(prs) == 0 {
		t.Fatalf("collected candidates lost at page limit: %d %v", len(prs), err)
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
	s, mock := discoveryServer(t)
	started := time.Now().Add(-time.Hour)
	run := discoveryRun(t, s, "claw-partial", started)
	recordTaskRunEventForTest(t, s, TaskRunEvent{RunID: run, EventKey: "stop", EventType: taskRunEventManualStopBeforeDelivery, OccurredAt: started.Add(30 * time.Minute)})
	_, err := s.db.Exec(`UPDATE claws SET github_repos='[{"repo":"owner/repo"},{"repo":"missing/repo"}]' WHERE id='claw-partial'`)
	if err != nil {
		t.Fatal(err)
	}
	key := githubTokenCacheKey([]RepoAccess{{Repo: "missing/repo", Permissions: "read"}})
	s.ghTokenCache = map[string]cachedGitHubToken{key: {expiresAt: time.Now().Add(time.Hour)}}
	// Closed-unmerged keeps the run terminal even after a partial association.
	pr := discoveryPR(1, started.Add(time.Minute), time.Time{})
	pr["state"], pr["closed_at"] = "closed", started.Add(2*time.Minute).Format(time.RFC3339)
	mock.prs = []map[string]interface{}{pr}
	s.reconcileUnreportedPRs("")
	var scanned, count int
	if err := s.db.QueryRow(`SELECT pr_discovery_scanned FROM task_runs WHERE id=?`, run).Scan(&scanned); err != nil {
		t.Fatal(err)
	}
	if scanned != 0 {
		t.Fatal("partial token failure marked complete")
	}
	_ = s.db.QueryRow(`SELECT COUNT(*) FROM task_run_prs WHERE run_id=?`, run).Scan(&count)
	if count != 1 {
		t.Fatal("accessible repository candidate lost")
	}
	delete(s.ghTokenCache, key)
	calls := mock.calls
	s.reconcileUnreportedPRs("")
	if mock.calls <= calls {
		t.Fatal("partially associated terminal run not retried")
	}
	if err := s.db.QueryRow(`SELECT pr_discovery_scanned FROM task_runs WHERE id=?`, run).Scan(&scanned); err != nil {
		t.Fatal(err)
	}
	if scanned != 1 {
		t.Fatal("successful retry not marked complete")
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
			if failed := s.insertClawPRsAtomic("claw-inferred-delivery", []prMentionCandidate{{repo: "owner/repo", number: 1, url: url, inferred: true}}, false); failed != "" {
				t.Fatal(failed)
			}
			switch path {
			case "message-scanner":
				s.scanMessageForPRs("claw-explicit-delivery", url, false)
			case "done-batch":
				if failed := s.insertClawPRsAtomic("claw-explicit-delivery", []prMentionCandidate{{repo: "owner/repo", number: 1, url: url}}, false); failed != "" {
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
