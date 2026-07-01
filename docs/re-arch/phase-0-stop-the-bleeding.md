# Phase 0 — Stop the bleeding

**Estimated duration:** 1 week · **Risk:** low · **Dependencies:** none

Goal: remove the risks that can cause data loss, credential leakage, or production
crashes **without** changing the architecture. Every item is localized and
independent of the others — one PR per item.

---

## 0.1 Graceful shutdown

**Problem:** `pkg/hub/server.go` boots with `http.ListenAndServe` and starts 8+
background goroutines (`pollProviderStatus`, `keepAliveDaytonaSandboxes`,
`pruneAnalytics`, `statusWatchdog`, `checkpointScheduler`, PR watcher, cron,
integration poller) with no cancellation mechanism. A SIGTERM kills the process in
the middle of SQLite writes and drops WS connections without a goodbye.

**Change:**

1. Create a root `context.Context` at server boot, cancelled by
   `signal.NotifyContext(ctx, os.Interrupt, syscall.SIGTERM)`.
2. Replace `http.ListenAndServe` with an explicit `http.Server` with timeouts
   (`ReadHeaderTimeout`, `IdleTimeout`) and call `srv.Shutdown(drainCtx)` on cancel
   (~15s drain, configurable).
3. Every background goroutine receives the root ctx and exits on `ctx.Done()`.
   Use `golang.org/x/sync/errgroup` (already a dependency) to wait for all of them.
4. Shutdown order: stop accepting connections → close claw WS connections with a
   close frame (`"killed"` is NOT sent — the sandbox stays alive) → wait for
   goroutines → close `sql.DB`.
5. `/healthz` returns 503 during the drain (today it always returns 200).

**Acceptance:**
- `kill -TERM` on a hub with a connected claw: process exits in <20s, the log shows
  the drain sequence, SQLite left with no dirty orphaned `-wal`.
- Integration test: start the server via `factorytest`, send the signal (or call
  `Shutdown()`), verify goroutines finished (no leaks, optionally via `goleak`).

## 0.2 Recovery middleware + error envelope

**Problem:** no handler has recover — a panic kills the goroutine and, in chains
with shared state, can corrupt invariants. Error responses alternate between plain
`http.Error` text and ad-hoc JSON.

**Change:**

1. `withRecovery` middleware at the top of the chain (before CORS): recover, log
   the stack with a request ID (Phase 1 adds the ID; the remote addr is fine here),
   respond 500.
2. Define a single error envelope and helper:
   ```go
   type apiError struct {
       Error string `json:"error"`
       Code  string `json:"code,omitempty"`
   }
   func writeErr(w http.ResponseWriter, status int, code, msg string)
   ```
3. Migrate only the handlers used by the UI (`/api/claws*`, `/api/messages*`,
   `/api/settings*`, `/api/auth/*`) in this phase; the rest migrates in Phase 2
   together with the reorganization. The frontend (`web/lib/api.ts:78-106`) already
   tries to parse JSON errors and falls back to text — compatible with a gradual
   migration.

**Acceptance:** a test that injects a panic in a fake handler and verifies 500 +
log; UI handlers return the JSON envelope on error.

## 0.3 Token out of the query string

**Problem:** `withAuth` (`server.go:~416`) accepts `?token=` as a fallback. Tokens
show up in access logs, reverse-proxy logs, and URL history.

**Change:**

1. REST: accept `Authorization: Bearer` only. Remove the query fallback.
2. WebSocket (`/api/ws`, `/api/terminal/{id}`) and resources used as `<img>` src
   (`/api/files/view/...`) cannot send headers from the browser. Replace with a
   **single-use ticket**: `POST /api/auth/ticket` (authenticated via header)
   returns an opaque token with a 30s TTL, redeemable once; WS/img use `?ticket=`.
3. Frontend: `web/hooks/use-hub.ts` and `web/lib/api.ts` request a ticket before
   opening WS / displaying a file. The URL-redaction logic in the logs
   (`use-hub.ts:42-51`) stays.
4. Transition: keep accepting `?token=` for 1 release with a log warning
   (`deprecated: token in query`), removable in Phase 2.

**Acceptance:** grepping the server code for `Query().Get("token")` only finds the
ticket path; terminal and file-preview E2E tests pass using tickets.

## 0.4 Restricted CORS

**Problem:** `corsMiddleware` responds `Access-Control-Allow-Origin: *`.

**Change:** the allowed origin comes from config (`hub.yaml: allowed_origins`,
default = the hub's own origin; in dev, `http://localhost:3000` via
`docker/hub.dev.yaml`). Echo the request origin if it's on the list; otherwise omit
the header. Always send `Vary: Origin`.

**Acceptance:** the dev compose keeps working (web:3000 → hub:8080); a request from
an unlisted origin gets no CORS header.

## 0.5 Migrations: stop swallowing errors

**Problem:** `pkg/hub/db.go:23-87` runs ~30 `ALTER TABLE` statements with
`_, _ = db.Exec(...)`. Real migration failures (full disk, lock) are invisible.

**Change (minimal — replacing the mechanism itself is Phase 1.4):**
classify the error: if it's "duplicate column name", ignore it; any other error is
logged and **aborts boot**. Helper `execIgnoreDuplicate(db, stmt string) error`.

**Acceptance:** a test with a read-only DB fails boot with a clear message; normal
boot remains idempotent.

## 0.6 Frontend hygiene

1. `web/package.json`: `"name": "my-project"` → `"elasticclaw-web"`.
2. Remove generated, unused shadcn components (confirmed: `input-otp`,
   `embla-carousel`/`ui/carousel.tsx`; verify imports before removing each one).
3. Remove the corresponding dependencies from `package.json` and run
   `npm run build` to confirm.

**Acceptance:** green `npm run build`; `npx depcheck` (or an import grep) shows no
obvious orphaned deps.

---

## Out of scope for this phase

- Request ID / structured logging (Phase 1).
- Splitting `pkg/hub` (Phase 2).
- Secret encryption and standard JWT (Phase 3).
