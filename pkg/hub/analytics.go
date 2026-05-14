package hub

import (
	"fmt"
	"log"
	"net/http"
	"strings"
	"time"

	"github.com/google/uuid"
)

// logFactoryAnalytics records a persistent analytics event for factory metrics.
func (s *Server) logFactoryAnalytics(factoryName, issueID, clawID, action, detail, result string) {
	id := uuid.New().String()
	_, _ = s.db.Exec(
		`INSERT INTO factory_analytics(id,factory_name,issue_id,claw_id,action,detail,result,created_at) VALUES(?,?,?,?,?,?,?,?)`,
		id, factoryName, issueID, clawID, action, detail, result, now(),
	)
}

// AnalyticsSummary provides aggregated metrics for a factory.
type AnalyticsSummary struct {
	FactoryName           string            `json:"factoryName"`
	TotalTriggers         int               `json:"totalTriggers"`
	SuccessfulCreations   int               `json:"successfulCreations"`
	FailedCreations       int               `json:"failedCreations"`
	Terminations          int               `json:"terminations"`
	PROpened              int               `json:"prOpened"`
	PRMerged              int               `json:"prMerged"`
	PRClosed              int               `json:"prClosed"`
	DoneSignals           int               `json:"doneSignals"`
	Errors                int               `json:"errors"`
	SuccessRate           float64           `json:"successRate"` // percentage of creations that succeeded
	PRMergeRate           float64           `json:"prMergeRate"` // percentage of opened PRs that merged
	ByTriggerStatus       map[string]int    `json:"byTriggerStatus"`
	RecentEvents          []AnalyticsEvent  `json:"recentEvents"`
	ComputeError          string            `json:"computeError,omitempty"` // set if aggregation failed for this factory
}

// AnalyticsEvent is a single analytics record.
type AnalyticsEvent struct {
	ID         string `json:"id"`
	FactoryName string `json:"factoryName"`
	IssueID    string `json:"issueId"`
	ClawID     string `json:"clawId"`
	Action     string `json:"action"`
	Detail     string `json:"detail"`
	Result     string `json:"result"`
	CreatedAt  string `json:"createdAt"`
}

// handleFactoryAnalytics serves GET /api/factories/:name/analytics
func (s *Server) handleFactoryAnalytics(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	// Path: /api/factories/:name/analytics
	parts := strings.Split(strings.TrimPrefix(r.URL.Path, "/api/factories/"), "/")
	if len(parts) < 2 || parts[1] != "analytics" {
		http.Error(w, "not found", http.StatusNotFound)
		return
	}
	factoryName := parts[0]

	// Time range filter (default: last 30 days)
	days := 30
	if d := r.URL.Query().Get("days"); d != "" {
		fmt.Sscanf(d, "%d", &days)
		if days < 1 {
			days = 30
		}
		if days > 365 {
			days = 365
		}
	}
	since := time.Now().UTC().AddDate(0, 0, -days).Format(time.RFC3339)

	summary, err := s.computeFactoryAnalytics(factoryName, since)
	if err != nil {
		log.Printf("[analytics] error computing analytics for %s: %v", factoryName, err)
		http.Error(w, "db error", http.StatusInternalServerError)
		return
	}

	jsonOK(w, summary)
}

// handleAllFactoriesAnalytics serves GET /api/analytics/factories
func (s *Server) handleAllFactoriesAnalytics(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	// Time range filter (default: last 30 days)
	days := 30
	if d := r.URL.Query().Get("days"); d != "" {
		fmt.Sscanf(d, "%d", &days)
		if days < 1 {
			days = 30
		}
		if days > 365 {
			days = 365
		}
	}
	since := time.Now().UTC().AddDate(0, 0, -days).Format(time.RFC3339)

	// Get all factory names from analytics
	rows, err := s.db.Query(
		`SELECT DISTINCT factory_name FROM factory_analytics WHERE created_at > ? ORDER BY factory_name`,
		since,
	)
	if err != nil {
		log.Printf("[analytics] error listing factories: %v", err)
		http.Error(w, "db error", http.StatusInternalServerError)
		return
	}

	var factoryNames []string
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			continue
		}
		factoryNames = append(factoryNames, name)
	}
	rows.Close()

	var summaries []AnalyticsSummary
	var hasErrors bool
	for _, name := range factoryNames {
		summary, err := s.computeFactoryAnalytics(name, since)
		if err != nil {
			hasErrors = true
			summaries = append(summaries, AnalyticsSummary{
				FactoryName:     name,
				ComputeError:    err.Error(),
				ByTriggerStatus: make(map[string]int),
				RecentEvents:    []AnalyticsEvent{},
			})
			continue
		}
		summaries = append(summaries, *summary)
	}

	if summaries == nil {
		summaries = []AnalyticsSummary{}
	}
	if hasErrors {
		w.Header().Set("X-Analytics-Partial", "true")
	}
	jsonOK(w, summaries)
}

// computeFactoryAnalytics builds an AnalyticsSummary for a factory.
func (s *Server) computeFactoryAnalytics(factoryName, since string) (*AnalyticsSummary, error) {
	summary := &AnalyticsSummary{
		FactoryName:     factoryName,
		ByTriggerStatus: make(map[string]int),
		RecentEvents:    []AnalyticsEvent{},
	}

	// Count actions
	rows, err := s.db.Query(
		`SELECT action, COUNT(*) FROM factory_analytics 
		 WHERE factory_name = ? AND created_at > ? 
		 GROUP BY action`,
		factoryName, since,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	for rows.Next() {
		var action string
		var count int
		if err := rows.Scan(&action, &count); err != nil {
			continue
		}
		switch action {
		case "claw_created":
			summary.TotalTriggers += count
			summary.SuccessfulCreations += count
		case "error":
			summary.TotalTriggers += count
			summary.FailedCreations += count
			// Also count errors separately for clarity
			summary.Errors += count
		case "claw_terminated":
			summary.Terminations += count
		case "pr_opened":
			summary.PROpened += count
		case "pr_merged":
			summary.PRMerged += count
		case "pr_closed":
			summary.PRClosed += count
		case "done_signal":
			summary.DoneSignals += count
		}
	}

	// Compute rates
	if summary.TotalTriggers > 0 {
		summary.SuccessRate = float64(summary.SuccessfulCreations) / float64(summary.TotalTriggers) * 100
	}
	if summary.PROpened > 0 {
		summary.PRMergeRate = float64(summary.PRMerged) / float64(summary.PROpened) * 100
	}

	// Get recent events (last 50)
	recentRows, err := s.db.Query(
		`SELECT id, factory_name, issue_id, claw_id, action, detail, result, created_at 
		 FROM factory_analytics 
		 WHERE factory_name = ? AND created_at > ? 
		 ORDER BY created_at DESC LIMIT 50`,
		factoryName, since,
	)
	if err != nil {
		return nil, err
	}
	defer recentRows.Close()

	for recentRows.Next() {
		var e AnalyticsEvent
		if err := recentRows.Scan(&e.ID, &e.FactoryName, &e.IssueID, &e.ClawID, &e.Action, &e.Detail, &e.Result, &e.CreatedAt); err != nil {
			continue
		}
		summary.RecentEvents = append(summary.RecentEvents, e)
	}

	// Get trigger status breakdown from factory_events (which has more detail)
	// We join with factory_events to get status transitions
	triggerRows, err := s.db.Query(
		`SELECT new_status, COUNT(*) FROM factory_events 
		 WHERE factory_name = ? AND created_at > ? AND action = 'claw_created'
		 GROUP BY new_status`,
		factoryName, since,
	)
	if err != nil {
		// factory_events may not have data, that's ok
		triggerRows = nil
	}
	if triggerRows != nil {
		defer triggerRows.Close()
		for triggerRows.Next() {
			var status string
			var count int
			if err := triggerRows.Scan(&status, &count); err != nil {
				continue
			}
			if status != "" {
				summary.ByTriggerStatus[status] = count
			}
		}
	}

	return summary, nil
}

// trackFactoryCreationSuccess logs a successful claw creation for analytics.
func (s *Server) trackFactoryCreationSuccess(factoryName, issueID, clawID string) {
	s.logFactoryAnalytics(factoryName, issueID, clawID, "claw_created", "", "success")
}

// trackFactoryCreationFailure logs a failed claw creation for analytics.
func (s *Server) trackFactoryCreationFailure(factoryName, issueID, detail string) {
	s.logFactoryAnalytics(factoryName, issueID, "", "error", detail, "failure")
}

// trackFactoryTermination logs a claw termination for analytics.
func (s *Server) trackFactoryTermination(factoryName, issueID, clawID, reason string) {
	s.logFactoryAnalytics(factoryName, issueID, clawID, "claw_terminated", reason, "")
}

// trackPROpened logs a PR opening for analytics.
func (s *Server) trackPROpened(factoryName, issueID, clawID, repo string, prNumber int) {
	s.logFactoryAnalytics(factoryName, issueID, clawID, "pr_opened", fmt.Sprintf("%s#%d", repo, prNumber), "")
}

// trackPRMerged logs a PR merge for analytics.
func (s *Server) trackPRMerged(factoryName, issueID, clawID, repo string, prNumber int) {
	s.logFactoryAnalytics(factoryName, issueID, clawID, "pr_merged", fmt.Sprintf("%s#%d", repo, prNumber), "success")
}

// trackPRClosed logs a PR close (without merge) for analytics.
func (s *Server) trackPRClosed(factoryName, issueID, clawID, repo string, prNumber int) {
	s.logFactoryAnalytics(factoryName, issueID, clawID, "pr_closed", fmt.Sprintf("%s#%d", repo, prNumber), "failure")
}

// trackDoneSignal logs a [DONE] signal for analytics.
func (s *Server) trackDoneSignal(factoryName, issueID, clawID string, prCount int) {
	s.logFactoryAnalytics(factoryName, issueID, clawID, "done_signal", fmt.Sprintf("prs=%d", prCount), "success")
}


