package hub

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/elasticclaw/elasticclaw/pkg/types"
)

type prDiscoveryTransport struct {
	prs []map[string]interface{}
}

func (m *prDiscoveryTransport) RoundTrip(r *http.Request) (*http.Response, error) {
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
	resetGitHubClientForTest(t)
	mock := &prDiscoveryTransport{}
	old := http.DefaultTransport
	http.DefaultTransport = mock
	t.Cleanup(func() { http.DefaultTransport = old })
	s, _ := NewTestServerWithConfig(t, &types.HubConfig{
		GitHubApps: []*types.GitHubAppConfig{{AppID: 1, PrivateKeyPEM: testGitHubAppPEM(t)}},
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
			s.checkAgentIdleResume(time.Now(), "claw-discovery-live", idleTestConn("claw-discovery-live", 15*time.Minute))
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
	other := discoveryRun(t, s, "claw-discovery-other", started)
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
