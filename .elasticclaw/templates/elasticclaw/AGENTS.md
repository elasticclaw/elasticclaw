# AGENTS.md - Your Workspace

## First Run

The repo is already cloned at `~/.openclaw/workspace/elasticclaw/`. Check with `ls ~/.openclaw/workspace/`.

Read SOUL.md, USER.md, and TOOLS.md before doing anything.

## Every Session

1. Read SOUL.md — who you are
2. Read USER.md — who you're helping
3. Read memory/YYYY-MM-DD.md (today + yesterday) for recent context
4. `git -C ~/.openclaw/workspace/elasticclaw status` to see where things stand
5. `git -C ~/.openclaw/workspace/elasticclaw pull` to get latest

## Memory

- **Daily notes:** `memory/YYYY-MM-DD.md` — log what you did, decisions made, blockers
- **Long-term:** `MEMORY.md` — distilled context that survives across sessions

Write to memory files. Don't rely on "mental notes" — this VM can be killed any time.

## How to Work

### Starting a task

1. Check current branch: `git -C ~/ws branch` (alias `~/ws` = workspace elasticclaw dir)
2. Pull latest: `git pull origin main`
3. Create a feature branch for non-trivial changes: `git checkout -b feat/my-thing`

### Go code

```bash
cd ~/ws  # or ~/.openclaw/workspace/elasticclaw
make build           # fast build, Go only
go build ./...       # verify compiles
go vet ./...         # check for issues
go test ./...        # run tests
gofmt -w <file>      # format
```

### Web UI (Next.js in `web/`)

```bash
cd ~/ws/web
npm install
NEXT_PUBLIC_HUB_URL=http://localhost:8080 npm run dev  # dev server :3000

# Full static build (embeds in binary):
cd ~/ws && make build-release
```

### Committing

```bash
cd ~/ws
git add -A
git commit -m "feat: <description>"
git push origin <branch>
gh pr create  # open a PR
```

### CI

CI runs on Depot. Workflows in `.depot/workflows/`:
- `release.yaml` — builds Go binaries on tag push
- `test.yaml` — unit tests + shellcheck + container tests on PR

## Key Files

```
cmd/                    CLI commands
cmd/claw-bridge/        claw-bridge binary (WS proxy on each VM)
pkg/hub/server.go       Hub server — REST, WS, lifecycle, bootstrap
pkg/hub/bootstrap.go    Pure function: GenerateReplicatedBootstrapScript()
pkg/hub/github.go       GitHub App token generation
pkg/install/scripts.go  Install script generation (pure functions, tested)
pkg/types/              Shared types
pkg/config/             Config loading
pkg/provider/           Provider implementations
web/                    Next.js web UI (static export)
internal/webui/         Embed package for web UI binary
.elasticclaw/templates/ Template definitions (you are in one right now)
```

## Safety

- `go build ./...` must pass before any Go commit
- `go test ./...` must pass
- Never put API keys, tokens, or secrets in the repo (it's public, Apache 2.0)
- PR for non-trivial changes, direct push for obvious fixes
