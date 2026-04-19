# MEMORY.md - Long-Term Memory

## Project: elasticclaw

Open source tool for provisioning ephemeral AI agent VMs. Apache 2.0.
Repo: github.com/elasticclaw/elasticclaw

### Architecture summary

- **Hub** (`pkg/hub/server.go`) — Go HTTP/WS server, SQLite backing, manages claw lifecycle
- **claw-bridge** (`cmd/claw-bridge/`) — runs on each VM, persistent WS → hub, proxies gateway
- **CLI** (`cmd/`) — login, list, create, chat, kill, template, hub, install, profile
- **Web UI** (`web/`) — Next.js static export, embedded in Go binary via `//go:embed all:out`
- **Providers** — `pkg/provider/replicated`, `daytona`, `vercel`, `local`
- **Relay** (`relay` repo) — optional TURN-style WS proxy for NAT traversal

### Single binary architecture (feat/embedded-web, merged to main)
- `make build` = Go only, no web (fast dev iteration)
- `make build-release` = npm build → copy to `internal/webui/out/` → go build with `-tags embedweb`
- `//go:embed all:out` required (not `//go:embed out`) to include `_next/` directory
- `http.FileServer` causes 301 redirects — use `fs.ReadFile` directly for static serving
- No Next.js middleware, no cookies — auth via sessionStorage `ec_hub_token`

### Auth model
- Login: `POST /api/auth/login` with `{password}` → returns `{hubToken}`
- Browser stores `ec_hub_token` in sessionStorage
- All hub calls: `Authorization: Bearer <hubToken>`
- Hub validates against `hubCfg.Token` (same token for API and web UI)
- `ui_password:` in hub.yaml sets the login password

### Key decisions

- Single-tenant OSS only (no SaaS, no master token)
- SQLite backing
- bridge_image in hub.yaml: set → use as-is OCI ref; unset + tagged → GitHub releases; unset + dev → fail
- Relay is explicit opt-in via `relay_url` / `relay_secret`
- claw-bridge has 32MB WS read limit
- Credential helper calls hub AFTER bridge starts (hub API reachable via bridge proxy)
- Config: `/etc/elasticclaw/hub.yaml` (system) or `~/.elasticclaw/hub.yaml` (dev)
- Data: `/var/lib/elasticclaw/` (system) or `~/.elasticclaw/` (dev)

### Install command
- `elasticclaw install --server ssh://user@ip --domain hub.example.com`
- No Docker needed — single binary handles everything
- Caddy config: single `reverse_proxy localhost:8080` (not split routing)
- `elasticclaw hub service install` for manual systemd setup on server
- `elasticclaw hub caddy install --domain hub.example.com` for Caddy

### Bootstrap sequence (Replicated)
1. Apt: Node 24, git
2. `npm install -g openclaw@latest`
3. Configure OpenClaw (model, gateway password, auth)
4. Start gateway, wait for :18789
5. Download claw-bridge (OCI or GitHub releases)
6. Start bridge, wait for connection
7. Install GitHub credential helper (AFTER bridge)
8. Clone repos

### Hub env vars (web)
- `NEXT_PUBLIC_HUB_URL` — browser-visible hub URL for dev (not a secret)
- No server-side env needed in production (same-origin)

### CI
- Depot-based (`.depot/workflows/`)
- `release.yaml` — Go binaries on tag push
- `release-web.yaml` — Docker image (deprecated, now embedded)
- `test.yaml` — unit tests + shellcheck + container integration tests on PR

### Known gotchas
- `tsconfig.tsbuildinfo` causes merge conflicts — always resolve with `--theirs`
- Linux-only npm deps must be in `optionalDependencies`
- `isRedirectError` is in `next/dist/client/components/redirect-error`
- `trailingSlash: true` causes 308 loops in dev — only enable in prod static export
- `SilenceUsage: true` on cobra root command to avoid wall of help text on errors
- Replicated VMs use SSH proxy that rejects arbitrary bash commands (not suitable for `install`)

### Claw features
- **Tags**: flat strings (e.g. `template:elasticclaw`, `nix`), auto-assigned `template:<name>`
- **Color**: 16 named accent colors, auto-assigned from name hash
- **Rename**: inline from UI card
- **Auto-wake**: hub sends silent intro message when claw first connects
