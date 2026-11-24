package hub

import (
	"fmt"
	"net/http"
	"reflect"
	"testing"

	"github.com/elasticclaw/elasticclaw/pkg/types"
)

func TestDonePRURLsFallback(t *testing.T) {
	t.Setenv("ELASTICCLAW_HUB_CONFIG", t.TempDir()+"/hub.yaml")
	const clawID = "done-fallback-claw"
	s, db := newTaskRunAnalyticsTestServer(t, clawID)
	for i, state := range []string{"open", "open", "closed", "merged"} {
		url := fmt.Sprintf("https://github.com/owner/repo/pull/%d", i+1)
		var err error
		switch i {
		case 0: // created by the claw, not yet delivered
			_, err = s.storePRRegistration(clawID, "owner/repo", i+1, url, "gh_create")
		case 1:
			_, err = s.storePRMention(clawID, "owner/repo", i+1, url, true, "mention")
		default:
			_, err = s.storePRMention(clawID, "owner/repo", i+1, url, false, "done")
		}
		if err != nil {
			t.Fatal(err)
		}
		if _, err := db.Exec(`UPDATE claw_prs SET state=? WHERE pr_url=?`, state, url); err != nil {
			t.Fatal(err)
		}
	}
	for _, tc := range []struct {
		message  string
		urls     []string
		fallback bool
	}{
		{"[DONE]", []string{"https://github.com/owner/repo/pull/1"}, true},
		{"[DONE] https://github.com/owner/repo/pull/8", []string{"https://github.com/owner/repo/pull/8"}, false},
	} {
		urls, fallback := s.donePRURLs(clawID, tc.message)
		if !reflect.DeepEqual(urls, tc.urls) || fallback != tc.fallback {
			t.Fatalf("%s: %v, %t", tc.message, urls, fallback)
		}
	}
	// The PR that triggered a PR-driven claw is not its delivery.
	if _, err := s.storePRMention(clawID, "owner/repo", 9, "https://github.com/owner/repo/pull/9", false, "trigger"); err != nil {
		t.Fatal(err)
	}
	// Rows stored before the source column existed may be trigger PRs too.
	if _, err := db.Exec(`INSERT INTO claw_prs(id, claw_id, repo, pr_number, pr_url, mention_only, source, created_at)
		VALUES('legacy-row', ?, 'owner/repo', 10, 'https://github.com/owner/repo/pull/10', 0, '', ?)`, clawID, now()); err != nil {
		t.Fatal(err)
	}
	if urls, _ := s.donePRURLs(clawID, "[DONE]"); !reflect.DeepEqual(urls, []string{"https://github.com/owner/repo/pull/1"}) {
		t.Fatalf("trigger or pre-migration PR in fallback: %v", urls)
	}
	if _, err := db.Exec(`UPDATE claw_prs SET state='closed' WHERE pr_number=1`); err != nil {
		t.Fatal(err)
	}
	if urls, fallback := s.donePRURLs(clawID, "[DONE]"); len(urls) != 0 || fallback {
		t.Fatalf("closed/mentioned fallback: %v, %t", urls, fallback)
	}
}

func TestHandleClawDoneSignalRegisteredFallbackValidation(t *testing.T) {
	t.Setenv("ELASTICCLAW_HUB_CONFIG", t.TempDir()+"/hub.yaml")
	for _, pipeline := range []bool{false, true} {
		for _, state := range []string{"open", "closed"} {
			t.Run(fmt.Sprintf("pipeline=%t/%s", pipeline, state), func(t *testing.T) {
				const clawID = "done-fallback-claw"
				s, db := newTaskRunAnalyticsTestServer(t, clawID)
				runID, _ := startTaskRunForTest(t, s, clawID, "fallback")
				if _, err := db.Exec(`UPDATE claws SET linear_issue_id='ISSUE-fallback',tags='["factory:fallback"]',pipeline_stage='working' WHERE id=?`, clawID); err != nil {
					t.Fatal(err)
				}
				s.hubCfg.Factories = []*types.FactoryConfig{{Name: "fallback", Template: "elasticclaw"}}
				if pipeline {
					s.hubCfg.Factories = []*types.FactoryConfig{{Name: "fallback", Template: "elasticclaw", PipelineYAML: `
stages:
  - id: working
    entry: true
  - id: review
    triggers:
      - message_contains: "[DONE]"
    on_enter:
      inject: "Review started"
`}}
				}
				if _, err := s.storePRRegistration(clawID, "owner/repo", 1, "https://github.com/owner/repo/pull/1", "gh_create"); err != nil {
					t.Fatal(err)
				}
				branchSweepTestToken(s)
				oldClient := defaultGitHubClient
				defaultGitHubClient = newGitHubClient()
				t.Cleanup(func() { defaultGitHubClient = oldClient })
				calls := 0
				defaultGitHubClient.httpClient = &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
					calls++
					if r.URL.Path != "/repos/owner/repo/pulls/1" {
						t.Fatalf("unexpected request %s", r.URL)
					}
					return githubAppTokenResponse(200, fmt.Sprintf(`{"state":%q}`, state)), nil
				})}
				s.handleClawDoneSignal(clawID, "[DONE]")
				if calls != 1 {
					t.Fatalf("validation calls=%d", calls)
				}
				var stage, status string
				if err := db.QueryRow(`SELECT pipeline_stage,status FROM claws WHERE id=?`, clawID).Scan(&stage, &status); err != nil {
					t.Fatal(err)
				}
				if state == "closed" {
					if stage != "working" || status != "running" {
						t.Fatalf("invalid fallback advanced: stage=%s status=%s", stage, status)
					}
					var count int
					if err := db.QueryRow(`SELECT COUNT(*) FROM messages WHERE claw_id=? AND content LIKE '%PR validation failed%'`, clawID).Scan(&count); err != nil || count != 1 {
						t.Fatalf("validation nudge=%d, %v", count, err)
					}
				} else if pipeline {
					if stage != "review" {
						t.Fatalf("stage=%s", stage)
					}
				} else if status != "idle" {
					t.Fatalf("status=%s", status)
				}
				var mentionOnly int
				var source string
				if err := db.QueryRow(`SELECT mention_only,source FROM claw_prs WHERE claw_id=?`, clawID).Scan(&mentionOnly, &source); err != nil {
					t.Fatal(err)
				}
				// [DONE] is what delivers a registered PR; a rejected one must not.
				if wantDelivered := state == "open"; (mentionOnly == 0) != wantDelivered || (source == "done") != wantDelivered {
					t.Fatalf("row after [DONE]: mention_only=%d source=%q", mentionOnly, source)
				}
				var count int
				if err := db.QueryRow(`SELECT COUNT(*) FROM task_run_events WHERE run_id=? AND event_type=?`, runID, taskRunEventDoneWithoutPR).Scan(&count); err != nil || count != 0 {
					t.Fatalf("done-without-pr=%d, %v", count, err)
				}
			})
		}
	}
}
