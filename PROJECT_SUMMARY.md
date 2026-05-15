# ElasticClaw Project Summary

## Overview

**ElasticClaw** is an open-source platform (Apache 2.0) for provisioning and managing ephemeral AI agent VMs called "claws." It functions as a "Heroku for AI agents" — a control plane that connects to cloud VM providers and manages the lifecycle of OpenClaw-powered AI agents.

Repository: `github.com/elasticclaw/elasticclaw`

---

## Core Architecture

### 1. Hub (`pkg/hub/`)
The central control plane — a Go HTTP/WebSocket server with SQLite backing.

**Key Components:**
- `server.go` — Main hub server, REST API, WebSocket handlers, claw lifecycle management
- `linear.go` — Linear webhook handler and factory engine for auto-spawning claws
- `bootstrap.go` — Generates bootstrap scripts for VM initialization
- `db.go` — SQLite schema and migrations
- `settings.go` — Hub configuration API
- `templates.go` — Template storage and management
- `factory_*.go` — Factory automation system (create, trigger, API, pipeline runner)
- `github*.go` — GitHub App integration, PR watching, issue webhooks
- `mcp_api.go` — MCP (Model Context Protocol) server management

**Key Features:**
- REST API (`/api/*`) + WebSocket (`/api/ws`) for real-time UI updates
- Embedded web UI (Next.js static export via `//go:embed all:out`)
- Provider polling and VM status tracking
- Factory automation (Linear, GitHub Issues, Shortcut integrations)
- GitHub credential helper for claws
- PR watching and CI failure detection
- Concurrency groups for limiting simultaneous claws

### 2. claw-bridge (`cmd/claw-bridge/`)
Runs on each provisioned VM, connecting the local OpenClaw gateway to the hub.

**Purpose:**
- Maintains persistent WebSocket connection to hub
- Proxies messages bidirectionally between user ↔ hub ↔ claw
- Handles bootstrap mode (installing Nix, Docker, OpenClaw)
- File transfer support (read/ack operations)
- Linear issue tracking integration on the claw side

**Environment Variables:**
- `ELASTICCLAW_HUB_URL` — hub WebSocket URL
- `ELASTICCLAW_CLAW_ID` — claw ID from hub
- `ELASTICCLAW_CLAW_TOKEN` — authentication token
- `ELASTICCLAW_GATEWAY` — local OpenClaw gateway address

### 3. CLI (`cmd/`)
The `elasticclaw` binary provides:

| Command | Purpose |
|---------|---------|
| `hub` | Run the hub server |
| `install` | Provision hub on remote Ubuntu VPS |
| `hub upgrade` | Upgrade remote hub via SSH |
| `upgrade` | Upgrade local CLI binary |
| `create` | Spawn a new claw |
| `kill` | Terminate a claw |
| `list` | List active claws |
| `chat` | Send messages to claws |
| `template` | Manage templates (push, show, rm) |
| `factory` | Factory management |
| `login` | Authenticate with hub |
| `profile` | Manage hub connection profiles |

### 4. Web UI (`web/`)
Next.js application (static export) providing:

- Dashboard with claw cards (sidebar + board view)
- Real-time chat interface with streaming responses
- Settings panel (Runtimes, LLM keys, GitHub, Integrations, Factories)
- Factory activity logs
- Manual factory trigger modal
- Drag-and-drop claw reordering
- Tag-based filtering

**Key Components:**
- `app/page.tsx` — Main dashboard
- `app/settings/page.tsx` — Settings UI
- `components/conversation-view.tsx` — Chat interface
- `components/sidebar.tsx` — Claw list sidebar
- `hooks/use-hub.ts` — WebSocket + polling, message state management
- `hooks/use-typewriter.ts` — Streaming response animation

### 5. Providers (`pkg/provider/`)
Pluggable VM provisioning backends:

| Provider | File | Description |
|----------|------|-------------|
| Replicated CMX | `replicated/replicated.go` | Primary provider - Ubuntu VMs via Replicated API |
| Daytona | `daytona/daytona.go` | Daytona sandbox environments |
| Vercel Sandbox | `vercel/vercel.go` | Vercel Sandbox provider |
| exe.dev | `exedev/exedev.go` | exe.dev provider |
| Local | `local/local.go` | Local development/testing |

### 6. Types & Config (`pkg/types/`, `pkg/config/`)
Core type definitions:
- `template.go` — `TemplateConfig`, `HubConfig`, `FactoryConfig`, `LLMKeyConfig`, `MCPConfig`, etc.
- `hub.go` — Hub-related types
- `config.go`, `hub.go` — Configuration loading

---

## Key Workflows

### Claw Lifecycle
1. **Creation:**
   - User creates claw via UI/CLI or factory triggers
   - Hub inserts claw record (status: `provisioning`)
   - Provider provisions VM (Replicated CMX, Daytona, etc.)
   - VM boots and runs bootstrap script
   - Bootstrap installs OpenClaw, claw-bridge
   - Bridge connects to hub → status: `connected`

2. **Operation:**
   - User sends message via WebSocket
   - Hub proxies to claw via bridge
   - Claw processes with OpenClaw gateway
   - Responses stream back to UI via typewriter effect

3. **Termination:**
   - User kills claw or factory triggers termination
   - Hub closes WebSocket, marks `status='deleted'`
   - Provider VM destroyed

### Factory Automation (Linear)
1. Linear sends webhook on issue status change
2. Hub validates HMAC signature
3. `processLinearEvent()` matches factory config
4. Issue enters `trigger_status` → `createClawForIssue()`
5. Issue leaves `trigger_status` → `terminateClawForIssue()`
6. Claw sends `[DONE] <pr-url>` → hub validates PRs → moves Linear issue → terminates claw

### Bootstrap Sequence
1. VM boots with cloud-init
2. Bootstrap script (generated by `bootstrap.go`) runs:
   - Creates `elasticclaw` user
   - Installs Nix and/or Docker (if configured)
   - Installs OpenClaw
   - Configures gateway with password
   - Starts gateway (port 18789)
   - Downloads and starts claw-bridge
   - Clones repo and injects CONTEXT.md

---

## Configuration

### Hub Config (`hub.yaml`)
```yaml
url: https://hub.example.com
token: hub_token_for_cli
public_url: https://hub.example.com  # for claws to connect back
ui_password: admin

providers:
  replicated:
    token: repl_...
    default_ttl: 48h
    default_instance_type: r1.large
  daytona:
    api_url: https://daytona.example.com
    api_key: key_...

llm_keys:
  - name: anthropic-prod
    provider: anthropic
    api_key: sk-...
    default: true
    default_model: anthropic/claude-sonnet-4-6

integrations:
  linear:
    - workspace: my-company
      token: lin_api_...
      webhook_secret: whsec_...

factories:
  - name: feature-factory
    integration: linear
    workspace: my-company
    trigger_status: "Ready for Agent"
    done_status: "In Review"
    template: base
    tags: [linear, feature]

mcp_servers:
  - name: github
    source: npx
    package: "@modelcontextprotocol/server-github"
    enabled: true
    secrets:
      GITHUB_TOKEN: github_token
```

---

## Build System

**Nix flake** (`flake.nix`) provides all dev tools — Go, Node, npm, ORAS, shellcheck.

**Make targets:**
- `make build` — Fast Go-only build (dev)
- `make build-web` — Build Next.js static export
- `make build-release` — Full build with embedded web UI
- `make test` — Go unit tests
- `make test-factory` — Factory integration tests
- `make test-bootstrap` — Bootstrap script tests
- `make test-container` — Container integration tests

---

## Key Design Decisions

1. **Single binary** — Hub + embedded web UI, no Docker required for deployment
2. **SQLite backing** — Simple, single-tenant OSS model
3. **WebSocket-first** — Real-time UI updates via WS, polling as fallback
4. **Template-based** — Claws created from versioned templates with config inheritance
5. **Factory automation** — Event-driven claw lifecycle via Linear/GitHub webhooks
6. **No SaaS** — Self-hosted only, no multi-tenancy complexity
7. **MCP support** — Model Context Protocol servers for tool extensibility

---

## Testing

- Unit tests: `go test ./...`
- Factory integration: `go test -tags integration ./pkg/hub/...`
- Bootstrap tests: Script generation and container execution
- CI: Depot (not GitHub Actions) — workflows in `.depot/workflows/`

---

## File Structure Summary

```
cmd/                    CLI commands
  claw-bridge/          Bridge binary source
  *.go                  CLI commands (hub, create, kill, etc.)
pkg/
  hub/                  Hub server implementation
  provider/             VM providers (replicated, daytona, vercel, local)
  types/                Core type definitions
  config/               Configuration loading
  install/              Install script generation
  bootopt/              Bootstrap optimization (experimental)
internal/
  webui/                Web UI embed package
web/                    Next.js web UI
.elasticclaw/
  templates/            Built-in templates
  factories/            Built-in factory definitions
```

---

## Documentation

- Main docs: `elasticclaw.ai/docs`
- Docs repo: `github.com/elasticclaw/elasticclaw.ai`
- Contributing: `CONTRIBUTING.md`
