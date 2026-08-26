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
		QueuedAt: 100, ProvisionStartedAt: 300, StartedAt: 500, FinishedAt: 2000,
	}
	humanWait := taskRunAnalyticsHumanWaitView{
		SignalToPROpenMs: 999, // Deliberately excluded from the aggregate wait bucket.
		PROpenToMergeMs:  200,
		Intervals: []taskRunAnalyticsHumanWaitIntervalView{
			{DurationMs: 100}, {DurationMs: 50},
		},
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
	run := taskRunAnalyticsRunView{StartedAt: 100, FinishedAt: 200}
	humanWait := taskRunAnalyticsHumanWaitView{PROpenToMergeMs: 75}
	activity := dedupeActivityMessages([]taskRunActivityRecord{
		{Kind: "tool", Phase: "completed", CallID: "one", DurationMs: 80},
	})

	got := taskRunAnalyticsWallClock(run, humanWait, activity)
	if got.UnattributedTimeMs != 0 {
		t.Fatalf("unattributed time = %d, want 0", got.UnattributedTimeMs)
	}
}
