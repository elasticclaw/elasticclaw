# MEMORY.md - Long-Term Memory

## Project: elasticclaw

Open source tool for provisioning ephemeral AI agent VMs. Apache 2.0.
Repo: github.com/elasticclaw/elasticclaw

### Architecture summary

- **Hub** (`pkg/hub/server.go`) — Go HTTP/WS server, SQLite backing, manages claw lifecycle
- **claw-bridge** (`cmd/claw-bridge/`) — runs on each VM, persistent WS → hub, proxies gateway
- **CLI** (`cmd/`) — login, list, create, chat, kill, template, hub, install, profile, upgrade
- **Web UI** (`web/`) — Next.js static export, embedded in Go binary via `//go:embed all:out`
- **Providers** — `pkg/provider/replicated`, `daytona`, `vercel`, `local`
- **Relay** (`relay` repo) — optional TURN-style WS proxy for NAT traversal

### Single binary architecture
- `make build` = Go only, no web (fast dev)
- `make build-release` = npm build → copy to `internal/webui/out/` → go build with `-tags embedweb`
- `//go:embed all:out` required (not `//go:embed out`) to include `_next/` directory
- `http.FileServer` causes 301 redirects — use `fs.ReadFile` directly
- No Next.js middleware, no cookies — auth via sessionStorage `ec_hub_token`

### Auth model
- Login: `POST /api/auth/login` with `{password}` → returns `{hubToken}`
- Browser stores `ec_hub_token` in sessionStorage
- All hub calls: `Authorization: Bearer <hubToken>`
- Hub validates against `hubCfg.Token`
- `ui_password:` in hub.yaml sets the login password

### Key decisions
- Single-tenant OSS only (no SaaS)
- SQLite backing
- bridge_image in hub.yaml: set → use as-is OCI ref; unset + tagged → GitHub releases; unset + dev → fail
- Relay is explicit opt-in via `relay_url` / `relay_secret`
- Credential helper calls hub AFTER bridge starts (hub API reachable via bridge proxy)
- Config: `/etc/elasticclaw/hub.yaml` (system) or `~/.elasticclaw/hub.yaml` (dev)
- Data: `/var/lib/elasticclaw/` (system) or `~/.elasticclaw/` (dev)

---

## Factories — How They Work

Factories auto-spawn and terminate claws based on external events.

### Linear factory

**Config in hub.yaml:**
```yaml
integrations:
  linear:
    - workspace: my-company       # label matching factory.workspace
      api_key: lin_api_...        # Linear API key (moves issues, reads state)
      # webhook_secret is per-factory now, not here

factories:
  - name: feature-factory
    integration: linear
    workspace: my-company
    team: ELA                     # Linear team key (optional, all teams if omitted)
    trigger_status: "Ready for Agent"
    done_status: "In Review"
    terminate_on_leave: true      # kill claw if issue leaves trigger_status
    template: base                # template name (must be pushed to hub)
    webhook_secret: whsec_...     # HMAC-SHA256 signing secret from Linear webhook
    tags:                         # applied to created claws (factory:<name> always added)
      - linear
      - feature
    color: teal                   # color for claw card
    name_pattern: "{issue_id}"    # claw name pattern (optional)
```

**Webhook URL:** `https://<hub-domain>/api/integrations/linear/webhook`

**Flow:**
1. Linear sends webhook on issue status change
2. Hub validates HMAC-SHA256 signature using factory's `webhook_secret`
3. `processLinearEvent()` checks each factory for matching workspace/team
4. Issue entering `trigger_status` → `createClawForIssue()`:
   - Enforces 1:1 (no duplicate claws per issue)
   - Resolves template files, injects `CONTEXT.md` with issue details
   - Inserts claw record with `linear_issue_id`, `tags`, `color`
   - Provisions VM async (Replicated CMX)
   - Broadcasts `claw_status: provisioning` over WS (dashboard appears immediately)
5. Issue leaving `trigger_status` → `terminateClawForIssue()`:
   - Queries DB by `linear_issue_id` (not by previousStatus — Linear omits it sometimes)
   - Closes WS, marks `status='deleted'`
   - Broadcasts `claw_status: deleted` over WS (card vanishes immediately)
   - If VM still provisioning: goroutine checks status='deleted' and calls `DeleteVM()`
6. Claw sends `[DONE]` message → `handleClawDoneSignal()`:
   - Moves issue to `done_status` via Linear GraphQL
   - Terminates claw

**CONTEXT.md injected:**
```markdown
# Linear Issue Context

**Title:** Fix login redirect bug
**ID:** ENG-123
**Status:** In Progress
**URL:** https://linear.app/.../ENG-123

## Description
...
```

### Race condition fixes (important)
- Poller (`pollProviderStatus`) excludes `status='deleted'` — won't re-bootstrap deleted claws
- `bootstrapReplicated` checks status at entry; if deleted, calls `DeleteVM` and returns
- DB status UPDATEs use `AND status != 'deleted'` guard
- `terminateClawForIssue` queries DB directly (not previousStatus from webhook payload)

### Factory activity log
- Table: `factory_events` (auto-pruned >4h)
- Actions: `claw_created`, `claw_terminated`, `error`, `not_actionable`
- API: `GET /api/factories/:name/events`
- UI: Settings → Factories → Activity link per factory
- `not_actionable` events still logged (for debugging wrong status names, etc.)

### Done signal
Agent sends `[DONE] <pr-url> [<pr-url2> ...]` as a chat message → hub validates PRs are open via GitHub API → stores in `claw_prs` table → moves Linear issue → terminates claw.

Format:
```
[DONE] https://github.com/org/repo/pull/123
```
Multiple repos:
```
[DONE] https://github.com/org/repo/pull/123 https://github.com/org/other/pull/45
```
If no valid open PRs are provided, the hub rejects the signal and injects an error message so the claw can retry.

---

## Settings UI

Settings page at `/settings` with these sections:
- **Sandbox Runtimes** — Replicated CMX token, Daytona URL/key/snapshot
- **LLM Keys** — Anthropic, OpenAI, etc. (injected as env vars at bootstrap)
- **GitHub Apps** — SSH public keys, App config
- **Security** — UI password change
- **Integrations** — Linear workspaces (label + API token)
- **Factories** — webhook URL display + copy, factory list with Edit/Activity/Remove, new factory form

Settings API: `GET/PATCH /api/settings` — reads/writes hub.yaml live (restarts in-memory config).

---

## Key Bugs Fixed (don't repeat)

- `{ }` bash group command in bootstrap → use `( )` subshell instead
- `sshRun()` was using `CombinedOutput(script)` which ran via `/bin/sh` (dash on Ubuntu) — fixed to pipe via stdin to `/bin/bash`
- Optimistic user messages (`opt-*`) filtered from localStorage — fixed by replacing with real UUID after API returns
- `loadMessages()` not called on refresh if claw already selected — fixed with mount effect
- `linear_issue_id` column missing from fresh DB `CREATE TABLE` (was ALTER TABLE only)
- Poller resurrecting deleted claws — exclude `deleted` from poller query
- LIKE `%factory:NAME%` matches prefixes — use `linear_issue_id` directly instead

---

## Known Gotchas

- `tsconfig.tsbuildinfo` causes merge conflicts — resolve with `--theirs`
- CI: Depot only, NOT GitHub Actions. Workflows in `.depot/workflows/`
- `release.yaml` triggers on tag push only — Depot doesn't support `release` event
- SilenceUsage: true on cobra root (prevents help wall on errors)
- `make build` does NOT embed web UI — use `make build-release` for production binary
- Bootstrap heredocs must NOT be PROFEOF-style when piped via stdin — use `printf`
- Replicated VM SSH proxy rejects arbitrary bash commands

---

## Open / Planned Work

### Factory improvements needed
- Webhook signature validation: currently validates against integration-level `webhook_secret`; recently moved to factory-level. Ensure linear.go validates per-factory not per-integration.
- Factory UI should show "currently active claws" count per factory
- Support multiple factories per Linear workspace (already works, just needs testing)
- Daytona provider support for factory-created claws
- GitHub Issues factory (next integration after Linear)
- Cron factory (time-based spawning)

### General
- `elasticclaw hub upgrade` — upgrades remote hub binary via SSH (done, in feat/upgrade-command)
- `elasticclaw upgrade` — upgrades local CLI binary (done)
- Drag-and-drop claw reordering — done in PR #26 (feat/claw-drag-reorder)
  - Sidebar list: `@dnd-kit/sortable` + `SortableClawCard`, 6px activation threshold
  - Board cards: horizontal DnD via `SortableClawBoardCard`, grip handle in card header
  - Order persisted to `localStorage` (`elasticclaw_claw_order`), re-applied on every `mergeClaws()` poll
  - Sidebar and board share the same order — `reorderClaws()` in `useHub`, wired through `page.tsx`
- Template registry at `github.com/elasticclaw/elasticclaw-templates`
- Relay: test with Caddy directly on VPS (Cloudflare idle timeout blocks WS)
- Bootstrap test that executes via stdin (catches bash-specific syntax issues)
