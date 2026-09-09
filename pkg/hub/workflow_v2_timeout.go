package hub

import (
	"strings"
	"time"

	typesv2 "github.com/elasticclaw/elasticclaw/pkg/types/v2"
)

// workflowV2DefaultRunTimeout is the safety-net bound for how long a v2 run may
// stay active/suspended before the reaper cancels it. A workflow can set a
// stricter per-run timeout via trigger.cron.timeout.
const workflowV2DefaultRunTimeout = 24 * time.Hour

// workflowV2RunTimeout returns the timeout for a workflow v2 run. If the
// workflow has a cron trigger with a valid timeout duration, that value is used;
// an explicit zero duration disables the run-level timeout; otherwise a
// conservative default keeps long-running agent tasks safe.
func workflowV2RunTimeout(rawConfig string) time.Duration {
	resolved, err := typesv2.ParseAndValidateWorkflow([]byte(rawConfig))
	if err != nil || resolved == nil || resolved.Workflow == nil ||
		resolved.Workflow.Trigger == nil || resolved.Workflow.Trigger.Cron == nil {
		return workflowV2DefaultRunTimeout
	}
	timeout := strings.TrimSpace(resolved.Workflow.Trigger.Cron.Timeout)
	if timeout == "" {
		return workflowV2DefaultRunTimeout
	}
	d, err := time.ParseDuration(timeout)
	if err != nil || d < 0 {
		return workflowV2DefaultRunTimeout
	}
	return d
}
