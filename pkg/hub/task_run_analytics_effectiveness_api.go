package hub

import (
	"fmt"
	"net/http"
	"sort"
	"time"
)

type taskRunAnalyticsEffectivenessResponse struct {
	OutcomesByDay     []taskRunAnalyticsOutcomesDay       `json:"outcomesByDay"`
	Funnel            taskRunAnalyticsFunnel              `json:"funnel"`
	CostPerMergedPr   taskRunAnalyticsCostPerMerged       `json:"costPerMergedPr"`
	MergeRate         float64                             `json:"mergeRate"`
	SuccessRate       float64                             `json:"successRate"`
	UniqueTickets     int                                 `json:"uniqueTickets"`
	TicketSuccessRate float64                             `json:"ticketSuccessRate"`
	TicketsByDay      []taskRunAnalyticsTicketsDay        `json:"ticketsByDay"`
	RunsPerTicket     []taskRunAnalyticsRunsPerTicket     `json:"runsPerTicket"`
	TopTicketsByCost  []taskRunAnalyticsTopTicket         `json:"topTicketsByCost"`
	Prior             *taskRunAnalyticsEffectivenessPrior `json:"prior,omitempty"`
}
type taskRunAnalyticsEffectivenessPrior struct {
	SuccessRate       float64 `json:"successRate"`
	TicketSuccessRate float64 `json:"ticketSuccessRate"`
	UniqueTickets     int     `json:"uniqueTickets"`
	MergeRate         float64 `json:"mergeRate"`
}
type taskRunAnalyticsOutcomesDay struct {
	Date           string `json:"date"`
	Clean          int    `json:"clean"`
	HumanInTheLoop int    `json:"humanInTheLoop"`
	Warning        int    `json:"warning"`
	Failed         int    `json:"failed"`
}
type taskRunAnalyticsFunnel struct {
	AgentStarted int `json:"agentStarted"`
	PROpened     int `json:"prOpened"`
	PRFinished   int `json:"prFinished"`
}
type taskRunAnalyticsCostPerMerged struct {
	Weekly  []taskRunAnalyticsWeeklyCost `json:"weekly"`
	Average float64                      `json:"average"`
}
type taskRunAnalyticsWeeklyCost struct {
	WeekStart       string  `json:"weekStart"`
	CostUsd         float64 `json:"costUsd"`
	MergedPrs       int     `json:"mergedPrs"`
	CostPerMergedPr float64 `json:"costPerMergedPr"`
}
type taskRunAnalyticsTicketsDay struct {
	Date       string `json:"date"`
	Delivered  int    `json:"delivered"`
	InProgress int    `json:"inProgress"`
	Failed     int    `json:"failed"`
}
type taskRunAnalyticsRunsPerTicket struct {
	Bucket  string `json:"bucket"`
	Tickets int    `json:"tickets"`
}
type taskRunAnalyticsTopTicket struct {
	IssueID    string  `json:"issueId"`
	IssueTitle string  `json:"issueTitle"`
	CostUsd    float64 `json:"costUsd"`
	Runs       int     `json:"runs"`
	Outcome    string  `json:"outcome"`
}

func (s *Server) handleTaskRunAnalyticsEffectiveness(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		jsonError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	f, err := parseTaskRunAnalyticsFilters(r)
	if err != nil {
		jsonError(w, http.StatusBadRequest, err.Error())
		return
	}
	out, err := s.readTaskRunAnalyticsEffectiveness(f)
	if err != nil {
		jsonError(w, http.StatusInternalServerError, "db error")
		return
	}
	prior, err := s.readTaskRunAnalyticsEffectivenessWithTicketAggregates(taskRunAnalyticsPriorFilters(f, time.Now().UTC()), false)
	if err != nil {
		jsonError(w, http.StatusInternalServerError, "db error")
		return
	}
	out.Prior = &taskRunAnalyticsEffectivenessPrior{
		SuccessRate:       prior.SuccessRate,
		TicketSuccessRate: prior.TicketSuccessRate,
		UniqueTickets:     prior.UniqueTickets,
		MergeRate:         prior.MergeRate,
	}
	jsonOK(w, out)
}
func (s *Server) readTaskRunAnalyticsEffectiveness(f taskRunAnalyticsFilters) (taskRunAnalyticsEffectivenessResponse, error) {
	return s.readTaskRunAnalyticsEffectivenessWithTicketAggregates(f, true)
}

func (s *Server) readTaskRunAnalyticsEffectivenessWithTicketAggregates(f taskRunAnalyticsFilters, includeTicketAggregates bool) (taskRunAnalyticsEffectivenessResponse, error) {
	w, a := taskRunAnalyticsSummaryWhere(f)
	rows, err := s.db.Query(`SELECT DATE(started_at/1000,'unixepoch'), status, COUNT(*), COALESCE(SUM(estimated_cost_usd),0), COALESCE(SUM(merged_pr_count),0), COALESCE(SUM(CASE WHEN merged_pr_count>0 THEN 1 ELSE 0 END),0), COALESCE(SUM(CASE WHEN agent_started_at>0 THEN 1 ELSE 0 END),0), COALESCE(SUM(CASE WHEN pr_opened_at>0 THEN 1 ELSE 0 END),0), COALESCE(SUM(CASE WHEN merged_pr_count>0 OR closed_pr_count>0 THEN 1 ELSE 0 END),0) FROM task_run_summaries `+w+` GROUP BY 1,status`, a...)
	if err != nil {
		return taskRunAnalyticsEffectivenessResponse{}, err
	}
	defer rows.Close()
	out := taskRunAnalyticsEffectivenessResponse{
		OutcomesByDay:    make([]taskRunAnalyticsOutcomesDay, 0),
		TicketsByDay:     make([]taskRunAnalyticsTicketsDay, 0),
		RunsPerTicket:    make([]taskRunAnalyticsRunsPerTicket, 0),
		TopTicketsByCost: make([]taskRunAnalyticsTopTicket, 0),
		CostPerMergedPr:  taskRunAnalyticsCostPerMerged{Weekly: make([]taskRunAnalyticsWeeklyCost, 0)},
	}
	byDay := map[string]*taskRunAnalyticsOutcomesDay{}
	weekly := map[string]*taskRunAnalyticsWeeklyCost{}
	var finished, success, opened, merged, mergedRuns int
	for rows.Next() {
		var d, status string
		var n, mp, mpr, as, po, pf int
		var cost float64
		if err = rows.Scan(&d, &status, &n, &cost, &mp, &mpr, &as, &po, &pf); err != nil {
			return out, err
		}
		x := byDay[d]
		if x == nil {
			x = &taskRunAnalyticsOutcomesDay{Date: d}
			byDay[d] = x
		}
		switch status {
		case taskRunStatusClean:
			x.Clean = n
		case taskRunStatusHumanInTheLoop:
			x.HumanInTheLoop = n
		case taskRunStatusWarning:
			x.Warning = n
		case taskRunStatusFailed:
			x.Failed = n
		}
		if status != taskRunStatusRunning {
			finished += n
			if status == taskRunStatusClean || status == taskRunStatusHumanInTheLoop || status == taskRunStatusWarning {
				success += n
			}
		}
		out.Funnel.AgentStarted += as
		out.Funnel.PROpened += po
		out.Funnel.PRFinished += pf
		opened += po
		merged += mp
		mergedRuns += mpr
		t, _ := time.Parse("2006-01-02", d)
		ws := t.AddDate(0, 0, -(int(t.Weekday())+6)%7).Format("2006-01-02")
		q := weekly[ws]
		if q == nil {
			q = &taskRunAnalyticsWeeklyCost{WeekStart: ws}
			weekly[ws] = q
		}
		q.CostUsd += cost
		q.MergedPrs += mp
	}
	if err = rows.Err(); err != nil {
		return out, err
	}
	var finishedTickets, successfulTickets int
	if !includeTicketAggregates {
		ticketRows, err := s.db.Query(`SELECT issue_id, COALESCE(SUM(CASE WHEN status != 'running' THEN 1 ELSE 0 END),0), COALESCE(SUM(CASE WHEN status IN ('clean','human_in_the_loop','warning') THEN 1 ELSE 0 END),0) FROM task_run_summaries `+w+` AND issue_id != '' GROUP BY issue_id`, a...)
		if err != nil {
			return out, err
		}
		defer ticketRows.Close()
		for ticketRows.Next() {
			var issueID string
			var ticketFinished, ticketSuccess int
			if err = ticketRows.Scan(&issueID, &ticketFinished, &ticketSuccess); err != nil {
				return out, err
			}
			if ticketFinished > 0 {
				finishedTickets++
				if ticketSuccess > 0 {
					successfulTickets++
				}
			}
			out.UniqueTickets++
		}
		if err = ticketRows.Err(); err != nil {
			return out, err
		}
	} else {
		ticketRows, err := s.db.Query(`SELECT issue_id,
			COALESCE(SUBSTR(MAX(CASE WHEN issue_title != '' THEN printf('%020d|%s', started_at, issue_title) END), 22), ''),
			COUNT(*),
			COALESCE(SUM(CASE WHEN status != 'running' THEN 1 ELSE 0 END),0),
			COALESCE(SUM(CASE WHEN status IN ('clean','human_in_the_loop','warning') THEN 1 ELSE 0 END),0),
			COALESCE(SUM(CASE WHEN status = 'running' THEN 1 ELSE 0 END),0),
			COALESCE(SUM(estimated_cost_usd),0), MIN(started_at), MAX(started_at)
			FROM task_run_summaries `+w+` AND issue_id != '' GROUP BY issue_id`, a...)
		if err != nil {
			return out, err
		}
		defer ticketRows.Close()
		type ticketAggregate struct {
			issueID, issueTitle                string
			runs, finished, delivered, running int
			cost                               float64
			earliest, latest                   int64
		}
		tickets := map[string]*ticketAggregate{}
		for ticketRows.Next() {
			var t ticketAggregate
			if err = ticketRows.Scan(&t.issueID, &t.issueTitle, &t.runs, &t.finished, &t.delivered, &t.running, &t.cost, &t.earliest, &t.latest); err != nil {
				return out, err
			}
			tickets[t.issueID] = &t
		}
		if err = ticketRows.Err(); err != nil {
			return out, err
		}
		out.UniqueTickets = len(tickets)
		ticketsByDay := map[string]*taskRunAnalyticsTicketsDay{}
		runBuckets := map[string]int{"1": 0, "2": 0, "3": 0, "4+": 0}
		for _, t := range tickets {
			if t.finished > 0 {
				finishedTickets++
				if t.delivered > 0 {
					successfulTickets++
				}
			}
			outcome := "failed"
			if t.delivered > 0 {
				outcome = "delivered"
			} else if t.running > 0 {
				outcome = "in_progress"
			}
			day := time.UnixMilli(t.earliest).UTC().Format("2006-01-02")
			d := ticketsByDay[day]
			if d == nil {
				d = &taskRunAnalyticsTicketsDay{Date: day}
				ticketsByDay[day] = d
			}
			switch outcome {
			case "delivered":
				d.Delivered++
			case "in_progress":
				d.InProgress++
			default:
				d.Failed++
			}
			bucket := "4+"
			if t.runs < 4 {
				bucket = fmt.Sprint(t.runs)
			}
			runBuckets[bucket]++
			if t.cost > 0 {
				out.TopTicketsByCost = append(out.TopTicketsByCost, taskRunAnalyticsTopTicket{IssueID: t.issueID, IssueTitle: t.issueTitle, CostUsd: t.cost, Runs: t.runs, Outcome: outcome})
			}
		}
		for _, d := range ticketsByDay {
			out.TicketsByDay = append(out.TicketsByDay, *d)
		}
		sort.Slice(out.TicketsByDay, func(i, j int) bool { return out.TicketsByDay[i].Date < out.TicketsByDay[j].Date })
		for _, bucket := range []string{"1", "2", "3", "4+"} {
			out.RunsPerTicket = append(out.RunsPerTicket, taskRunAnalyticsRunsPerTicket{Bucket: bucket, Tickets: runBuckets[bucket]})
		}
		sort.Slice(out.TopTicketsByCost, func(i, j int) bool { return out.TopTicketsByCost[i].CostUsd > out.TopTicketsByCost[j].CostUsd })
		if len(out.TopTicketsByCost) > 10 {
			out.TopTicketsByCost = out.TopTicketsByCost[:10]
		}
	}
	for _, x := range byDay {
		out.OutcomesByDay = append(out.OutcomesByDay, *x)
	}
	sort.Slice(out.OutcomesByDay, func(i, j int) bool { return out.OutcomesByDay[i].Date < out.OutcomesByDay[j].Date })
	for _, x := range weekly {
		if x.MergedPrs > 0 {
			x.CostPerMergedPr = x.CostUsd / float64(x.MergedPrs)
		}
		out.CostPerMergedPr.Weekly = append(out.CostPerMergedPr.Weekly, *x)
	}
	sort.Slice(out.CostPerMergedPr.Weekly, func(i, j int) bool {
		return out.CostPerMergedPr.Weekly[i].WeekStart < out.CostPerMergedPr.Weekly[j].WeekStart
	})
	if merged > 0 {
		var cost float64
		for _, x := range out.CostPerMergedPr.Weekly {
			cost += x.CostUsd
		}
		out.CostPerMergedPr.Average = cost / float64(merged)
	}
	if opened > 0 {
		out.MergeRate = float64(mergedRuns) / float64(opened)
	}
	if finished > 0 {
		out.SuccessRate = float64(success) / float64(finished)
	}
	if finishedTickets > 0 {
		out.TicketSuccessRate = float64(successfulTickets) / float64(finishedTickets)
	}
	return out, nil
}

type taskRunAnalyticsCostDriver struct {
	Name            string                      `json:"name"`
	Runs            int                         `json:"runs"`
	SuccessRate     float64                     `json:"successRate"`
	CostUsd         float64                     `json:"costUsd"`
	MergedPrs       int                         `json:"mergedPrs"`
	CostPerMergedPr float64                     `json:"costPerMergedPr"`
	DailyCost       []taskRunAnalyticsDailyCost `json:"dailyCost"`
}

func (s *Server) handleTaskRunAnalyticsCostDrivers(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		jsonError(w, 405, "method not allowed")
		return
	}
	f, err := parseTaskRunAnalyticsFilters(r)
	if err != nil {
		jsonError(w, 400, err.Error())
		return
	}
	group := r.URL.Query().Get("groupBy")
	if group == "" {
		group = "factory"
	}
	if group != "factory" && group != "workflow" {
		jsonError(w, 400, "groupBy must be factory or workflow")
		return
	}
	out, err := s.readTaskRunAnalyticsCostDrivers(f, group)
	if err != nil {
		if err == errTaskRunAnalyticsInvalidCostRange {
			jsonError(w, http.StatusBadRequest, err.Error())
			return
		}
		jsonError(w, 500, "db error")
		return
	}
	jsonOK(w, out)
}
func (s *Server) readTaskRunAnalyticsCostDrivers(f taskRunAnalyticsFilters, group string) ([]taskRunAnalyticsCostDriver, error) {
	if f.FromStartedAt == 0 && f.ToStartedAt == 0 {
		end := time.Now().UTC().Truncate(24 * time.Hour)
		f.FromStartedAt = end.AddDate(0, 0, -30).UnixMilli()
		f.ToStartedAt = end.Add(24*time.Hour - time.Millisecond).UnixMilli()
	} else if f.FromStartedAt == 0 && f.ToStartedAt > 0 {
		// Only `to` was supplied. Anchor the missing lower bound on it (mirroring
		// the costs endpoint's to-only anchoring) so totals and the sparkline use
		// the same window.
		toDay := time.UnixMilli(f.ToStartedAt).UTC().Truncate(24 * time.Hour)
		f.FromStartedAt = toDay.AddDate(0, 0, -30).UnixMilli()
	}
	if f.FromStartedAt > 0 && f.ToStartedAt > 0 && f.ToStartedAt < f.FromStartedAt {
		return nil, errTaskRunAnalyticsInvalidCostRange
	}
	col := "factory_name"
	if group == "workflow" {
		col = "workflow_name"
	}
	w, a := taskRunAnalyticsSummaryWhere(f)
	q := fmt.Sprintf(`SELECT %s, COUNT(*), COALESCE(SUM(estimated_cost_usd),0), COALESCE(SUM(merged_pr_count),0), COALESCE(SUM(CASE WHEN status IN ('clean','human_in_the_loop','warning') THEN 1 ELSE 0 END),0), COALESCE(SUM(CASE WHEN status!='running' THEN 1 ELSE 0 END),0) FROM task_run_summaries %s GROUP BY %s ORDER BY 3 DESC`, col, w, col)
	rows, err := s.db.Query(q, a...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []taskRunAnalyticsCostDriver{}
	for rows.Next() {
		var x taskRunAnalyticsCostDriver
		var suc, fin int
		if err = rows.Scan(&x.Name, &x.Runs, &x.CostUsd, &x.MergedPrs, &suc, &fin); err != nil {
			return nil, err
		}
		if fin > 0 {
			x.SuccessRate = float64(suc) / float64(fin)
		}
		if x.MergedPrs > 0 {
			x.CostPerMergedPr = x.CostUsd / float64(x.MergedPrs)
		}
		out = append(out, x)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	start := time.Now().UTC().AddDate(0, 0, -30).Truncate(24 * time.Hour)
	end := time.Now().UTC().Truncate(24 * time.Hour)
	if f.FromStartedAt > 0 {
		start = time.UnixMilli(f.FromStartedAt).UTC().Truncate(24 * time.Hour)
	}
	if f.ToStartedAt > 0 {
		end = time.UnixMilli(f.ToStartedAt).UTC().Truncate(24 * time.Hour)
	}
	for i := range out {
		for d := start; !d.After(end); d = d.AddDate(0, 0, 1) {
			out[i].DailyCost = append(out[i].DailyCost, taskRunAnalyticsDailyCost{Date: d.Format("2006-01-02")})
		}
	}
	// Cost drivers has no ledger mode: the sparkline always uses per-run
	// attribution against the same eligibility-filtered run set as the row
	// totals above, so totals and sparkline never disagree.
	q = `SELECT s.` + col + `, u.usage_day, COALESCE(SUM(u.committed_cost_usd),0) FROM task_run_usage u JOIN (SELECT run_id, ` + col + ` FROM task_run_summaries ` + w + `) s ON s.run_id=u.run_id WHERE u.tenant_id=?`
	q += ` GROUP BY s.` + col + `, u.usage_day`
	rs, e := s.db.Query(q, append(a, f.TenantID)...)
	if e != nil {
		return nil, e
	}
	values := map[string]map[string]float64{}
	for rs.Next() {
		var name, day string
		var cost float64
		if e = rs.Scan(&name, &day, &cost); e != nil {
			rs.Close()
			return nil, e
		}
		if values[name] == nil {
			values[name] = map[string]float64{}
		}
		day = clampTaskRunAnalyticsUsageDay(day, start, end)
		values[name][day] += cost
	}
	if e = rs.Err(); e != nil {
		rs.Close()
		return nil, e
	}
	rs.Close()
	for i := range out {
		for j := range out[i].DailyCost {
			out[i].DailyCost[j].CostUsd = values[out[i].Name][out[i].DailyCost[j].Date]
		}
	}
	return out, nil
}
