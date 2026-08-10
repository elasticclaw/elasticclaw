# V2 Workflow History and Logs

- Date: 2026-08-11
- Status: accepted
- Deciders: TBD

## Context and problem statement

Workflow v2 is shipped and fully supported alongside workflow v1. A v2 run is a durable state machine backed by `workflow_v2_runs` and `workflow_v2_attempts`. The hub already exposes a single-run inspection endpoint (`GET /api/v2/workflow-runs/{runId}`) that returns state, facts, events, transitions, effects, and delivery PRs.

However, operators cannot yet discover or browse v2 runs through the UI or CLI:

- The existing v1 history endpoints (`/api/workspaces/{ws}/workflows/{wf}/cron/runs`) only query the `workflow_runs` table and are semantically tied to the cron scheduler. They return `503` if the cron scheduler is unavailable and have no knowledge of `workflow_v2_runs`.
- The CLI commands `elasticclaw workflow runs` and `elasticclaw workflow logs` use those v1 endpoints, so they fail for v2 workflows.
- Unlike v1, a v2 run can have multiple attempts over its lifetime, and each attempt has its own `claw_id`. Logs are fetched per `claw_id` via `/api/messages/{claw_id}/activity`. Therefore, a simple "one row per run" history view would hide the logs from earlier attempts.

We need a history and logs experience for v2 that is comparable to v1, even before v2 has its own cron scheduler.

## Decision

We will add v2-specific history and logs endpoints and update the CLI to use them for v2 workflows. The existing v1 `/cron/runs` endpoints will remain unchanged.

Key design points:

1. **One row per attempt in the v2 history view.** A v2 run can span multiple attempts, each with its own `claw_id`. Listing attempts as rows lets the operator see every provisioned claw and fetch the correct log stream for each.
2. **Add `trigger_type` to `workflow_v2_runs`.** This lets the history view show how each run was started (manual, scheduled, event-driven) and mirrors the `trigger_type` field on v1 `workflow_runs`.
3. **Reuse the existing `/api/messages/{claw_id}/activity` endpoint for the actual log content.** The new endpoints only resolve `run_id`/`attempt_id` → `claw_id`.
4. **Keep v1 endpoints untouched.** The CLI and UI will branch on the workflow's `schemaVersion` to pick the right endpoint.

### New API endpoints

| Endpoint | Purpose |
|---|---|
| `GET /api/v2/workspaces/{workspace}/workflows/{workflow}/runs` | List v2 history as one row per attempt. |
| `GET /api/v2/workflow-runs/{runId}/logs` | Logs for the current attempt of a run. |
| `GET /api/v2/workflow-runs/{runId}/attempts` | List all attempts of a run (optional, for an attempt picker). |
| `GET /api/v2/workflow-runs/{runId}/attempts/{attemptId}/logs` | Logs for a specific attempt. |

`GET /api/v2/workflow-runs/{runId}` already exists for single-run inspection and is not changed.

### Example list response

```json
{
  "runs": [
    {
      "run_id": "run-abc",
      "attempt_id": "attempt-1",
      "attempt_number": 1,
      "claw_id": "claw-xyz",
      "workspace_name": "engineering",
      "workflow_name": "dependency-update",
      "state": "awaiting_ci",
      "display_phase": "pr",
      "run_status": "active",
      "attempt_status": "active",
      "trigger_type": "manual",
      "started_at": "2026-08-11T10:00:00Z",
      "finished_at": null,
      "updated_at": "2026-08-11T10:05:00Z"
    }
  ],
  "next_cursor": "..."
}
```

### Database changes

```sql
ALTER TABLE workflow_v2_runs ADD COLUMN trigger_type TEXT NOT NULL DEFAULT 'manual';
```

Populate `trigger_type` when a run is created:
- `manual` for `POST /api/workspaces/{ws}/workflows/{wf}/trigger`
- `cron` for the future v2 cron scheduler
- `event` for PR/CI/review event adapters

Existing rows can be backfilled to `'manual'` or `'unknown'`.

### CLI changes

No new top-level commands. Existing commands branch on the workflow's `schemaVersion`.

#### How the CLI determines the schema version

The CLI decodes the workflow list/detail response into `workflowCLIView`. Today that struct does not include `SchemaVersion`. We will add it:

```go
type workflowCLIView struct {
    ...
    SchemaVersion string `json:"schemaVersion"`
}
```

Before listing runs or fetching logs, the CLI fetches the workflow view (`GET /api/workspaces/{ws}/workflows/{name}`) and checks `v2.IsV2(workflow.SchemaVersion)`:

- v1 (`schemaVersion == "v1"` or empty): use the existing `/cron/runs` endpoints.
- v2 (`schemaVersion == "2"` or `"v2"`): use the new `/api/v2` endpoints.

This adds one extra HTTP round-trip for `workflow runs` and `workflow logs`, but it is explicit, reliable, and avoids fragile "try both endpoints" fallback logic.

#### Commands

```bash
# List history. For v2, calls GET /api/v2/workspaces/{ws}/workflows/{wf}/runs
elasticclaw workflow runs dependency-update --workspace engineering --limit 50

# Logs for the current attempt. For v2, calls GET /api/v2/workflow-runs/{runId}/logs
elasticclaw workflow logs dependency-update run-abc

# Logs for a specific attempt. For v2, calls GET /api/v2/workflow-runs/{runId}/attempts/{attemptId}/logs
elasticclaw workflow logs dependency-update run-abc --attempt attempt-1
```

### UI changes

- The workflow list API already returns `schemaVersion`. The history tab uses it to pick the endpoint.
- For v2 workflows, render the history table with one row per attempt and columns: Run ID, Attempt, State, Phase, Status, Trigger, Started, Updated, Actions.
- **Inspect** opens `GET /api/v2/workflow-runs/{runId}`.
- **Logs** opens `GET /api/v2/workflow-runs/{runId}/logs` (current attempt) or the attempt-specific logs endpoint.
- Optionally provide an attempt picker using `GET /api/v2/workflow-runs/{runId}/attempts`.

## Consequences

- v2 workflows become fully observable in both the UI and CLI without waiting for v2 cron support.
- v1 history endpoints and CLI behavior remain unchanged and unaffected.
- The UI and CLI must branch on `schemaVersion`. This is a small increase in client complexity but avoids a fragile union response schema.
- The database schema gains one column on `workflow_v2_runs`.
- Future v2 cron scheduling can reuse the same history and logs endpoints.

## Alternatives considered

1. **Reuse the existing v1 `/cron/runs` endpoints for v2.**
   - Rejected because the handlers require the cron scheduler to be running (`503` if `s.cronScheduler == nil`), the path implies cron, and the response schema (`types.WorkflowRun`) has no room for v2 concepts like `state`, `display_phase`, or `attempt_number`.

2. **Introduce a single generalized `/runs` endpoint for both v1 and v2.**
   - Cleaner long-term but more invasive. It would require changing the v1 query path and the CLI path, and it would still need to return a union schema. Deferred in favor of v2-specific endpoints that can be unified later.

3. **List one row per v2 run instead of per attempt.**
   - Rejected because a v2 run can have multiple attempts with different `claw_id`s. A single row per run would only expose the current attempt's logs and hide earlier attempts.

## Related ADRs

- None yet.
