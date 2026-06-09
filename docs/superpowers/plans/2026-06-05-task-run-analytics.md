# Task Run Analytics Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Implement issue #350 task-run analytics so ElasticClaw records PR-scoped task outcomes, exposes auditable summary/run/event APIs, and renders the new scope-aware analytics UI.

**Reference:** [GitHub issue #350](https://github.com/elasticclaw/elasticclaw/issues/350) contains the source specification for schema, instrumentation, API, and UI scope.

**Architecture:** Add durable task-run analytics tables beside the legacy `factory_analytics` tables, keep legacy endpoints intact, and materialize summaries from append-only task-run events plus normalized PR projections. Existing creation, PR watcher, failure, and dashboard paths emit typed events through one helper; dashboard reads use the materialized tables and detail endpoints.

**Tech Stack:** Go, SQLite via `modernc.org/sqlite`, `net/http` handlers, Next.js/React/TypeScript, existing shadcn UI components.

---

## Rollback Plan

- Disable new recording by setting `analytics_enabled=false` on affected factory or workflow configs; set `requires_pr=false` only for enabled code-task runs that should be tracked outside PR-required denominators.
- If a runtime issue appears in the new dashboard only, remove or hide the UI entry point in the sidebar while leaving event recording and legacy factory analytics untouched.
- The new task-run tables are additive. Dropping them requires first removing references from `claws.task_run_id` and `factory_triggers.task_run_id` readers, but core claw creation/provisioning is otherwise independent of the dashboard projection.
- Keep `analytics_enabled` and `requires_pr` independent: `analytics_enabled=true/requires_pr=false` is valid for tracked non-PR code-task runs, while `analytics_enabled=false` excludes the run from the dashboard projection.

---

## File Structure

- Modify `pkg/hub/db.go`: idempotent migrations for task-run tables and compatibility columns.
- Create `pkg/hub/task_run_analytics.go`: event types, helpers, materialization, filters, API view models.
- Create `pkg/hub/task_run_analytics_test.go`: schema, classification, API, cursor/filter tests.
- Modify `pkg/hub/server.go`: register `/api/analytics/*` task-run routes.
- Modify `pkg/hub/analytics.go`: preserve legacy factory analytics and keep route separation explicit.
- Modify `pkg/hub/factory_creator.go`, `pkg/hub/workflow_creator.go`, `pkg/hub/github_webhook.go`, `pkg/hub/external_webhook.go`: link created claws to task runs.
- Modify `pkg/hub/factory_trigger.go`, `pkg/hub/factory_triggers.go`, `pkg/hub/linear.go`, `pkg/hub/shortcut.go`, `pkg/hub/github_issues.go`, `pkg/hub/integration_poller.go`, `pkg/hub/pr_watcher.go`, `pkg/hub/pipeline_runner.go`: emit start/failure/PR/human-interaction events at existing central hooks.
- Modify `web/lib/api.ts`: typed task-run analytics client helpers.
- Modify `web/lib/types.ts`: typed task-run analytics models.
- Modify `web/components/sidebar.tsx`: add the analytics navigation item.
- Create `web/components/task-run-analytics-view.tsx`: full-width analytics surface.
- Modify `web/app/page.tsx`: switch the main content between agents and analytics.

---

### Task 1: Schema, Event Helper, And Materialization

**Files:**
- Modify: `pkg/hub/db.go`
- Create: `pkg/hub/task_run_analytics.go`
- Create: `pkg/hub/task_run_analytics_test.go`

- [ ] **Step 1: Write failing schema tests**

Add tests that open `openDB(":memory:")` and verify:
- `task_runs`, `task_run_attempts`, `task_run_events`, `task_run_prs`, `task_run_summaries` exist.
- `claws.task_run_id` and `factory_triggers.task_run_id` exist.
- CHECK constraints reject invalid `run_kind`, invalid `status`, and invalid JSON `tags/detail`; tests must also assert that `analytics_enabled=1/requires_pr=0` remains valid for tracked non-PR code-task runs.
- required indexes exist through `PRAGMA index_list`.

Run: `go test ./pkg/hub -run 'TestTaskRunSchema' -count=1`
Expected: FAIL because tables/columns do not exist.

- [ ] **Step 2: Add idempotent migrations**

In `migrate(db)` add idempotent `ALTER TABLE` statements for:
- `claws.task_run_id TEXT NOT NULL DEFAULT ''`
- `factory_triggers.task_run_id TEXT NOT NULL DEFAULT ''`

SQLite does not support `ALTER TABLE ... ADD COLUMN IF NOT EXISTS`; follow the existing migration pattern by executing the `ALTER TABLE` and suppressing duplicate-column errors, or pre-check with `PRAGMA table_info()` before adding the column.

Add `CREATE TABLE IF NOT EXISTS` statements matching issue #350 for:
- `task_runs`
- `task_run_attempts`
- `task_run_events`
- `task_run_prs`
- `task_run_summaries`

Use integer epoch milliseconds (`INTEGER`) for all new time fields. Add all CHECK constraints and indexes from the spec that apply to v1.

Run: `go test ./pkg/hub -run 'TestTaskRunSchema' -count=1`
Expected: PASS.

- [ ] **Step 3: Write failing classification/materialization tests**

Add tests for direct helper usage:
- clean success: start -> claw_created -> pr_associated -> pr_opened -> pr_merged produces `clean_success`.
- warning success: same plus `human_pr_comment` produces `warning_success`.
- failure: start -> claw_created -> done_without_pr produces `failed` with `failure_type='done_without_pr'`.
- multi-PR: one merged and another open remains `running`/waiting, then closes/merges terminally.
- idempotency: repeating an event with the same key does not duplicate event rows or change counts.
- out-of-order late warning after merge reclassifies clean to warning.

Run: `go test ./pkg/hub -run 'TestTaskRun(Materialization|Classification|Idempotency)' -count=1`
Expected: FAIL because helpers do not exist.

- [ ] **Step 4: Implement helper and materializer**

In `task_run_analytics.go`, implement:
- enum-style constants for run statuses, phases, warning types, failure types, event types, actor types, sources, and interaction roles.
- `epochMillis(time.Time) int64` and RFC3339 conversion for API output.
- `TaskRunStart`, `TaskRunEvent`, and `TaskRunPR` inputs.
- `ensureTaskRunForClaw(clawID string, opts TaskRunStart) (runID string, attemptID string, error)` that reads claw metadata/tags, creates a run/attempt if missing, updates `claws.task_run_id`, and inserts a `task_start` or `claw_created` event.
- `recordTaskRunEvent(input TaskRunEvent) error` with deterministic idempotency via `(tenant_id, run_id, event_key)`.
- `associateTaskRunPR(input TaskRunPR) error` that upserts `task_run_prs`, emits `pr_associated`/`pr_opened`, and materializes.
- `materializeTaskRun(runID string) error` that rebuilds the summary from retained events and PR rows.
- bounded JSON detail sanitization with `schemaVersion` and maximum persisted size.

Run: `go test ./pkg/hub -run 'TestTaskRun(Materialization|Classification|Idempotency)' -count=1`
Expected: PASS.

- [ ] **Step 5: Run package tests**

Run: `go test ./pkg/hub -count=1`
Expected: PASS.

### Task 2: Lifecycle And PR Instrumentation

**Files:**
- Modify: `pkg/hub/factory_creator.go`
- Modify: `pkg/hub/workflow_creator.go`
- Modify: `pkg/hub/github_webhook.go`
- Modify: `pkg/hub/external_webhook.go`
- Modify: `pkg/hub/factory_trigger.go`
- Modify: `pkg/hub/factory_triggers.go`
- Modify: `pkg/hub/linear.go`
- Modify: `pkg/hub/shortcut.go`
- Modify: `pkg/hub/github_issues.go`
- Modify: `pkg/hub/integration_poller.go`
- Modify: `pkg/hub/pr_watcher.go`
- Modify: `pkg/hub/pipeline_runner.go`
- Modify: `pkg/hub/task_run_analytics_test.go`

- [ ] **Step 1: Write failing instrumentation tests**

Add tests that drive existing central functions where practical:
- manual factory trigger creates one `task_runs` row, one attempt, a `task_start`/`claw_created` event, and links `claws.task_run_id`.
- manual workflow trigger creates a workflow-owned run with workspace/workflow names from tags.
- PR association through `storePRMention` creates `task_run_prs` and `pr_opened`.
- `trackPRMerged`/`checkPRMerged` creates `pr_merged` and terminal success.
- `trackPRClosed` creates `pr_closed_unmerged` and terminal failure when no merged PR exists.
- dashboard/user message after start records `human_dashboard_message` warning; hub/system injected messages do not.
- manual stop/agent stop records structured terminal failure.

Run targeted tests with `go test ./pkg/hub -run 'TestTaskRun.*Instrumentation|Test.*TaskRun' -count=1`
Expected: FAIL before instrumentation.

- [ ] **Step 2: Link run creation at central claw creation points**

After successful claw insert in each creation helper, call `ensureTaskRunForClaw` with:
- factory owner metadata for factory-created claws.
- workflow owner metadata for workflow-created claws.
- `run_kind='pr_task'` for GitHub PR factories and `code_task` otherwise.
- `analytics_enabled=true`, `requires_pr=true` for PR-producing known paths.
- excluded metadata for non-PR or unknown workflows where the contract is absent.

**Error handling strategy:**
- Creation-time analytics writes share the claw-creation transaction boundary, block the new claw if analytics row creation fails, and return/surface failures instead of silently ignoring them.
- Follow-up analytics writes from PR polling, dashboard events, and terminal observations log failures with enough identifiers for replay without blocking the external workflow.
- Validation errors fail fast; transient DB errors are candidates for a future async backfill/retry worker.

- [ ] **Step 3: Emit terminal and PR events**

Instrument:
- `storePRMention` -> `associateTaskRunPR`.
- PR merge/close path -> `pr_merged` or `pr_closed_unmerged`.
- `[DONE]` with no PR -> `done_without_pr`.
- `stopAgentWithReason` and existing timeout/failure paths -> structured failure events.
- manual stop before delivery -> `manual_stop_before_delivery`.

- [ ] **Step 4: Emit warning human interactions**

Instrument:
- PR comments, review comments, requested changes, dismissed/commented review, and manual code pushes detected by PR watcher/webhook.
- dashboard-originated human messages after initial start.
- manual resume/retry/stop/status/settings changes affecting a running task.

Preserve bot/system classification for hub-injected CI, bugbot, Greptile, and operational messages.

- [ ] **Step 5: Run package tests**

Run: `go test ./pkg/hub -count=1`
Expected: PASS.

### Task 3: Task-Run Analytics APIs

**Files:**
- Modify: `pkg/hub/server.go`
- Modify: `pkg/hub/task_run_analytics.go`
- Modify: `pkg/hub/task_run_analytics_test.go`

- [ ] **Step 1: Write failing API tests**

Using `httptest` and `NewTestServerWithConfig`, seed task-run rows through helpers and test:
- `GET /api/analytics/summary`
- `GET /api/analytics/runs`
- `GET /api/analytics/runs/:id/attempts`
- `GET /api/analytics/runs/:id/events`
- `GET /api/analytics/runs/:id/prs`
- `GET /api/analytics/filter-options`

Cover filters: global/workspace/owner, status, model, warning type, failure type, integration, repo, actor, run kind, phase, issue, PR, claw, q, from/to.

Cover cursor behavior:
- default order `started_at DESC, run_id DESC`.
- `limit` default 50 and max 200.
- stale/different filter hash returns `400` with `code='invalid_cursor'`.

Run: `go test ./pkg/hub -run 'TestTaskRunAnalyticsAPI' -count=1`
Expected: FAIL because routes do not exist.

- [ ] **Step 2: Register routes**

In `registerRoutes`, add exact handlers before/alongside legacy analytics:
- `/api/analytics/summary`
- `/api/analytics/runs`
- `/api/analytics/runs/`
- `/api/analytics/filter-options`

Keep `/api/analytics/factories` unchanged.

- [ ] **Step 3: Implement API queries**

Implement query parsing, filter SQL, coverage metadata, summary children, breakdowns, runs pagination, attempts/events/PR detail, and filter options.

Follow the Cross-Cutting Considerations security guidance while implementing query builders: scope by tenant first, whitelist filterable/sortable fields, and never interpolate arbitrary request parameters into SQL.

All API times must be RFC3339 strings generated from epoch ms. Do not compare RFC3339 strings against SQLite datetime columns for new tables.

Run: `go test ./pkg/hub -run 'TestTaskRunAnalyticsAPI' -count=1`
Expected: PASS.

- [ ] **Step 4: Run package tests**

Run: `go test ./pkg/hub -count=1`
Expected: PASS.

### Task 4: Scope-Aware Analytics UI

**Files:**
- Modify: `web/lib/api.ts`
- Modify: `web/lib/types.ts`
- Modify: `web/components/sidebar.tsx`
- Create: `web/components/task-run-analytics-view.tsx`
- Modify: `web/app/page.tsx`

- [ ] **Step 1: Add typed analytics API client**

Add TypeScript types for summary, coverage, run rows, attempts, events, PRs, filter options, filters, and pagination. Add helpers that build `URLSearchParams` and call the new endpoints.

Run: `cd web && npm run lint`
Expected initially may fail if helpers are unused; keep changes minimal until UI uses them.

- [ ] **Step 2: Add sidebar analytics navigation**

Extend `Sidebar` with:
- `activeView` prop for `agents` versus `analytics`.
- `onSelectAnalytics` callback.
- "Task Run Analytics" button with `BarChart3` icon.
- Conditional active styling based on `activeView`.

In `web/app/page.tsx`, add `activeView` state, switch the main content between `ConversationView` and `TaskRunAnalyticsView`, clear selected claw when opening analytics, and collapse the sidebar on mobile.

- [ ] **Step 3: Implement full-width analytics view**

Create `TaskRunAnalyticsView` as the full-width main content area. Do not nest cards inside cards. Use a dense operational layout:
- toolbar with date/status/model/warning/failure/integration/repo/runKind/phase/actor/search filters.
- metric strip showing numerator/denominator for rates.
- warning/failure breakdown pills.
- run table/list with PR context.
- run detail panel/timeline when a run is selected.

- [ ] **Step 4: Implement required states**

MVP states that must ship: no-runs, no-matching-filters, loading, API error, null rates, long-name handling, and timeline unavailable without layout overlap.

Nice-to-have states that can follow after the first delivery: partial history, stale materialization, rebuild/error, late-event grace, and incomplete coverage.

- [ ] **Step 5: Run frontend verification**

Run:
- `cd web && npm run lint`
- `cd web && npm run build`

Expected: PASS.

### Cross-Cutting Considerations

**Performance And Scalability**
- Keep analytics list endpoints paginated and backed by indexed filter columns.
- Materialize summaries during event writes so dashboard reads avoid expensive joins over raw event history.
- Preserve bounded UI fetches for details and only load attempts/events/PRs for the selected run.

**Security**
- Scope every analytics API query by tenant before applying user-controlled filters.
- Whitelist sortable/filterable fields in API query builders; do not interpolate arbitrary request parameters into SQL.
- Treat PR URLs, actor names, repo names, and model names as display data only in the UI.

**Observability**
- Log materialization failures with run, attempt, and event identifiers.
- Emit counters for analytics write failures, stale summaries, and analytics navigation/API usage.
- Track API latency and result counts for summary, list, and detail endpoints separately.

### Task 5: Final Verification And CodeRabbit Gate

**Files:** all changed files.

- [ ] **Step 1: Full backend verification**

Run: `go test ./...`
Expected: PASS.

- [ ] **Step 2: Full frontend verification**

Run:
- `cd web && npm run lint`
- `cd web && npm run build`

Expected: PASS.

- [ ] **Step 3: CodeRabbit review gate**

Run: `cr --base main`
Expected: no unaddressed findings, verified by rerunning until the CLI returns no findings. If any findings are returned, implement fixes and rerun the command; this implementation branch is not complete while CodeRabbit still reports findings.

- [ ] **Step 4: Final diff review**

Run:
- `git diff --check`
- `git status --short`
- `git diff --stat`

Expected: no whitespace errors and only intended files changed.
