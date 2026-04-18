# MEMORY.md - Long-Term Memory

## Project: elasticclaw

Open source tool for provisioning ephemeral AI agent VMs. Apache 2.0.
Repo: github.com/elasticclaw/elasticclaw

### Architecture summary

- **Hub** (`pkg/hub/server.go`) — Go HTTP/WS server, SQLite backing, manages claw lifecycle
- **claw-bridge** (`cmd/claw-bridge/`) — runs on each VM, persistent WS → hub, proxies gateway
- **CLI** (`cmd/`) — login, list, create, chat, kill, template, hub init
- **Web UI** (`web/`) — Next.js, server-side auth, connects to hub via `/hub/*` rewrite proxy
- **Providers** — `pkg/provider/replicated`, `daytona`, `vercel`, `local`
- **Relay** (`relay` repo) — optional TURN-style WS proxy for NAT traversal

### Key decisions

- Single-tenant OSS only (no SaaS, no master token, no tenant table in active use)
- SQLite backing (no Postgres)
- `bridge_image` in hub.yaml: set → use as-is OCI ref; unset + tagged → GitHub releases URL; unset + dev → fail loudly
- Relay is explicit opt-in via `relay_url` / `relay_secret` in hub.yaml
- claw-bridge has 32MB WS read limit (default was 32KB — broke on large LLM responses)
- Credential helper calls hub at `/api/github/token/<clawID>` — runs AFTER bridge starts
- GitHub token endpoint auth: validates against `hub_claw_token` directly (no tenant lookup)
- Web UI default password: `admin` if `ELASTICCLAW_UI_TOKEN` not set (warns in dev)
- Daytona exec needs `export HOME=/home/daytona` + full PATH in every command

### Bootstrap sequence (Replicated)

1. Apt: Node 24, git
2. `npm install -g openclaw@latest`
3. `openclaw onboard --non-interactive` + Python patch to set model/key/gateway auth
4. Start gateway via `nohup`, wait for `:18789` to respond
5. Download claw-bridge (OCI or GitHub releases)
6. Export env vars, start bridge via `nohup`
7. Wait up to 10s for bridge to connect (check log for "connected|registered|ready")
8. Install GitHub credential helper (now that bridge proxy is up)
9. Run `gh auth login` + clone repos

### Hub env vars (web UI)

- `ELASTICCLAW_UI_TOKEN` — web UI password
- `ELASTICCLAW_HUB_URL` — hub URL (server-side only)
- `ELASTICCLAW_HUB_TOKEN` — hub user token (server-side only)

### CI

- Depot-based CI (`.depot/workflows/`)
- `release.yaml` — Go binaries on tag push, triggers Homebrew update
- `release-web.yaml` — Docker image `marc/elasticclaw-web` on tag push
- Homebrew tap: `elasticclaw/elasticclaw`

### Known issues / gotchas

- `tsconfig.tsbuildinfo` causes merge conflicts — always resolve with `--theirs`
- Linux-only npm deps (tailwindcss oxide) must be in `optionalDependencies`
- `isRedirectError` is in `next/dist/client/components/redirect-error`, not `next/navigation`
- Relay over Cloudflare Tunnel has idle WS timeouts — use Caddy directly on VPS
