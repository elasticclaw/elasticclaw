package hub

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/elasticclaw/elasticclaw/pkg/hub/workflowv2"
)

func TestWorkflowV2RunTimeoutUsesCronTriggerTimeout(t *testing.T) {
	raw := `
schema_version: 2
name: timed
enabled: true
initial_state: done
states:
  done:
    phase: done
    terminal: true
trigger:
  cron:
    schedule: "0 0 * * *"
    timeout: "90m"
`
	got := workflowV2RunTimeout(raw)
	if got != 90*time.Minute {
		t.Fatalf("timeout = %v, want 90m", got)
	}
}

func TestWorkflowV2RunTimeoutFallsBackToDefault(t *testing.T) {
	cases := []string{
		"",
		`
schema_version: 2
name: no-trigger
enabled: true
initial_state: done
states:
  done:
    phase: done
    terminal: true
`,
		`
schema_version: 2
name: bad-timeout
enabled: true
initial_state: done
states:
  done:
    phase: done
    terminal: true
trigger:
  cron:
    schedule: "0 0 * * *"
    timeout: "not-a-duration"
`,
	}
	for _, raw := range cases {
		got := workflowV2RunTimeout(raw)
		if got != workflowV2DefaultRunTimeout {
			t.Fatalf("timeout for %q = %v, want default %v", raw, got, workflowV2DefaultRunTimeout)
		}
	}
}

func TestWorkflowV2RunTimeoutZeroDisables(t *testing.T) {
	cases := []string{
		"0",
		"0s",
		"0m",
		"0h",
	}
	base := `
schema_version: 2
name: disabled-timeout
enabled: true
initial_state: done
states:
  done:
    phase: done
    terminal: true
trigger:
  cron:
    schedule: "0 0 * * *"
    timeout: %q
`
	for _, timeout := range cases {
		raw := fmt.Sprintf(base, timeout)
		got := workflowV2RunTimeout(raw)
		if got != 0 {
			t.Fatalf("timeout for %q = %v, want 0", timeout, got)
		}
	}
}

func TestWorkflowV2CronZeroTimeoutIsNotReaped(t *testing.T) {
	for _, status := range []string{"active", "suspended"} {
		t.Run(status, func(t *testing.T) {
			s, db := NewTestServerWithConfig(t, nil, "", "", "")
			store := workflowv2.NewStore(db)
			old := time.Now().UTC().Add(-7 * workflowV2DefaultRunTimeout)
			store.SetClock(func() time.Time { return old })
			raw := strings.Replace(signalWorkflowYAML(), "overlap_policy: signal", "overlap_policy: signal\n    timeout: \"0\"", 1)
			run := createSignalTestRun(t, store, "no-timeout", raw)
			var timeoutAt int64
			if err := db.QueryRow(`SELECT timeout_at FROM workflow_v2_runs WHERE id=?`, run.ID).Scan(&timeoutAt); err != nil || timeoutAt != 0 {
				t.Fatalf("timeout_at = %d, err = %v", timeoutAt, err)
			}
			if _, err := db.Exec(`UPDATE workflow_v2_runs SET status=? WHERE id=?`, status, run.ID); err != nil {
				t.Fatal(err)
			}
			s.reapWorkflowV2RunTimeouts(time.Now().UTC(), func() bool {
				t.Error("zero-timeout run consumed reaper budget")
				return true
			})
			updated, err := store.GetRun(context.Background(), run.ID)
			if err != nil || string(updated.Status) != status {
				t.Fatalf("run status = %s, err = %v", updated.Status, err)
			}
		})
	}
}
