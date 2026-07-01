# Phase 2 — Reorganizing pkg/hub

**Risk:** medium · **Dependencies:** Phases 0 and 1 (shutdown, request ID, and
contract tests make the refactor observable and safe)

Goal: split the `pkg/hub` god package (103 files, ~54k LOC, `server.go` at 6,535
lines) into domain subpackages with explicit concurrency and data boundaries.
**Mechanical migration of existing code — not a rewrite.** The existing
`factorytest` suite + integration tests gate every move.

---

## 2.1 Target structure

```
pkg/hub/
├── httpserver/      # router, middleware chain, error envelope, HTTP helpers
├── claws/           # claw lifecycle, WS connections (/claw/ws), files, streaming
├── integrations/    # linear.go, github_*.go, jira.go, shortcut.go, external_webhook.go,
│                    # integration_poller.go — one subdirectory per tracker if it grows
├── workflows/       # pipeline_runner, factory_*, cron_*, pr_watcher
├── checkpoints/     # checkpoints, volumes, external_storage, artifacts
├── analytics/       # task_run_analytics*
├── settings/        # settings, ai_config, model_auth, doctor
├── store/           # repositories + migrations (see 2.4)
└── hub.go           # composition: wires store → services → httpserver; Run(ctx)
```

Dependency rules (enforced by `depguard` in golangci-lint):

- `httpserver` knows the services; services do **not** know `httpserver`.
- Services know `store` and `pkg/types`; `store` only knows `pkg/types`.
- `integrations`/`workflows` may call `claws` (create a claw from an event);
  `claws` knows neither of them.
- Cycles are forbidden; if a cycle seems necessary, the shared type moves down to
  `pkg/types` or an interface is born in the consumer.

## 2.2 Extraction roadmap (least-coupled first)

Each step is one PR; use `git mv` to preserve history; no behavior changes.

1. **`httpserver/`** — extract `registerRoutes`, middlewares, and response helpers
   from `server.go`. Handlers stay where they are; the router references them
   through interfaces.
2. **`analytics/`** — the most isolated (task_run_analytics* ~3.4k LOC + its own
   tests).
3. **`settings/`** — settings, ai_config, model_auth, doctor.
4. **`integrations/`** — webhooks + pollers; while moving, extract signature
   validation into a shared helper and **audit that every integration validates
   signatures** (review finding: not confirmed for all of them).
5. **`checkpoints/`** — checkpoints, volumes, external_storage.
6. **`workflows/`** — pipeline_runner, factory_*, cron, pr_watcher.
7. **`claws/`** — last (it's the core): handleClawWS, streaming, files, terminal.
8. **Residual `server.go` → `hub.go`** — composition and lifecycle only.

Quantitative target: no file >1,500 LOC at the end; `server.go`/`hub.go` <500 LOC.

## 2.3 Concurrency

**Problems:** a single `Server` mutex protecting claws, users, config, webhook
dedup, and the model cache; one goroutine per WS message with no bound
(`server.go:2546`); 84 `context.Background()` calls.

**Changes:**

1. **One mutex per subsystem**, moved together with its state during the 2.2
   extraction: `claws.registry` (its own RWMutex), `users.registry`,
   `settings.cache`, etc. No global lock remains in `hub.go`.
2. **Worker pool for WS messages**: replace the `go func(payload, conn)` with a
   pool using `semaphore.Weighted` (x/sync is already a dep) — configurable limit
   (default 64 concurrent per hub); on backpressure, close the connection with a
   dedicated close code if the queue overflows (protection against a
   malicious/looping client).
3. **`context.Background()` sweep**: in HTTP handlers → `r.Context()`; for work
   that must outlive the request (e.g., provisioning a sandbox after a webhook) →
   derive from the server's root ctx (`hub.baseCtx`) with an explicit timeout,
   never bare `Background()`. Enable the `contextcheck` linter to prevent
   regressions.
4. **Streaming with partial commits**: today `streamingBuf` accumulates chunks in
   memory and only persists at the end — a mid-stream timeout loses everything.
   Persist partial segments every N bytes/seconds (the `streamingSplit` mechanism
   already exists; start using it as a periodic flush), marking the record as
   partial until the stream ends.

**Acceptance:** a simple load test (script in `hack/`) with 100 simulated claws
shows no unbounded goroutine growth (`runtime.NumGoroutine` stable); a streaming
test with a mid-stream disconnect preserves the content received so far.

## 2.4 Data layer (`store/`)

**Problem:** raw SQL scattered across handlers (direct `db.Exec`/`QueryRow`), no
transactions in multi-step operations.

**Change:**

1. One repository per aggregate: `store.Claws`, `store.Messages`,
   `store.Checkpoints`, `store.Workflows`, `store.Analytics`, `store.Settings`.
   Interfaces defined in the consuming package when needed for tests; a single
   SQLite implementation.
2. Multi-step operations (create claw + initial message + analytics trigger) get a
   transactional repository method (`store.WithTx(ctx, fn)`).
3. `SQLITE_BUSY` handling: short retry with backoff in the store wrapper (not in
   handlers).
4. **Preparation for optional Postgres (not an implementation):** no
   SQLite-exclusive SQL syntax outside `store/`; placeholders via a constant. The
   decision to support Postgres comes later — this phase just keeps the door open.

**Acceptance:** `grep -r "db.Exec\|db.Query" pkg/hub --include='*.go' | grep -v store/`
is empty at the end; repository tests run against an in-memory DB.

## 2.5 Domain errors and typed WS payloads

1. Sentinel errors in `pkg/types/errors.go`: `ErrClawNotFound`,
   `ErrTenantMismatch`, `ErrWorkflowNotFound`, `ErrUnauthorized`… `httpserver`
   maps `errors.Is` → HTTP status in a single place (ends the response
   inconsistency).
2. `WSMessage.Payload interface{}` → typed decoding: keep the
   `{type, payload: json.RawMessage}` envelope on the wire (no protocol break) and
   replace the type assertions with a registry
   `map[string]func(json.RawMessage) (Handler, error)` unmarshalling straight into
   the concrete type. Assertion panics become logged protocol errors.

**Acceptance:** no type assertion on `Payload` outside the decoder; the
error→status mapping table is covered by a test.

## 2.6 Compatibility removals

- Remove the deprecated `?token=` fallback (transition started in Phase 0.3).
- Migrate the remaining handlers to the error envelope (started in Phase 0.2).

---

## Risks and mitigation

| Risk | Mitigation |
|---|---|
| Refactor breaks subtle WS/streaming behavior | Extract `claws/` last; parity tests (`make test-parity`) as the gate |
| Giant `git mv` PRs that are hard to review | One subpackage per PR; the move commit separate from the import-fix commit |
| Conflicts with features developed in parallel | A per-subpackage code-freeze window agreed before each extraction |
| Split mutexes introduce deadlocks | Lock-acquisition order documented in `hub.go`; `go test -race` mandatory in CI (verify it already is) |
