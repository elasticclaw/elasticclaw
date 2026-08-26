package hub

import "testing"

func TestDedupeActivityMessagesToolCalls(t *testing.T) {
	records := []taskRunActivityRecord{
		{Kind: "tool", Phase: "start", CallID: "counted"},
		{Kind: "tool", Phase: "running", CallID: "counted"},
		{Kind: "tool", Phase: "running", CallID: "counted"},
		{Kind: "tool", Phase: "completed", CallID: "counted", DurationMs: 125},
		{Kind: "tool", Phase: "start", CallID: "unfinished"},
		{Kind: "tool", Phase: "running", CallID: "unfinished"},
		{Kind: "still_working", Phase: "completed", CallID: "heartbeat", DurationMs: 999},
		{Kind: "activity", Phase: "running"},
	}

	deduped := dedupeActivityMessages(records)
	var toolTotal int64
	for _, record := range deduped {
		if record.Kind == "still_working" {
			t.Fatal("still_working record was retained")
		}
		if record.Kind == "tool" {
			toolTotal += record.DurationMs
		}
	}
	if toolTotal != 125 {
		t.Fatalf("tool total = %d, want 125; records = %#v", toolTotal, deduped)
	}
	if len(deduped) != 2 {
		t.Fatalf("deduped records = %#v, want terminal tool plus activity", deduped)
	}
}

func TestTaskRunAnalyticsWallClock(t *testing.T) {
	run := taskRunAnalyticsRunView{
		QueuedAt: 100, ProvisionStartedAt: 300, StartedAt: 500, FinishedAt: 2000, PROpenedAt: 1000, MergedAt: 1200,
	}
	humanWait := taskRunAnalyticsHumanWaitView{
		SignalToPROpenMs: 999, // Deliberately excluded from the aggregate wait bucket.
		PROpenToMergeMs:  200,
		Intervals:        []taskRunAnalyticsHumanWaitIntervalView{{StartAt: 600, EndAt: 700, DurationMs: 100}, {StartAt: 800, EndAt: 850, DurationMs: 50}},
	}
	activity := dedupeActivityMessages([]taskRunActivityRecord{
		{Kind: "tool", Phase: "completed", CallID: "one", DurationMs: 400},
		{Kind: "tool", Phase: "done", CallID: "two", DurationMs: 300},
	})

	got := taskRunAnalyticsWallClock(run, humanWait, activity)
	if got.TotalMs != 1500 || got.QueueTimeMs != 200 || got.ToolTimeMs != 700 || got.HumanWaitMs != 350 || got.UnattributedTimeMs != 250 {
		t.Fatalf("wall clock = %#v, want total=1500 queue=200 tool=700 human=350 unattributed=250", got)
	}
}

func TestTaskRunAnalyticsWallClockClampsUnattributedTime(t *testing.T) {
	run := taskRunAnalyticsRunView{StartedAt: 100, FinishedAt: 200, PROpenedAt: 110, MergedAt: 185}
	humanWait := taskRunAnalyticsHumanWaitView{PROpenToMergeMs: 75}
	activity := dedupeActivityMessages([]taskRunActivityRecord{
		{Kind: "tool", Phase: "completed", CallID: "one", DurationMs: 80},
	})

	got := taskRunAnalyticsWallClock(run, humanWait, activity)
	if got.UnattributedTimeMs != 0 {
		t.Fatalf("unattributed time = %d, want 0", got.UnattributedTimeMs)
	}
}

func TestTaskRunAnalyticsWallClockHumanWaitDoesNotDoubleCount(t *testing.T) {
	run := taskRunAnalyticsRunView{StartedAt: 100, FinishedAt: 1000, PROpenedAt: 300, MergedAt: 800}
	humanWait := taskRunAnalyticsHumanWaitView{PROpenToMergeMs: 500, Intervals: []taskRunAnalyticsHumanWaitIntervalView{{StartAt: 400, EndAt: 500, DurationMs: 100}}}
	got := taskRunAnalyticsWallClock(run, humanWait, nil)
	if got.HumanWaitMs != 500 || got.HumanWaitMs > got.TotalMs {
		t.Fatalf("human wait = %d total = %d, want merged 500ms within total", got.HumanWaitMs, got.TotalMs)
	}
}

func TestReadTaskRunActivityRecordsUsesActivityFormat(t *testing.T) {
	s, db := newTaskRunAnalyticsTestServer(t, "claw-activity-format")
	defer db.Close()
	_, err := db.Exec(`INSERT INTO messages(id, claw_id, tenant_id, role, content, format, created_at) VALUES(?,?,?,?,?,?,?)`,
		"activity-format", "claw-activity-format", "test-tenant-id", "activity", "Running go test ./...", `activity:{"kind":"tool","phase":"completed","call_id":"c1","duration_ms":1500}`, 1)
	if err != nil {
		t.Fatalf("insert activity: %v", err)
	}
	records, err := s.readTaskRunActivityRecords("test-tenant-id", []string{"claw-activity-format"})
	if err != nil {
		t.Fatalf("read activity: %v", err)
	}
	if len(records) != 1 || records[0].DurationMs != 1500 {
		t.Fatalf("records = %#v, want one 1500ms tool", records)
	}
}
