# AGENTS.md - Your Workspace

## First Run

There are multiple repos cloned:

- `~/.openclaw/workspace/elasticclaw/`: this is the main elasticlaw repo where it='s likely you will be working
- `~/.openclaw/workspace/elasticclaw.ai/`: this is the public https://elasticclaw.ai site that includes our docs

Check that these repos exist with:

```bash
ls ~/.openclaw/workspace/
```

Read `SOUL.md`, `USER.md`, `MEMORY.md`, and `TOOLS.md` before doing anything.

**Context file:** If a `CONTEXT.md` exists in your workspace, read it. It is injected by the factory and contains the Linear issue context.

## Every Session

1. Read SOUL.md — who you are
2. Read USER.md — who you're helping
3. Read MEMORY.md — project context, architecture, open work, known gotchas
4. Read memory/YYYY-MM-DD.md (today + yesterday)
5. Read CONTEXT.md if it exists (Linear issue context)
6. `git -C ~/.openclaw/workspace/elasticclaw status` to see where things stand
7. `git -C ~/.openclaw/workspace/elasticclaw pull` to get latest
8. `cd ~/.openclaw/workspace/elasticclaw && nix develop` — **all** dev tools (Go, Node, npm, ORAS, etc.) come from the repo `flake.nix`. Nix and Docker are pre-installed; do **not** install language runtimes or ORAS by hand.

## Toolchain

Work only inside `nix develop` at the repo root. Do not install Go, Node, npm, or ORAS outside the flake.

```bash
cd ~/.openclaw/workspace/elasticclaw
nix develop
# optional sanity checks:
go version && node --version && npm --version && oras version
docker version   # Docker is on the VM; use for container tests
```

## Git

You have a git credential helper installed. Use that to interaction with git and gh.
NEVER force push and rebase unless you are resolving conflicts. When addressing standard feedback, it's preferable to push a new commit.

## Memory

- **Daily notes:** `memory/YYYY-MM-DD.md` — log what you did, decisions made, blockers
- **Long-term:** `MEMORY.md` — distilled context that survives across sessions
- **Update MEMORY.md** when you learn something new about the codebase

Write to memory files. Don't rely on "mental notes" — this VM can be killed any time.

## How to Work

### Starting a task

1. Check current branch: `git -C ~/.openclaw/workspace/elasticclaw branch`
2. Pull latest: `git -C ~/.openclaw/workspace/elasticclaw pull origin main`
3. Create a feature branch for non-trivial changes: `git checkout -b feat/my-thing`
4. `cd ~/.openclaw/workspace/elasticclaw && nix develop` before any `make`, `go`, or `npm` work
5. Optional alias: `alias ws="cd ~/.openclaw/workspace/elasticclaw"` (you still must run `nix develop` from that directory)

### Go code

From repo root, inside `nix develop`:

```bash
make build           # fast build, Go only (no web embed)
go build ./...       # verify compiles
go vet ./...         # static analysis
go test ./...        # run tests
gofmt -w <file>      # format edited files
```

### Web UI (Next.js in `web/`)

Inside `nix develop` (repo root):

```bash
cd web
npm install
NEXT_PUBLIC_HUB_URL=http://localhost:8080 npm run dev  # dev server :3000

# Full static build (embeds in binary):
cd .. && make build-release
```

### Full local development workflow (2 terminals)

In each terminal: `cd ~/.openclaw/workspace/elasticclaw && nix develop`.

Terminal 1 (hub):
```bash
make build
./bin/elasticclaw hub --no-web-ui
```

Terminal 2 (web):
```bash
cd web
cp .env.example .env.local
# Ensure .env.local contains:
# NEXT_PUBLIC_HUB_URL=http://localhost:8080
npm install
npm run dev
```

Then open `http://localhost:3000`.

### Tests (what to run and when)

Inside `nix develop` at repo root:

```bash
make test           # baseline for all changes
make test-bootstrap # required for bootstrap/install script changes
make test-install   # requires Docker and integration tag path
```

Container-heavy tests (repo root, inside `nix develop`):
```bash
make test-container
```

Factory integration tests:
```bash
make test-factory
```

### Committing

```bash
cd ~/.openclaw/workspace/elasticclaw
git add -A
git commit -m "feat: <description>"
git push origin <branch>
gh pr create --base main
```

## Documentation Updates (Required)

- Docs repo: `elasticclaw/elasticclaw.ai`
- It is already cloned at `~/.openclaw/workspace/elasticclaw.ai`
- If your change affects user-facing behavior, CLI commands/flags/output, API/schema/contracts, configuration, install/upgrade flows, or workflows, you must update docs in `elasticclaw/elasticclaw.ai` as part of the same task.
- Open a corresponding docs PR whenever docs changes are needed. Product/code PRs that require docs must not be considered done without the docs PR URL.

## CI

CI runs on Depot. Workflows in `.depot/workflows/`:
- `release.yaml` — builds Go binaries on tag push, updates Homebrew formula
- `test.yaml` — unit tests + shellcheck + container tests on PR

**Important:** Depot does NOT support `release` event trigger — only `push`/`pull_request`/`tag`.

## Reading Depot CI Logs

If you need to inspect CI logs for a Depot build, use the Depot CI API directly. The `DEPOT_TOKEN` env var is injected into the workspace (set via hub secrets). It is a Bearer token for `https://api.depot.dev`.

**Security note:** `DEPOT_TOKEN` is exposed to every process in the workspace, just like any other injected secret. Store it in hub secrets with the narrowest scope possible — ideally a token that can only read CI logs and metrics, not trigger builds or access other Depot resources. If you only need the token occasionally, you can also omit it from the workspace config and set it manually when asked.

Useful API calls (Connect RPC over HTTP, JSON encoding):

```bash
# List recent CI runs for a repo
curl -fsSL https://api.depot.dev/depot.ci.v1.CIService/ListRuns \
  -H "Authorization: Bearer $DEPOT_TOKEN" \
  -H "Content-Type: application/json" \
  -H "Connect-Protocol-Version: 1" \
  --json '{"repo": "elasticclaw/elasticclaw", "pageSize": 10}'

# Get run status with nested workflows, jobs, and attempts
curl -fsSL https://api.depot.dev/depot.ci.v1.CIService/GetRunStatus \
  -H "Authorization: Bearer $DEPOT_TOKEN" \
  -H "Content-Type: application/json" \
  -H "Connect-Protocol-Version: 1" \
  --json '{"runId": "<run-id>"}'

# Get logs for a job attempt (use current attemptId from GetRunStatus)
curl -fsSL https://api.depot.dev/depot.ci.v1.CIService/GetJobAttemptLogs \
  -H "Authorization: Bearer $DEPOT_TOKEN" \
  -H "Content-Type: application/json" \
  -H "Connect-Protocol-Version: 1" \
  --json '{"jobId": "<job-id>", "attemptId": "<attempt-id>"}'

# Get failure diagnosis for a failed run/workflow/job/attempt
curl -fsSL https://api.depot.dev/depot.ci.v1.CIService/GetFailureDiagnosis \
  -H "Authorization: Bearer $DEPOT_TOKEN" \
  -H "Content-Type: application/json" \
  -H "Connect-Protocol-Version: 1" \
  --json '{"runId": "<run-id>"}'
```

Full reference: https://depot.dev/docs/api/ci/reference

When asked to read CI logs, start by listing recent runs, identify the relevant run/workflow/job, then fetch the attempt logs. Use `GetFailureDiagnosis` when the run failed to get a summarized reason.

## Key Files

```
cmd/                    CLI commands
  install.go            elasticclaw install (provisions hub on remote server)
  hub_upgrade.go        elasticclaw hub upgrade (upgrades remote hub via SSH)
  upgrade.go            elasticclaw upgrade (upgrades local CLI)
pkg/hub/
  server.go             Hub server — REST, WS, claw lifecycle, provider polling
  linear.go             Linear webhook handler, factory engine
  bootstrap.go          GenerateReplicatedBootstrapScript() — pure function
  settings.go           GET/PATCH /api/settings
  templates.go          Hub template storage
  db.go                 InitDB — SQLite schema + migrations
pkg/install/scripts.go  Install script generation (pure, tested)
pkg/types/template.go   FactoryConfig, HubConfig, etc.
pkg/config/hub.go       Hub config loading
pkg/provider/replicated/ Replicated CMX provider
web/                    Next.js web UI
  app/settings/page.tsx Settings page (Runtimes, LLM, GitHub, Integrations, Factories)
  app/factories/[name]/ Factory activity page
  hooks/use-hub.ts      WS + polling, message/claw state
internal/webui/         Embed package for web UI binary
.elasticclaw/templates/ Template definitions
```

## Safety

- `go build ./...` must pass before any Go commit
- `go test ./...` must pass
- `make build-release` must pass for web/UI-affecting changes
- Never put API keys, tokens, or secrets in the repo (public, Apache 2.0)
- Open a PR for every change (no direct pushes to main)

## When You're Done with a GitHub Issue

1. Make sure all changes are committed on a feature branch (never commit directly to main)
2. Open a PR on GitHub
3. Send exactly this message with all PR URLs space-separated after `[DONE]`:

```
[DONE] https://github.com/org/repo/pull/123
```

For work spanning multiple repos:
```
[DONE] https://github.com/org/repo/pull/123 https://github.com/org/other-repo/pull/45
```

The factory engine will validate that all PRs are open, record them, move the Linear issue to done, and terminate this claw. If validation fails you'll receive an error message — fix the issue and resend `[DONE] <url>`.
