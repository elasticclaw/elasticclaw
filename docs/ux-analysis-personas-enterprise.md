# ElasticClaw UX Analysis — Personas & Enterprise Scaling

Based on a full sweep of the codebase (web UI, auth/RBAC, analytics/cost pipeline, admin surfaces). File references point at the current worktree.

---

## 1. Current state in one paragraph

The product has exactly two surfaces: the agent board (`/`, `web/components/conversation-view.tsx`) and a 4,780-line Settings SPA (`web/app/settings/[[...parts]]/settings-content.tsx`) that contains *everything else* — workflows, analytics, providers, models, auth, diagnostics. There is **no user entity, no roles beyond a YAML admin list, one tenant, and one shared omnipotent token**. Analytics (the richest screen in the product) lives *inside* the admin-gated Settings area, so non-admins have zero access to run history, success rates, or any outcome data. Cost data is excellent at the pipeline level (per run/session/model/workflow, provider-reported with pricing fallback) but has no per-user/team attribution and no budgets.

---

## 2. Persona-by-persona analysis

### 2.1 Developer — "uses the tool; home = agents; sees success/progress of agents/runs/tickets; must NOT see costs"

**What works today**
- The agent board is genuinely good as a home: live cards with status rail, bootstrap progress (Sandbox → Runtime → OpenClaw → Workspace → Connect), context-usage bar, inline chat, terminal, PR links on the card back.
- No cost appears on the board, cards, or chat — the "no costs" requirement is accidentally satisfied because cost only exists inside Analytics.

**What's broken for this persona**
1. **No visibility into outcomes at all.** Run history, success/failure, ticket status, the run detail drawer with agent logs — all of it lives in `/settings/{ws}/workspace-analytics`, which is admin-gated. A developer literally cannot answer "did my agent's run succeed last night?" A terminated agent vanishes from the board and its history is unreachable.
2. **No "my work" scoping.** `trigger_actor_json` records who triggered a run (`pkg/hub/db.go:259`) but is never joined into analytics or the UI. There is no "my agents / my runs" filter; tag-based ACL (`{user}` in `view_requires_tags`) is inert unless hand-configured, and password users bypass it entirely.
3. **The cost restriction is unenforceable.** Cost gating today = hiding the entire Settings gear. There is no `costs:read` permission; if you granted a developer analytics access, they'd see every dollar figure (runs table Cost column, 6 cost charts, run drawer Usage section).
4. Empty state tells users to use the CLI while a "Create Agent" button exists in the sidebar — contradictory onboarding.

**Target experience**
- Home `/agents` (current board) + a new **`/runs` screen**: run list + detail drawer (status, phase, attempts, events, agent logs, PRs, ticket link) — *without* the Usage/cost block.
- "My runs" default filter via persisted trigger-actor identity.
- A delivery-focused mini-dashboard (success rate, runs per ticket, funnel) with **zero cost columns** — this requires splitting analytics into "Delivery" vs "Cost" views (the API already separates `/api/analytics/summary|effectiveness` from `/api/analytics/costs`; the split is nearly free on the backend).

### 2.2 Tech lead — "configures workflows, sees agents + costs; home = analytics"

**What works today**
- The analytics command center is the strongest screen in the product: 9 KPIs, 11 charts, cost per merged PR, cost drivers, most-expensive tickets, period-over-period deltas.
- Cost pipeline granularity (per run/session/model/workflow/ticket) supports everything a lead needs.

**What's broken for this persona**
1. **There is no role for them.** The model is binary admin-or-not. To see analytics a lead must be a full admin — which also grants secrets, hub config, auth settings, and the "Reveal secrets" toggle. Massive over-provisioning.
2. **Analytics is buried and lossy.** Home for this persona is 3 clicks deep (gear → workspace nav → Analytics), and the `/analytics` redirect chain **silently drops query filters** (`settings-content.tsx:307`), so "Open full analytics for this workflow" never actually filters by workflow.
3. **Workflows are read-only.** The UI offers exactly two toggles (`enabled`, `enableManualTrigger`); authoring is YAML pushed via CLI. A "configures workflows" persona has no create/edit/delete, no stage editing, no trigger-rule editing. The AI-config chat rewrites the whole `hub.yaml` — too blunt.
4. **No team lens.** Cost/success by engineer or by team is impossible (no user dimension in `task_runs`/`usage_daily`).
5. **No budgets/alerts.** `projectedMonth` is computed but nothing consumes it; a lead can't set a monthly cap or get a spend alert.
6. Scheduled/cron `workflow_runs` never join analytics — a lead's picture of workflow health is incomplete.

**Target experience**
- Login lands on `/analytics` (full version, costs included). Promote analytics to a top-level route; kill the redirect stub.
- **Workflow editor**: structured form or validated YAML editor with diff+apply per workflow (not whole-hub rewrite), plus run history inline (already exists in `workflow-runs-dialog.tsx`).
- Role `maintainer`/`tech_lead`: workflows CRUD + analytics + costs + agents; no secrets, no auth config, no infra.
- Cost drivers grouped by team/user once attribution exists; budget config with alerting on `projectedMonth`.

### 2.3 Admin — "manages users, infra, sandbox, access tokens, models; home = analytics"

**What exists vs what the persona needs**

| Need | Today | Gap |
|---|---|---|
| Manage users | No users table. Admin = GitHub login string in YAML list; password login is hard-coded `is_admin: true` (`server.go:854`) | Entire user management product missing |
| Access tokens | None. One shared hub token, returned raw to every password login; no scopes/expiry/revocation | PAT/service-token product missing |
| Infra/sandbox | Provider config forms only (5 providers). **No fleet view**: nothing lists running sandboxes, age, provider, or lets you kill one | Sandbox lifecycle screen missing |
| Models | Provider keys + hardcoded model catalogs (stale on every release); no per-workspace allowed-models; no key validation test | Model governance missing |
| Analytics home | Same buried screen as everyone | Same fix as tech lead |

**Additional admin-surface defects (from the settings audit)**
- `/settings` has no route guard — non-admins get the full chrome and a bare "Failed to load" string; the gear icon flashes for everyone (`sidebar.tsx:112` defaults `isAdmin = true`).
- Destructive actions (provider/key/secret/MCP/GitHub App delete) fire with no confirmation.
- GitHub App private key is an unmasked `<textarea>`; AI-config "Reveal secrets" dumps the whole `hub.yaml` unmasked with no confirm/audit.
- MCP servers render under workspace-scoped URLs but patch global settings — editing under workspace A silently changes B.
- `concurrencyGroups`/`maxConcurrentClaws` fetched and typed but never rendered.
- ~600 lines of dead components (`TaskRunAnalyticsView`, `WebhooksSection`, `WorkspaceRepositoryList`, `WorkspaceAccessList`) — the dead analytics view has features the live one lost (clickable KPI drill-down, clear filters).

**Target experience**
- Home `/analytics` shared with tech lead, plus an **Admin area**: Users & roles, Tokens (create/scope/expire/revoke + last-used), Sandbox fleet (live VMs, cost, kill), Model governance (catalog sync, per-workspace allow-list, key test), Audit log.

---

## 3. Target permission model

Cost visibility must be a **permission, not a screen** — that's the core structural change the developer persona forces.

| Capability | developer | tech lead | admin |
|---|---|---|---|
| Agents board, chat, terminal, kill (own/team) | ✅ | ✅ | ✅ |
| Runs list + detail + logs (delivery view) | ✅ | ✅ | ✅ |
| Delivery analytics (success, funnel, throughput) | ✅ (team-scoped) | ✅ | ✅ |
| **Costs (any $ or token figure)** | ❌ | ✅ | ✅ |
| Workflows: view | ✅ | ✅ | ✅ |
| Workflows: create/edit/toggle | ❌ | ✅ | ✅ |
| Workspace config, secrets, integrations | ❌ | (optional, per-team) | ✅ |
| Users, roles, teams | ❌ | ❌ | ✅ |
| Tokens, sandbox fleet, models, auth, hub config | ❌ | ❌ | ✅ |

Default landing: developer → `/agents`; tech lead & admin → `/analytics`.

Implementation notes:
- Backend already splits cost endpoints (`/api/analytics/costs`, cost-drivers) from delivery endpoints — gate them with a `costs:read` scope and strip `estimated_cost_usd`/token fields from `/api/analytics/runs` and run detail for callers without it.
- Frontend: split the command center into Delivery and Cost tabs/sections rendered by permission; remove cost column from the runs table for developers.

## 4. Target information architecture

```
/agents        ← board + chat (dev home)
/runs          ← run history + detail drawer (all personas, cost-stripped for devs)
/analytics     ← top-level, tabs: Delivery | Cost (lead/admin home)
/workflows     ← list + editor + run history (lead+)
/settings      ← workspace config: integrations, secrets, MCP (admin, maybe lead)
/admin         ← users & roles, teams, tokens, sandbox fleet, models, audit log
```

Quick wins independent of RBAC: fix the analytics redirect filter-dropping bug; guard `/settings`; unify the three StatusBadge vocabularies; resurrect the dead analytics features (KPI drill-down, clear-filters); confirmation dialogs on destructive actions; align empty-state onboarding with the in-app Create Agent button.

---

## 5. Scaling to multiple teams / enterprise (WorkOS)

### 5.1 Prerequisite: identity foundation (no WorkOS needed yet)

Nothing enterprise works until these exist — WorkOS cannot compensate for them:

1. **`users` table** (id, email, external idp id, display name, status) + **server-side sessions** (revocable — today's HMAC blobs can't be invalidated, so IdP deprovisioning wouldn't bite for up to 7 days).
2. **`teams` table** + membership; add `team_id`/`user_id` dimensions to `task_runs` and `usage_daily` (propagate the already-captured `trigger_actor_json`). This unlocks per-team cost attribution, team-scoped agent views, and team budgets.
3. **Roles table** (admin / tech_lead / developer) replacing the YAML admin list; enforcement middleware replacing the binary `withWebAdminAuth`.
4. **Kill the shared-token-as-login pattern.** Password login returning `hubCfg.Token` (full admin, unrevocable, bypasses all ACLs) is the single biggest blocker: it defeats identity, audit, and cost gating simultaneously. Replace with per-user sessions; keep the hub token for machine-to-machine only, then replace it with scoped service tokens.
5. **Resource ownership**: workspaces/workflows get an owning team. Today they're global directories on disk with no owner field.

The schema is half-ready: every table already carries an indexed `tenant_id`, and handlers filter by it — but only one tenant row ever exists. Tenancy = organizations can be activated rather than retrofitted.

### 5.2 Where WorkOS fits

WorkOS solves exactly the layer ElasticClaw is missing, without building N enterprise integrations:

- **SSO (SAML + OIDC), one integration** → replaces the hardcoded GitHub-only OAuth (`auth_github.go` is GitHub-specific end-to-end; there's no provider abstraction). Enterprise customers connect Okta/Entra/Google via WorkOS; keep GitHub OAuth as one connection among many. Requires an `AuthProvider` interface where today there's a concrete `GitHubOAuthConfig` field.
- **Directory Sync (SCIM)** → users and groups provisioned/deprovisioned from the IdP. Map IdP groups → ElasticClaw teams and roles (e.g. `eng-platform` → team Platform, role developer). Deprovisioning must revoke sessions — hence the server-side session store above.
- **Organizations** → WorkOS orgs map 1:1 to tenant rows; connection-per-org gives per-customer SSO policy. For a single company running multi-team, one org + Directory-Sync groups is enough; multi-org matters if ElasticClaw is offered as a hosted product.
- **Audit Logs** → enterprise buyers require it and today there's nothing beyond `log.Printf`. Emit: login, run trigger, kill, workflow change, secret change, settings change, token create/revoke, secret reveal.
- **Admin Portal** → self-serve SSO/SCIM setup for customer IT, offloading the most painful support surface.
- **RBAC**: keep authorization *decisions* in ElasticClaw (roles/permissions on your own tables); optionally source role *assignments* from WorkOS roles or IdP groups. Don't outsource enforcement.

### 5.3 Multi-team UX consequences

- **Team scoping everywhere**: agent board and runs filter to "my teams" by default with an all-teams switch for leads/admins; workflows and workspaces listed per owning team.
- **Analytics adds a team dimension**: cost by team, success by team, team budgets with alerts (the `projectedMonth` computation finally gets a consumer); cost-drivers `groupBy` extended from `factory|workflow` to `team|user|workspace|model` (usage_daily already supports workspace/model).
- **Concurrency & quota per team**: `concurrencyGroups` exists in config; surface it per team (fleet fairness is a real multi-team pain).
- **Model governance per team/workspace**: allowed models list, so a lead can cap a team to cheaper models — this also makes the hardcoded model catalog untenable; sync it from providers.

### 5.4 Suggested sequencing

1. **Phase 1 — UX quick wins** (no schema): promote `/analytics` and `/runs` to top level, fix redirect/filter bug, settings route guard, split Delivery/Cost views behind a temporary flag.
2. **Phase 2 — Identity core**: users + sessions + roles tables, per-user login, deprecate shared-token login, `costs:read` enforcement server-side. Personas become real here.
3. **Phase 3 — Teams & attribution**: teams, resource ownership, user/team dimensions in analytics, team-scoped views, budgets.
4. **Phase 4 — Enterprise**: WorkOS SSO + Directory Sync + Audit Logs, scoped API tokens, admin area (users/tokens/fleet/models/audit), org-level tenancy activation.

---

## 6. Security items found in passing (worth fixing regardless of personas)

- Default UI password is literally `"admin"`; `disable_password_auth` off by default; empty GitHub allowlist admits any GitHub account.
- `Access-Control-Allow-Origin: *` on every route; tokens accepted via `?token=` query param (lands in logs); session secret defaults to the hub token (rotating one invalidates the other).
- Tokens in `sessionStorage` + open `BroadcastChannel` are XSS-reachable.
- Tag ACL fails open and is skipped for password users by construction (`if ghLogin != ""` short-circuits).
