# Phase 3 — Frontend and hardening

**Estimated duration:** 2–3 weeks · **Risk:** low/medium · **Dependencies:**
Phase 1.1 (generated types) for item 3.2; other items independent

Goal: pay down the frontend debt (god component, zero tests), close the remaining
security items (secrets at rest, JWT), and consolidate hub configuration.

---

## 3.1 Split the settings panel

**Problem:** `web/app/settings/[[...parts]]/settings-content.tsx` is 4,891 lines
mixing state, API calls, and the rendering of every admin section; the /settings
bundle loads everything regardless of which tab is open.

**Change:**

1. The route is already a catch-all (`[[...parts]]`) — use the segments for
   code-splitting: one component per section in `web/app/settings/sections/`:
   `workspaces.tsx`, `workflows.tsx`, `github.tsx`, `llm-keys.tsx`, `mcp.tsx`,
   `secrets.tsx`, `analytics.tsx`, `ai-config.tsx` (adjust to the actual tab split).
2. `settings-content.tsx` becomes a shell (~200 LOC): navigation +
   `React.lazy`/`dynamic` per section.
3. Each section's state and API calls move down into colocated hooks
   (`useWorkflowSettings`, `useLLMKeys`, …) — no new global context.
4. Target: no new file >500 LOC; the shell + sections combined may keep the ~4.9k
   lines (this is a split, not a rewrite), but each unit is testable.

**Acceptance:** `npm run build` shows separate chunks per section; zero behavior
diff (manual validation guided by a checklist of the tabs).

## 3.2 Generated types in the UI + use-hub cleanup

1. Migrate `web/lib/types.ts` to derive from `web/lib/gen/api.d.ts` (Phase 1.1);
   delete the hand-written `Api*` interfaces.
2. Extract from `web/hooks/use-hub.ts` (636 LOC):
   - `useWebSocket` — connection, exponential backoff, URL redaction in logs
     (reusable by the terminal, which currently duplicates connection logic);
   - `useMessageCache` — localStorage persistence + durable/transient merge;
   - `useHub` remains as composition (~250 LOC target).
3. Magic constants (localStorage keys, 10s/60s polling intervals, 200-message
   limit) → `web/lib/constants.ts`.

**Acceptance:** identical behavior (same WS events, same cache); the terminal uses
the extracted `useWebSocket`.

## 3.3 Frontend tests

**Problem:** zero tests; refactors 3.1/3.2 have no safety net.
**Ordering note:** write the pure-function tests **before** 3.1/3.2 — they are the
refactor's safety net.

1. **Vitest** (unit): `lib/mappers.ts`, `lib/attachments.ts`,
   `lib/auth-storage.ts` (pure functions first), then
   `useMessageCache`/message-merge with `@testing-library/react` (the optimistic
   merge logic in `use-hub.ts:276-303` is the most valuable test case in the
   frontend).
2. **Playwright smoke** (1 spec): token login → claw list renders → open a
   conversation → open settings. Runs against the dev-compose hub in CI (new job
   in `.depot/workflows/main.yaml`, allowed to fail for 2 weeks until it
   stabilizes).
3. Scripts: `npm test`, `npm run test:e2e`; CI gate for the unit tests.

**Acceptance:** CI runs vitest; minimum coverage is not the goal — the goal is the
message-merge logic and mappers covered.

## 3.4 Secrets at rest

**Problem:** `hub.yaml` stores the GitHub App private key, Linear/Shortcut/Jira
tokens, and passwords in plaintext.

**Change:**

1. Encryption envelope: sensitive values written as
   `enc:v1:<base64(nonce+ciphertext)>` using AES-256-GCM. Master key via
   `ELASTICCLAW_MASTER_KEY` (32 bytes base64) or a `~/.elasticclaw/master.key`
   file created on first boot (0600).
2. Transparent migration: on load, a value without the `enc:` prefix is accepted
   and re-written encrypted on the next save. An
   `elasticclaw hub encrypt-secrets` command forces the full migration.
3. The installer (`pkg/install/scripts.go`) generates the master key and injects
   it into the systemd unit (`Environment=` or a 0600 `EnvironmentFile`).
4. Scope: fields marked sensitive on the config structs (`secret:"true"` tag), not
   the whole file — hub.yaml stays readable/editable.

**Acceptance:** a fresh install's hub.yaml contains no cleartext secret; upgrading
an existing install migrates without intervention; losing the master key has a
documented runbook (re-enter secrets).

## 3.5 Sessions with golang-jwt

**Problem:** `auth_github.go:32-77` implements a hand-rolled signed format (manual
HMAC-SHA256) even though `golang-jwt/jwt/v5` is already in go.mod.

**Change:** issue standard JWTs (HS256; claims `sub`, `exp` 7d, `iat`, custom
`login`/`avatar`), key derived from the 3.4 master key via HKDF (no longer the raw
`hubCfg.Token`). Accept the old format for 1 release (dual verification), then
remove it.

**Acceptance:** existing sessions don't get logged out on upgrade (transition
window); issuance/validation/expiry tests.

## 3.6 Config consolidation

**Problem:** two systems — Viper for the CLI, manual YAML for the hub — and
hub.yaml has no hot reload.

**Change:**

1. Define ownership: the **hub's** config leaves Viper entirely; a single loader
   in `pkg/config/hub.go` with typed structs + defaults + boot-time validation
   (Viper stays CLI-only, where it makes sense for flags/env).
2. Hot reload on SIGHUP (the server-standard pattern) for the safe subset:
   `allowed_origins`, branding, log level, integrations. Fields that require a
   restart (port, DB path) are documented and rejected on reload with a clear log.
3. Documented precedence: flag > env (`ELASTICCLAW_*`) > hub.yaml > default.

**Acceptance:** `kill -HUP` applies a log-level change without a restart; the
precedence matrix is covered by a test in `pkg/config/hub_test.go`.

## 3.7 Backend testability (review leftovers)

1. `pkg/provider/mock`: an in-memory implementation of the `Provider` interface
   (programmable states, injectable failures) for unit tests without
   Daytona/Docker.
2. Unit tests for the handlers extracted in Phase 2 using the mock + in-memory
   store.
3. Hot-path benchmark: `BenchmarkClawWSMessage` (decode → persist → broadcast) as
   a baseline for future optimizations.

**Acceptance:** `go test ./pkg/provider/mock/...` passes and at least 5 main
handlers have direct unit tests (create claw, send message, list claws, settings
get/put, Linear webhook).

---

## Suggested PR order

1. 3.3 vitest over pure functions (safety net first — before 3.1/3.2).
2. 3.2 hook extraction → 3.1 settings split (in that order: extracting hooks first
   shrinks the god component naturally).
3. 3.4 secrets → 3.5 JWT (3.5 depends on the 3.4 master key).
4. 3.6 config, 3.7 mock/unit tests, 3.3 Playwright — parallelizable.
