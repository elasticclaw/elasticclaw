package hub

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/elasticclaw/elasticclaw/pkg/types"
)

func infraEventTypes(t *testing.T, s *Server) []string {
	t.Helper()
	rows, err := s.db.Query(`SELECT event_type FROM infra_events ORDER BY rowid`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var eventType string
		if err := rows.Scan(&eventType); err != nil {
			t.Fatal(err)
		}
		out = append(out, eventType)
	}
	return out
}

func TestDependencyWatcherDebounceAndUnknown(t *testing.T) {
	s, _ := NewTestServerWithConfig(t, nil, "", "", "")
	base := time.Date(2026, 9, 1, 12, 0, 0, 0, time.UTC)
	bad := DependencyStatus{ID: "model:test", Name: "Test", Status: dependencyStatusDowntime, Message: "down"}
	if err := s.observeDependencyStatus(bad, base, 0); err != nil {
		t.Fatal(err)
	}
	if got := infraEventTypes(t, s); len(got) != 0 {
		t.Fatalf("single bad check emitted %v", got)
	}
	var sinceBefore int64
	if err := s.db.QueryRow(`SELECT since FROM dependency_status_state WHERE id='model:test'`).Scan(&sinceBefore); err != nil {
		t.Fatal(err)
	}
	s.nowFunc = func() time.Time { return base.Add(30 * time.Second) }
	s.dependencyStatus.mu.Lock()
	s.dependencyStatus.cache = &DependencyStatusResponse{Dependencies: []DependencyStatus{{ID: "model:test", Status: dependencyStatusUnknown}}, CheckedAt: now()}
	s.dependencyStatus.mu.Unlock()
	s.dependencyWatcherTick(t.Context(), base.Add(30*time.Second))
	var sinceAfter int64
	if err := s.db.QueryRow(`SELECT since FROM dependency_status_state WHERE id='model:test'`).Scan(&sinceAfter); err != nil {
		t.Fatal(err)
	}
	if sinceBefore != sinceAfter {
		t.Fatalf("unknown reset since: %d -> %d", sinceBefore, sinceAfter)
	}
	if got := infraEventTypes(t, s); len(got) != 0 {
		t.Fatalf("unknown emitted %v", got)
	}
	if err := s.observeDependencyStatus(bad, base.Add(time.Minute), 0); err != nil {
		t.Fatal(err)
	}
	if got := infraEventTypes(t, s); strings.Join(got, ",") != "dependency_down" {
		t.Fatalf("events = %v, want dependency_down", got)
	}
}

// Regression: the snapshot cache lives five minutes while the watcher ticks
// every one, so the SAME vendor-page fetch is re-served to consecutive ticks.
// A re-served snapshot must not count as the second consecutive check the
// debounce demands — otherwise one flapping observation pages the channel a
// minute later all by itself.
func TestDependencyWatcherCachedSnapshotDoesNotSatisfyDebounce(t *testing.T) {
	s, _ := NewTestServerWithConfig(t, nil, "", "", "")
	seed := func(checkedAt time.Time) {
		s.dependencyStatus.mu.Lock()
		s.dependencyStatus.cache = &DependencyStatusResponse{
			Dependencies: []DependencyStatus{{ID: "model:test", Name: "Test", Status: dependencyStatusDowntime, Message: "down", CheckedAt: checkedAt}},
			CheckedAt:    checkedAt,
		}
		s.dependencyStatus.mu.Unlock()
	}
	// CheckedAt must sit inside the cache TTL relative to the wall clock, or
	// snapshotForWatcher would refresh over the network instead of re-serving.
	checked := time.Now().UTC()
	seed(checked)
	s.dependencyWatcherTick(t.Context(), checked)
	s.dependencyWatcherTick(t.Context(), checked.Add(time.Minute))
	if got := infraEventTypes(t, s); len(got) != 0 {
		t.Fatalf("two ticks over one cached fetch emitted %v; the debounce needs two distinct observations", got)
	}
	seed(checked.Add(time.Minute))
	s.dependencyWatcherTick(t.Context(), checked.Add(2*time.Minute))
	if got := infraEventTypes(t, s); strings.Join(got, ",") != "dependency_down" {
		t.Fatalf("events after a genuinely new fetch = %v, want dependency_down", got)
	}
}

// repeat_after re-alerts while a dependency is still degraded/down, once per
// elapsed interval; zero (the default) never repeats, and recovery resets the
// repeat clock.
func TestDependencyWatcherRepeatAfterReAlerts(t *testing.T) {
	s, _ := NewTestServerWithConfig(t, nil, "", "", "")
	base := time.Date(2026, 9, 1, 12, 0, 0, 0, time.UTC)
	observe := func(at time.Time, repeatAfter time.Duration) {
		t.Helper()
		dep := DependencyStatus{ID: "model:test", Name: "Test", Status: dependencyStatusDowntime, Message: "down", CheckedAt: at}
		if err := s.observeDependencyStatus(dep, at, repeatAfter); err != nil {
			t.Fatal(err)
		}
	}
	observe(base, time.Hour)
	observe(base.Add(time.Minute), time.Hour)
	if got := infraEventTypes(t, s); strings.Join(got, ",") != "dependency_down" {
		t.Fatalf("events = %v, want dependency_down", got)
	}
	// Half an hour later the repeat is not yet due.
	observe(base.Add(31*time.Minute), time.Hour)
	if got := infraEventTypes(t, s); strings.Join(got, ",") != "dependency_down" {
		t.Fatalf("events before repeat_after elapsed = %v", got)
	}
	// An hour past the first alert, the repeat fires under a fresh event key.
	observe(base.Add(62*time.Minute), time.Hour)
	if got := infraEventTypes(t, s); strings.Join(got, ",") != "dependency_down,dependency_down" {
		t.Fatalf("events after repeat_after elapsed = %v", got)
	}
	// The default (zero) never repeats, no matter how stale the last alert is.
	observe(base.Add(5*time.Hour), 0)
	if got := infraEventTypes(t, s); strings.Join(got, ",") != "dependency_down,dependency_down" {
		t.Fatalf("repeat fired with repeat_after unset: %v", got)
	}
	good := DependencyStatus{ID: "model:test", Name: "Test", Status: dependencyStatusOperational, CheckedAt: base.Add(6 * time.Hour)}
	if err := s.observeDependencyStatus(good, base.Add(6*time.Hour), time.Hour); err != nil {
		t.Fatal(err)
	}
	if got := infraEventTypes(t, s); strings.Join(got, ",") != "dependency_down,dependency_down,dependency_recovered" {
		t.Fatalf("events after recovery = %v", got)
	}
}

func TestInfraRepeatAfterConfig(t *testing.T) {
	s, _ := NewTestServerWithConfig(t, &types.HubConfig{Notifications: &types.NotificationsConfig{
		Infra: &types.InfraNotificationsConfig{RepeatAfter: "45m"},
	}}, "", "", "")
	if got := s.infraRepeatAfter(); got != 45*time.Minute {
		t.Fatalf("infraRepeatAfter = %v, want 45m", got)
	}
	s.hubCfg.Notifications.Infra.RepeatAfter = ""
	if got := s.infraRepeatAfter(); got != 0 {
		t.Fatalf("empty repeat_after = %v, want 0 (off)", got)
	}
}

func TestDependencyWatcherRecoveryDoesNotReAlertAfterRestart(t *testing.T) {
	s, _ := NewTestServerWithConfig(t, nil, "", "", "")
	base := time.Date(2026, 9, 1, 12, 0, 0, 0, time.UTC)
	bad := DependencyStatus{ID: "model:test", Name: "Test", Status: dependencyStatusDowntime}
	for _, at := range []time.Time{base, base.Add(time.Minute)} {
		if err := s.observeDependencyStatus(bad, at, 0); err != nil {
			t.Fatal(err)
		}
	}
	good := DependencyStatus{ID: "model:test", Name: "Test", Status: dependencyStatusOperational}
	if err := s.observeDependencyStatus(good, base.Add(2*time.Minute), 0); err != nil {
		t.Fatal(err)
	}
	if err := s.observeDependencyStatus(good, base.Add(3*time.Minute), 0); err != nil {
		t.Fatal(err)
	}
	if got := infraEventTypes(t, s); strings.Join(got, ",") != "dependency_down,dependency_recovered" {
		t.Fatalf("events = %v", got)
	}
}

func TestLLMUsageLimitInfraEventsArePerKeyAndSecretSafe(t *testing.T) {
	s, _ := NewTestServerWithConfig(t, nil, "", "", "")
	const key = "sk-this-must-never-appear"
	for i := 0; i < 4; i++ {
		limitClaw(t, s, fmt.Sprintf("shared-key-%d", i), key, false)
	}
	// The provider message quotes a raw token UNRELATED to the configured key
	// id: literal key-id substitution alone would wave it straight through.
	const rawToken = "sk-ant-api03-Fak3Tok3nFak3Tok3nFak3"
	limit := types.LLMUsageLimit{Reason: types.LLMLimitUsage, RegainAt: now().Add(time.Hour), Message: "account " + key + " reached its allowance; rejected credential " + rawToken}
	for i := 0; i < 4; i++ {
		s.handleLLMUsageLimit(nil, fmt.Sprintf("shared-key-%d", i), limit)
	}
	if got := infraEventTypes(t, s); strings.Join(got, ",") != "provider_limit_opened" {
		t.Fatalf("events = %v", got)
	}
	var detail, eventKey string
	if err := s.db.QueryRow(`SELECT detail, event_key FROM infra_events`).Scan(&detail, &eventKey); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(detail, key) || strings.Contains(eventKey, key) {
		t.Fatalf("raw key leaked into event: key=%q detail=%q", eventKey, detail)
	}
	if strings.Contains(detail, rawToken) {
		t.Fatalf("raw provider token leaked into event detail: %q", detail)
	}
	var payload map[string]any
	if err := json.Unmarshal([]byte(detail), &payload); err != nil {
		t.Fatal(err)
	}
	if payload["parked_claws"] != float64(4) {
		t.Fatalf("parked_claws = %v, want 4", payload["parked_claws"])
	}
	s.releaseLLMUsageLimit(key, "test release", false)
	if got := infraEventTypes(t, s); strings.Join(got, ",") != "provider_limit_opened,provider_limit_released" {
		t.Fatalf("events after release = %v", got)
	}
	record, _ := s.loadLLMUsageLimit(key)
	record.ReleasedAt, record.Retries = now(), llmLimitMaxAutoRetries-1
	if err := s.storeLLMUsageLimit(record); err != nil {
		t.Fatal(err)
	}
	s.handleLLMUsageLimit(nil, "shared-key-0", limit)
	if got := infraEventTypes(t, s); strings.Join(got, ",") != "provider_limit_opened,provider_limit_released,provider_limit_exhausted" {
		t.Fatalf("events after exhaustion = %v", got)
	}
}
