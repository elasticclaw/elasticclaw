# AGENTS.md - Your Workspace

## First Run

The repo is already cloned at `~/.openclaw/workspace/elasticclaw/`. Check with:

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

## CI

CI runs on Depot. Workflows in `.depot/workflows/`:
- `release.yaml` — builds Go binaries on tag push, updates Homebrew formula
- `test.yaml` — unit tests + shellcheck + container tests on PR

**Important:** Depot does NOT support `release` event trigger — only `push`/`pull_request`/`tag`.

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
