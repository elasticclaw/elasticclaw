package hub

import (
	"testing"
	"time"
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
