package hub

import "testing"

func TestDeriveTaskRunAnalyticsTicketStatusFixtureTickets(t *testing.T) {
	tests := []struct {
		issueID string
		runs    []taskRunAnalyticsTicketRunSummary
		prs     []taskRunAnalyticsTicketPRView
		want    string
	}{
		{issueID: "ADV-812", runs: []taskRunAnalyticsTicketRunSummary{{RunID: "run_8f21c4", Status: "clean"}, {RunID: "run_c41d90", Status: "human_in_the_loop"}, {RunID: "run_6b21f8", Status: "warning"}, {RunID: "run_3c05a1", Status: "failed"}}, prs: []taskRunAnalyticsTicketPRView{{taskRunAnalyticsPRView: taskRunAnalyticsPRView{ID: "pr1", State: "merged"}, RunID: "run_8f21c4"}, {taskRunAnalyticsPRView: taskRunAnalyticsPRView{ID: "pr8", State: "closed"}, RunID: "run_c41d90"}, {taskRunAnalyticsPRView: taskRunAnalyticsPRView{ID: "pr9", State: "closed"}, RunID: "run_6b21f8"}}, want: "delivered"},
		{issueID: "PLT-31", runs: []taskRunAnalyticsTicketRunSummary{{RunID: "run_4a12bd", Status: "clean"}, {RunID: "run_d90c33", Status: "failed"}}, prs: []taskRunAnalyticsTicketPRView{{taskRunAnalyticsPRView: taskRunAnalyticsPRView{ID: "pr6", State: "open"}, RunID: "run_4a12bd"}}, want: "pr_open"},
		{issueID: "SUP-201", runs: []taskRunAnalyticsTicketRunSummary{{RunID: "run_5c33bb", Status: "failed"}, {RunID: "run_e77b02", Status: "failed"}}, want: "failed"},
		{issueID: "ADV-806", runs: []taskRunAnalyticsTicketRunSummary{{RunID: "run_1b77c0", Status: "running"}}, want: "in_progress"},
	}
	for _, test := range tests {
		t.Run(test.issueID, func(t *testing.T) {
			if got := deriveTaskRunAnalyticsTicketStatus(test.runs, test.prs); got != test.want {
				t.Fatalf("deriveTaskRunAnalyticsTicketStatus() = %q, want %q", got, test.want)
			}
		})
	}
}

func TestCollapseTaskRunAnalyticsTicketStory(t *testing.T) {
	story := collapseTaskRunAnalyticsTicketStory([]taskRunAnalyticsTicketStoryEntry{
		{ID: "e1", EventType: "run_queued", Label: taskRunAnalyticsStoryLabels["run_queued"], Time: 1, Count: 1},
		{ID: "e2", EventType: "attempt_retried", Label: taskRunAnalyticsStoryLabels["attempt_retried"], Time: 2, Count: 1},
		{ID: "e3", EventType: "attempt_retried", Label: taskRunAnalyticsStoryLabels["attempt_retried"], Time: 3, Count: 1},
		{ID: "e4", EventType: "ci_failed", Time: 4, Count: 1},
	})
	if len(story) != 2 {
		t.Fatalf("story entries = %d, want 2: %#v", len(story), story)
	}
	if story[1].EventType != "attempt_retried" || story[1].Count != 2 {
		t.Fatalf("retry entry = %#v, want one collapsed retry with count 2", story[1])
	}
}
