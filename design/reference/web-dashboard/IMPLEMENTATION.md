# Web dashboard redesign — implementation contract

Source of truth for the redesign of `web/` against the design kit in this directory.
Each phase ends with a validation gate; a phase is not done until its gate passes.
Deliberate divergences from the kit must be listed in this file under "Accepted departures".

## Approved decisions (owner: Ana)

- Backend scope: ALL of — message authorship, PR state on claw PRs, tickets endpoint,
  expose `issue_created_at`, CI event types, real context usage, structured pipeline logs.
- Requester metadata: Linear (creator/priority/description) + GitHub issue (author/body) fallback.
- Ticket aggregation: server-side (`/api/analytics/tickets`), paginated, same filters as runs.
- "Waiting on you": keep the existing client heuristic (`extractQuestion`); no server flag.
- Sidebar/board DnD: keep @dnd-kit, restricted to reorder WITHIN a derived section.
- Board card flip: REMOVED. Replaced by header actions (Open PRs popover, copy-with-confirm,
  Agent details sheet carrying purpose/status/terminal/kill).
- Charts: keep Recharts; move hardcoded hex series colors to `--chart-1..5` tokens; wrap in
  the new ChartCard anatomy (hairline header, body, mono stat line).
- Analytics + tickets stay admin-only.
- Kill dead code: `TaskRunAnalyticsView` (extract still-used exports first).
- Login: light restyle to kit aesthetics.
- Single PR, atomic commits per phase, draft, assignee AnaBerg, merge (never rebase/force-push).
- Filter bar dimensions (Factory/Workflow/Repo/Model) are MULTI-select (decided 2026-08-18,
  supersedes the kit's single-value selects): each dimension accepts several values (OR
  semantics), every selected value becomes its own removable chip, and the analytics API
  accepts comma-separated lists for those keys. Everything else follows the kit FilterBar:
  "Label: All" placeholders, raw values (no title-casing), Clear all right-aligned in the
  top row, the scope|dimension rule always visible.

## Phases

### Phase 0 — Backend data (Go hub)
0a. Migrations + writes:
  - `messages.user_login` TEXT NULL; POST /api/messages stores the resolved GitHub login
    (it is already resolved for the dashboard-message event); REST timeline + WS `message`
    frames return `user_login`.
  - `claw_prs`: add `state` (open/merged/closed), `merged`, `merged_at`; pr_watcher keeps
    them fresh; `/api/claws/:id/prs` returns them.
  - `task_run_events`: extend CHECK with `ci_succeeded`, `ci_failed`; pr_watcher emits them
    when `last_ci_conclusion` transitions.
  - Run view exposes `issueCreatedAt`.
  - `claw_status` (WS + /api/claws) carries real context usage when the agent reports it.
0b. Tickets API: `GET /api/analytics/tickets` — paginated, accepts the same filters as
  /api/analytics/runs. Per ticket: derived status (delivered | pr_open | in_progress |
  failed, rule in analytics-data.js `deriveTicketStatus`), issueId/title, requester meta,
  runIds + child run summaries, cost/tokens/humanTouches totals, PRs with state,
  reportedAt, timeToFirstRun, leadTime, lastActivity, business story (STORY_LABEL /
  STORY_KIND mapping in analytics-data.js, consecutive retries collapsed).
  Ticket metadata table filled from Linear (creator, priority, description→ask) with
  GitHub issue fallback (author, body).
0c. Structured logs: pipeline stages emit log records (ts, severity TRACE..FATAL with OTEL
  numbers 1/5/9/13/17/21, body, attrs); hub stores them per task_run_output and
  `/api/analytics/runs/:id/outputs` returns span metadata (span_id, kind, duration_ms,
  status, exit code) + records + raw stdout/stderr.

GATE 0: Go tests green. Contract tests assert the JSON shapes against the fixtures in
`analytics-data.js` (ticket status derivation table-tested with those runs; story collapse;
PR state; user_login present in REST and WS; outputs records shape). `go build ./...`,
`go test ./...`.

### Phase 1 — Local design system (web/)
New components in the repo's Tailwind/shadcn idiom under `web/components/ds/`:
AgentStateChip (6 states only: RUNNING/READY/STARTING/IDLE/OFFLINE/ERROR) + `agentSection()`
single authority (attention/working/offline rule from README), DatePicker range
(presets + range calendar, range-middle band uses `--primary` wash) built on ui/calendar,
CardAction, KpiTile, ChartCard/Panel anatomy (hairline 12px header, body, mono 10px stat
line), TicketStatusBadge, RunStatusBadge (moved), SeverityChip/AttrChip, stable per-person
color (hash of login → chart token palette). Status/section color tokens added to
globals.css. A dev-only gallery route/flag renders every component in all states.

GATE 1: gallery screenshot vs kit cards; AgentStateChip shows exactly the 6 labels;
DatePicker range wash is `--primary`; `npm run build` (static export) green.

### Phase 2 — Sidebar + Board
Sidebar: three sections (Needs attention / Working / Offline) with sticky tinted headers +
live count, empty-section copy, pinned-first inside section, DnD within section only,
AgentStateChip in rows. Board: cards clustered under the same sections with tinted band
headers; rail color = streaming/provisioning/error else section color; new card header
(identity block; hairline-separated actions: OpenPrs popover w/ count — omit when none,
lists even a single PR; copy-with-green-check 1.4s; Agent details sheet with the old back
face content + kill + terminal); footer stat line; composer unchanged. Mobile (Sheet +
MobileTabBar) preserved.

GATE 2: seeded-hub visual check (recipe below) against `index.html` baseline; checklist:
section order/counters/empties, pin-inside-section, rail colors, aria-labels/titles on all
card actions, copy confirm, PR popover behavior, no flip anywhere.

### Phase 3 — Conversation
Strictly chronological timeline; turn card after the agent's first reply; MessageBubble
authorship: self (right, blue, "You", no avatar) / teammate (left, neutral secondary
surface, initials avatar + name in stable color, "teammate" caption) / agent (accent tint,
Bot avatar). "Me" resolved via /api/auth/me login vs message.user_login.

GATE 3: seeded hub with a teammate message fixture; verify the three classes, alignment,
stable colors, chronological order.

### Phase 4 — Analytics shell
FilterBar: scope zone (DatePicker + workspace) | rule | dimension zone (factory, workflow,
repo, model), active filters restated as removable chips + Clear all; presets folded into
DatePicker; from/to keep living in the URL. KPI strip: ONE measured grid (cols 6/3/1,
tile min 158px), group subheads, uppercase mono-value tiles with arrow-led delta (delta
color rule: cost→up bad; good→down bad; else up bad). All chart/table cards in the new
anatomy with data-derived stat lines. Bottom table = TicketsTable (from /api/analytics/
tickets): columns Status/Ticket/Requester/Runs/Cost/Lead time/Last activity, chevron
expands child run rows, pagination. Heatmap unchanged in behavior. Recharts series colors
via chart tokens.

GATE 4: KPI grid snaps 6/3/1 at three widths; zones + chips work; stat lines change when
fixture data changes; tickets table statuses match the derivation rule; screenshots vs
`analytics.html` baseline.

### Phase 5 — Drill-downs
TicketDetailPanel (business register; NO spans/tokens/exit codes text anywhere): header
(status badge, priority, via source, title, requester · team · reported, repo · workflow,
Review PR when open PRs); KPIs Lead time / Time to first run / Total cost / Human touches;
What was asked for; Where it stands; Delivery (PR rows w/ state); How it went (story rail);
Runs on this ticket (Try N rows → run panel, stacking above). RunDetailPanel new hierarchy
(badge+phase+failure/warning captions → issue 16px semibold → owner · workflow · workspace
→ mono runId; KPI tiles Duration/Cost/Tokens/Human touches — Human touches NOT error-toned;
Run section rows; Timing rows with proportional bars; PRs; Attempts; Events rail newest
first, cap 12). AgentLogsDialog: Actions via StepRow with attempt selector; Output = OTEL
(resource header with service.name/deployment.environment/trace_id, severity filter
ALL/DEBUG/INFO/WARN/ERROR, RAW toggle → verbatim stdout/stderr, SpanBlock per output with
stat line record counts).

GATE 5: run panel hierarchy + timing bars; ticket panel textual check (no technical
vocabulary); ticket→run stacking; severity filter maths (>= rank); RAW fallback.

### Phase 6 — Cleanup + finish
Delete TaskRunAnalyticsView dead code (keep FilterSelect/StatusBadge/DetailState/
urlFilterKeys wherever they now live), login restyle, admin gating intact, full
`go test ./...` + `npm run build` + lint. Final screenshot pass (all screens, dark,
1440×900) — these go in the PR body.

GATE 6: full-suite green + final visual pass + departures documented below.

## Verification recipes

- Seeded hub: throwaway `pkg/hub/zz_manual_verify_test.go` gated on MANUAL_VERIFY env,
  NewTestServerWithConfig + insertTaskRunAnalyticsAPIRun/apiRunFixture seeds mirroring
  the fixtures in data.js/analytics-data.js, ListenAndServe on localhost:8080 with CORS.
  UI password is `admin`. Seed task_run_usage/usage_daily too (cost endpoints read them).
  Claws only show `connected` with a live WS: fake bridges dial ws://localhost:8080/claw/ws
  and send {type:"register", payload:{claw_id, name, token, gateway_ready:true}} (register
  name overwrites DB name — pass the real one).
- Web: `npm --prefix web run dev` (proxies /api to :8080; use localhost, never 127.0.0.1).
- Screenshots: headless Chrome `--headless=new --remote-debugging-port=9222` + Node CDP
  script (set sessionStorage ec_hub_token before the real navigation; remove
  `nextjs-portal` overlay nodes before capture).

## Accepted departures

- `checkPRMerged` in `pr_watcher.go` deletes `claw_prs` rows when a PR merges or
  closes without merging, so `GET /api/claws/:id/prs` cannot expose terminal PR
  state. Phase 5's Delivery rows source terminal state from retained `task_run_prs`.
- Ticket panel narrative drops the requester's role (kit shows "requester and
  role"; port shows requester name only) — `tickets_api.go:396` notes neither
  Linear nor GitHub exposes a reliable requester-role field for us to surface,
  so this is a data-availability gap, not a rendering gap. Kept as-is.
- ACL-scoped ticket pagination materializes at most 5,000 viewable runs per
  request because tag permissions are evaluated outside SQL. Larger restricted
  viewer windows return a truncated response rather than being retained
  unboundedly.
- Heatmap grid anchors on a Monday with a partial last column (GitHub-style). The kit
  fixture anchors 363+mondayIndex(end) days back from the end day, which lands the first
  cell on the end's own weekday and drifts every row off its M-S label; diverged for
  correctness (2026-08-19).
- The CI-status optimization proposed in `5ac7a640` was reverted in `b87086db`: updating
	  the watermark outside the event transaction could permanently consume it when the event
	  write rolled back. The task-run lookup, conditional claim, and event write therefore stay
	  transactional; a cheap `claw_prs` pre-read skips BEGIN/ROLLBACK for already-settled polls.
