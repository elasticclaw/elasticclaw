# Phase 1 — API contract and observability

**Risk:** low/medium · **Dependencies:** Phase 0 (error envelope)

Goal: turn the Go↔TypeScript contract from "mirrored by hand" into one generated
from a single source, and give the hub structured logging, traces, and metrics.
These two workstreams unblock the Phase 2 reorganization (refactoring with
telemetry and a formal contract is much safer).

---

## 1.1 OpenAPI as the single source of the contract

**Problem:** ~90 endpoints registered by hand in `pkg/hub/server.go`
(`registerRoutes`), TS types mirrored manually in `web/lib/types.ts`. Drift already
exists: `InstanceStatus` in Go includes `starting` and `deleted`; the `ClawStatus`
union in TS does not.

**Decision:** spec-first with `openapi.yaml` under `api/`, codegen for both sides.

- **Go:** `oapi-codegen` generating `api/gen/` — request/response types and server
  interfaces (`std-http` mode, compatible with the current ServeMux). Existing
  handlers adopt the generated interfaces gradually.
- **TS:** `openapi-typescript` generating `web/lib/gen/api.d.ts`. `web/lib/types.ts`
  re-exports/derives from the generated types; `web/lib/mappers.ts` stays as the
  presentation layer (snake_case → camelCase).

**Incremental scope (do not spec all 90 endpoints at once):**

1. **Batch 1 — UI endpoints:** `/api/claws*`, `/api/messages*`, `/api/auth/*`,
   `/api/settings*`, `/api/analytics/*`, `/api/templates*`. These have a TS
   consumer and are where drift hurts.
2. **Batch 2 — integration webhooks** (`/api/integrations/*`): payload schemas and
   response codes only (the consumer is external).
3. **Batch 3 — internal claw↔hub endpoints** (checkpoints, volumes, files):
   schemas in a separate `api/internal.yaml` spec (not public API).

**Build:** `make gen` targets (runs both codegens) and `make gen-check` (generates
and fails if `git diff` is non-empty) — `gen-check` goes into CI
(`.depot/workflows/main.yaml`).

**Acceptance:**
- Green `make gen-check` in CI.
- `web/lib/types.ts` no longer hand-defines any batch-1 mirrored type.
- Browsable API docs (static Redoc/Scalar generated from the yaml, served at
  `/api/docs` in dev mode only).

## 1.2 Fix the status drift in the UI

**Problem:** the UI handles neither `starting` nor `deleted`.

**Change:** with the generated types from 1.1, the compiler flags the
non-exhaustive switches. Define presentation: `starting` renders like provisioning
(spinner); `deleted` filters the claw out of the list. Add an exhaustiveness helper
(`assertNever(status)`) so new states break the build instead of silently
disappearing.

**Acceptance:** a mapper test covering every value of the generated enum.

## 1.3 Structured logging + request ID

**Problem:** ~200 `log.Printf` calls with manual `[component]` tags; impossible to
correlate lines from the same request; `fmt.Println` surviving in
`external_storage.go:685,714`.

**Change:**

1. Adopt `log/slog` with a JSON handler (text in dev, controlled by
   `ELASTICCLAW_LOG_FORMAT`). Root logger created at boot, injected into `Server`.
2. `withRequestID` middleware: generate a short ID, store it in the context, add
   `X-Request-ID` to the response. Helper `logger.FromContext(ctx)` returns a
   logger with `request_id` + `tenant_id` already attached.
3. Mechanical migration: `log.Printf("[claw] ...")` → `slog` with `component=claw`
   as an attribute. Do it file by file, without changing messages (so nothing that
   parses them breaks).
4. Forbid new `fmt.Println`/`log.Printf` via lint (`golangci-lint`: `forbidigo`).

**Acceptance:** all log lines for a claw-creation request carry the same
`request_id`; `golangci-lint` fails on a new `log.Printf` in `pkg/hub`.

## 1.4 Versioned migrations

**Problem:** migrations via idempotent `ALTER TABLE` with no versioning (Phase 0.5
only stopped swallowing errors).

**Change:**

1. Adopt numbered, embedded migrations (`pkg/hub/store/migrations/0001_init.sql`, …)
   with a `schema_migrations` table. Prefer a minimal in-house implementation
   (~80 LOC, `//go:embed`) over pulling in all of `golang-migrate` — evaluate in
   the first PR; if the in-house one exceeds ~150 LOC, use `golang-migrate` with
   the sqlite driver.
2. `0001_init.sql` = the current consolidated schema (today's
   `CREATE TABLE IF NOT EXISTS` + all the ALTERs collapsed). Existing-install
   detection: if the tables already exist and `schema_migrations` does not,
   register the baseline as version 1 without executing it.
3. Migrations run at boot inside a transaction (SQLite supports transactional DDL);
   failure aborts boot.

**Acceptance:** upgrading a real `hub.db` from a previous version works (test with
a committed old-DB fixture); clean install likewise; downgrade unsupported —
documented.

## 1.5 Metrics and traces

**Problem:** OTel is already in go.mod (transitively via the Daytona SDK) but
unused; no metrics exported.

**Change (minimum useful, not big-bang):**

1. Prometheus `/metrics` endpoint (use `prometheus/client_golang`): request
   counters by route/status, latency histogram (via middleware), gauges for claws
   by status, WS messages in/out, webhook errors per integration, SQLite pool size.
2. Optional OTel traces (`ELASTICCLAW_OTLP_ENDPOINT`): a span per HTTP request
   (`otelhttp` middleware) and manual spans around provider operations
   (`Create`/`Exec`/`Destroy`) — those are the slow operations that matter.
3. No OTLP endpoint configured → no-op provider, zero cost.

**Acceptance:** `curl /metrics` shows the series; a claw-creation trace is visible
in a local collector (the dev compose gains an optional, commented-out
`otel-collector` service).

---

## Suggested PR order

1. 1.3 (slog + request ID) — benefits every subsequent PR.
2. 1.4 (migrations) — small and isolated.
3. 1.1 batch 1 + 1.2 (spec + codegen + status fix) — the biggest; can be split
   into spec/gen first, handler adoption after.
4. 1.5 (metrics/traces).
5. 1.1 batches 2 and 3 — can slip into Phase 2 without blocking anything.
