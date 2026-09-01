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
	if err := s.observeDependencyStatus(bad, base); err != nil {
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
	if err := s.observeDependencyStatus(bad, base.Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	if got := infraEventTypes(t, s); strings.Join(got, ",") != "dependency_down" {
		t.Fatalf("events = %v, want dependency_down", got)
	}
}

func TestDependencyWatcherRecoveryDoesNotReAlertAfterRestart(t *testing.T) {
	s, _ := NewTestServerWithConfig(t, nil, "", "", "")
	base := time.Date(2026, 9, 1, 12, 0, 0, 0, time.UTC)
	bad := DependencyStatus{ID: "model:test", Name: "Test", Status: dependencyStatusDowntime}
	for _, at := range []time.Time{base, base.Add(time.Minute)} {
		if err := s.observeDependencyStatus(bad, at); err != nil {
			t.Fatal(err)
		}
	}
	good := DependencyStatus{ID: "model:test", Name: "Test", Status: dependencyStatusOperational}
	if err := s.observeDependencyStatus(good, base.Add(2*time.Minute)); err != nil {
		t.Fatal(err)
	}
	if err := s.observeDependencyStatus(good, base.Add(3*time.Minute)); err != nil {
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
	limit := types.LLMUsageLimit{Reason: types.LLMLimitUsage, RegainAt: now().Add(time.Hour), Message: "account " + key + " reached its allowance"}
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
