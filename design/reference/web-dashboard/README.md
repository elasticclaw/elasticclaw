# UI kit — ElasticClaw web dashboard

Recreation of the Next.js dashboard at `web/` in the [elasticclaw/elasticclaw](https://github.com/elasticclaw/elasticclaw) repo.

| File | Source it recreates |
|---|---|
| `index.html` | `web/app/page.tsx` → `web/components/home-shell.tsx` (interactive: filter, select, send, kill) |
| `login.html` | `web/app/login/page.tsx` |
| `Sidebar.jsx` | `web/components/sidebar.tsx` + `claw-card.tsx` (expanded and collapsed rails) |
| `Board.jsx` | `web/components/conversation-view.tsx` → `ClawBoardCard` |
| `Conversation.jsx` | `web/components/conversation-view.tsx` chat panel + `agent-timeline/*` |
| `analytics.html` + `Analytics.jsx` | `web/components/analytics-command-center.tsx` — shell, filter bar, both KPI groups |
| `AnalyticsCharts.jsx` | the same file's `OutcomesChart`, `TicketThroughputChart`, `DeliveryFunnel`, `RunsPerTicketChart`, `TopTicketsByCostChart`, `CostPerMergedPrChart`, `WorkflowCostComparisonChart`, `DailyCostChart` (Recharts replaced by SVG/CSS at the same shapes) |
| `RunDetail.jsx` | `web/components/task-run-analytics-view.tsx` — `RunDetailPanel` (scrim + right sheet) and `StatusBadge`, plus `web/components/run-logs-dialog.tsx` (Agent logs modal, Actions / Output tabs) |
| `AnalyticsTables.jsx` | the same file's `RunsTable`, `CostDrivers` and the trailing-year `Heatmap` ("Cost by day", 52×7) |
| `Login.jsx` | `web/app/login/page.tsx` `LoginForm` |
| `data.js` | shapes from `web/lib/types.ts` (`Claw`, `Message`, `AgentActivity`) |

## Deliberate departures from the source

- **KPI tiles** use an uppercase micro caption, a monospaced value and an arrow-led delta instead of the source's sentence-case label + semibold sans value. Machine numbers are monospaced everywhere else in this system; the KPI strip was the exception.
- **Filter bar** is split into a scope zone (period + workspace) and a dimension zone (factory / workflow / repo / model) with active filters restated as removable chips. The source puts all seven controls in one undifferentiated row.
- **Period** is a `DatePicker` (presets + range calendar in one popover) rather than four preset buttons, so the active range is always readable. Its range-middle band is a `--primary` wash, not `--accent` — see readme.md.
- **Run drill-down hierarchy** departs from the source's flat 2-column grid of eight equally-weighted bordered boxes, which gave a phase name the same weight as a duration. Here: state (badge + phase + failure/warning captions) → what the run was for (issue, 16px semibold) → who ran it (owner · workflow · workspace) → the opaque `runId` in mono; then the headline numbers as KPI tiles (Duration, Cost, Tokens, Human touches), then each group as a card with the analytics anatomy. Timing rows carry a proportional bar (the delivery-funnel treatment) and Events render as a rail-and-dot timeline, newest first.
- **Chart and table cards** are built to the agent board card's anatomy: hairline-separated 12px header with the title left and the affordance icon right, a body region, and a monospaced 10px stat line on the bottom edge (the board card's "4 steps · ctx 64%" row). The source uses a flat `p-4` section with the title inline. Every stat-line value is derived from the fixtures, not invented.
- **KPI strip** is one grid for all nine tiles (group names as full-width subheads) instead of the source's two grids inside a fixed 6fr/3fr pair. Two grids reflowed at different widths, so tile sizes and row counts disagreed. The column count is measured and snapped to 6 / 3 / 1 — the only counts that divide a 6-tile and a 3-tile group without leaving an orphan on its own row; free `auto-fit` lands on 4 or 5 and does exactly that.

## Deliberately not recreated

- **Task-run explorer page** (`web/components/task-run-analytics-view.tsx`'s own header: AI Spend cards, General Stats, the seven clickable Metric tiles and its wider 11-column table). The run **drill-down** from that file *is* recreated — `RunDetail.jsx`, reachable from any row of the Runs table.
- **Sidebar rows** show a STATE chip + detail + age instead of the source's dot-only lifecycle encoding; see the AgentRow note in the root readme.
- **Agent logs → Actions tab** embeds `ClawActivityLog` upstream; here the same tool activity renders through the design system's `StepRow`.
- **Agent logs → Output tab reads as OpenTelemetry.** Upstream prints a raw stdout/stderr blob per stage, which stops being readable past a few lines. Same data, OTEL shape: a resource header (`service.name`, `deployment.environment`, `trace_id`), one **span** per stage output (`span_id`, `span.kind`, duration, status OK/ERROR, exit code), and **log records** with a millisecond timestamp, a severity chip (TRACE/DEBUG/INFO/WARN/ERROR/FATAL carrying OTEL severity numbers 1/5/9/13/17/21 in its tooltip), a body, and typed attributes as `key=value` chips (numbers tinted). A severity filter narrows by `severity_number >=`, and **RAW** falls back to the verbatim stdout/stderr the source shows. Severity colors come from the chart tokens, so they match the rest of analytics.
- **Settings** (`web/app/settings/[[...parts]]/settings-content.tsx`, 220k) — same reason.
- **Terminal** (xterm.js) and the **board card flip** (3D `rotateY`) are represented by their affordances only.

## Interactions that work

Filter agents · select an agent from the sidebar (clears unread) · open a card's conversation · send a message on a card or in the chat panel · pin/unpin · kill an agent · collapse the sidebar · toggle timeline density · expand a failed step's output · switch to the analytics placeholder and back.

### Analytics: unique tickets, two drill-downs

The bottom table lists **unique tickets**, not runs. Each row expands (chevron) into the
runs that served that ticket, so the retry history is visible without leaving the page.

Ticket status is **derived** in `analytics-data.js` from the runs' PR states — never authored:

| Derived status | Rule |
| --- | --- |
| `delivered` | any PR on the ticket is merged |
| `pr_open` | PRs are open and none is merged or closed — work landed, review has not |
| `in_progress` | a run is still running and no PR exists yet |
| `failed` | every run failed and no PR was opened |

`pr_open` is the state the run-level view could not express, which is why the board
turns on it: PLT-31 sits there with #2848 awaiting review.

Two drill-downs, deliberately different in register:

- **Ticket → business** (`TicketDetail.jsx`) — lead time from report to merge, time to
  first run, total cost of ownership, human touches, what was asked for and by whom, PRs
  with their state, and the ticket's story as business milestones ("Shipped", "Human
  stepped in", "Blocked: no sandbox available"). No spans, no tokens, no exit codes.
- **Run → technical** (`RunDetail.jsx`) — OTEL spans, severity-filtered log records,
  attempts, token usage. Reachable from a child row or from the ticket panel's run list,
  where it stacks above the business panel.

### Agent state, everywhere

The lifecycle vocabulary lives in one component, `AgentStateChip` (`components/agents/`),
used by the sidebar row, where a list of
names needs it. The board card and the conversation header deliberately do NOT carry it:
there the rail colour, the status dot, the uptime/error line and the NowStrip already say
the state, and a chip was a fourth telling of the same thing. `RUNNING · READY · STARTING · IDLE · OFFLINE · ERROR`, and nothing else —
the NowStrip under it already says what the agent is waiting for, so the chip does not
repeat it (`reason` exists on the component for surfaces with no NowStrip). The board also clusters its cards under the same three section headers as the
sidebar, and each card carries its section colour on the rail — so "what needs me" reads
the same whether you are looking at the list or the cards.

### Sidebar sections

The agent list is grouped into three sections instead of Pinned / All Agents, so the
list answers "what do I have to look at" before "what exists". The bucket is derived
from `ClawStatus` plus the two "needs you" signals the product already tracks:

| Section | Rule |
| --- | --- |
| **Needs attention** | `error`, or `idle` with unread replies or a pending question |
| **Working** | `connected` / streaming / `provisioning` — running unattended |
| **Offline** | `offline`, or `idle` with nothing waiting on you |

Pins no longer own a section; a pinned agent sorts first *inside* its section, so a
pinned agent that breaks still surfaces under Needs attention. Section headers are
sticky with a live count, and an empty section states so rather than disappearing.

### Board card header

Rebuilt for reading order: the identity block (status dot, name, unread pill, then
`template · uptime`) is one unit, and the actions sit in a hairline-separated cluster on
the right so glyphs no longer compete with the name.

Every action states itself — `aria-label` + `title`, and a count on the ones carrying a
payload:

- **Open PRs** (`GitPullRequest` + count, in `--chart-1`) — quick access to the agent's
  open pull requests. Click lists them (repo#number + title) rather than jumping blind;
  even a single PR lists first.
- **Copy transcript** — confirms in place (glyph swaps to a green check for 1.4s) instead
  of silently succeeding.
- **Agent details** — the source's bot-info affordance, now labelled.

Open PRs come from `openPrs` on the agent fixture; agents with none simply omit the button.

### Who said what

Agent threads are shared rooms, so the transcript separates three authorship classes
(`MessageBubble`, `self` + `authorColor`):

| Author | Treatment |
| --- | --- |
| **You** (signed-in user) | blue `--bubble-user` tint, aligned to the right edge, no avatar |
| **A teammate** | neutral `--secondary` surface, initials avatar + name in that person's stable colour, "teammate" caption |
| **The agent** | the agent's accent tint plus a `Bot` avatar |

Alignment carries mine-vs-theirs; colour carries who. `window.messageAuthor(message, agent)`
resolves it once from `userId` against `EC_PEOPLE` — screens render the result, they never
decide authorship themselves. The conversation view is now strictly **chronological** (the
turn card sits after the agent's first reply) because a teammate's answer only reads
correctly next to what it answered.
