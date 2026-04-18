# AGENTS.md - Your Workspace

## First Run

The repo is already cloned at `~/elasticclaw/` (or `~/.openclaw/workspace/elasticclaw/`). Check with `ls ~` and `ls ~/.openclaw/workspace/`.

Read this file, SOUL.md, USER.md, and TOOLS.md before doing anything.

## Every Session

1. Read SOUL.md — who you are
2. Read USER.md — who you're helping
3. Read memory/YYYY-MM-DD.md (today + yesterday) for recent context
4. Run `git -C ~/elasticclaw status` to see where things stand

## Memory

- **Daily notes:** `memory/YYYY-MM-DD.md` — log what you did, decisions made, blockers
- **Long-term:** `MEMORY.md` — distilled context that survives across sessions

Write to memory files. Don't rely on "mental notes" — this VM can be killed any time.

## How to Work

### Starting a task

1. Check current branch: `git -C ~/elasticclaw branch`
2. Pull latest: `git -C ~/elasticclaw pull origin main`
3. Create a feature branch for non-trivial changes

### Go code

- Build: `cd ~/elasticclaw && go build ./...`
- Vet: `go vet ./...`
- Format: `gofmt -w <file>`
- Test: `go test ./...`

### Web UI (Next.js in `web/`)

- Install: `cd ~/elasticclaw/web && npm install`
- Dev: `npm run dev`
- Type check: `npx tsc --noEmit`
- Build: `npm run build`

### Committing

```bash
cd ~/elasticclaw
git add -A
git commit -m "feat: <description>"
git push origin <branch>
```

Then open a PR with `gh pr create`.

### CI

CI runs on Depot. Workflows are in `.depot/workflows/`:
- `release.yaml` — builds Go binaries + Homebrew update on tag push
- `release-web.yaml` — builds + pushes `marc/elasticclaw-web` Docker image on tag push

## Key files to know

```
cmd/                    CLI commands (chat, create, kill, list, login, hub, template)
cmd/claw-bridge/        claw-bridge binary (the WS proxy that runs on each VM)
pkg/hub/server.go       Hub server — REST, WS, lifecycle, bootstrap scripts
pkg/hub/github.go       GitHub App token generation
pkg/hub/relay.go        Relay client (outbound WS to relay server)
pkg/types/              Shared types (HubConfig, Template, ClawStatus, etc.)
pkg/config/             Config loading (hub.yaml, CLI config)
pkg/provider/           Provider implementations (replicated, daytona, vercel, local)
web/                    Next.js web UI
.elasticclaw/templates/ Template definitions (you are in one right now)
```

## Safety

- `go build ./...` must pass before any Go commit
- `npx tsc --noEmit` must pass before any TS commit
- Never put API keys, tokens, or secrets in the repo
- The repo is public (Apache 2.0)
