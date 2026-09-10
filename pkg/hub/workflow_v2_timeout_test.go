package hub

import (
	"fmt"
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
