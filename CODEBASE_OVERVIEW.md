# ElasticClaw Codebase Overview

This document provides a high-level summary of the ElasticClaw codebase structure and architecture.

## What is ElasticClaw?

ElasticClaw is an open-source platform (Apache 2.0) for provisioning and managing ephemeral AI agent VMs ("claws"). It enables users to spin up isolated AI agent environments on demand, connect them to a central hub, and automate workflows through factory integrations.

## Architecture Components

### 1. Hub (`pkg/hub/`)

The central control plane - a Go HTTP/WebSocket server with SQLite backing.

**Key files:**
- `server.go` - HTTP/WS server, claw lifecycle management, provider polling
- `linear.go` - Linear webhook handler and factory engine
- `bootstrap.go` - VM bootstrap script generation (pure function)
- `db.go` - SQLite schema and migrations
- `settings.go` - Hub configuration API

### 2. CLI (`cmd/`)

User-facing command-line interface built with Cobra.

**Key commands:**
- `hub` - Start/stop the hub server
- `create` - Create a new claw
- `kill` - Terminate a claw
- `chat` - Interactive chat with a claw
- `template` - Template management (new, push, rm, show)
- `install` - Install hub on a remote VPS
- `upgrade` - Self-upgrade the CLI

### 3. Web UI (`web/`)

Next.js React application that gets embedded in the Go binary.

**Key areas:**
- `app/page.tsx` - Main dashboard with claw list and chat
- `hooks/use-hub.ts` - WebSocket connection and state management
- `components/` - React components for UI

### 4. claw-bridge (`cmd/claw-bridge/`)

Companion binary that runs on each VM:
- Maintains persistent WebSocket connection to hub
- Proxies gateway traffic between OpenClaw and the hub
- Reports status and health back to hub

### 5. Providers (`pkg/provider/`)

Pluggable VM provisioning backends:
- `replicated/` - Replicated Compatibility Matrix (CMX) VMs
- `daytona/` - Daytona workspaces
- `vercel/` - Vercel Sandboxes
- `exedev/` - Exedev provider

## Factory System

The factory engine (in `pkg/hub/linear.go`) enables automated claw lifecycle management through Linear integration:

1. **Webhook endpoint** receives Linear issue status changes
2. **HMAC validation** ensures webhook authenticity per-factory
3. **Dedup logic** prevents duplicate claw creation from retries
4. **Lifecycle mapping**:
   - Issue enters `trigger_status` → provision VM, inject CONTEXT.md
   - Issue leaves `trigger_status` → terminate claw
   - Agent sends `[DONE] <pr-url>` → move issue to `done_status`, terminate

## Build System

Uses Nix flake for reproducible toolchain:

```bash
make build          # Fast Go-only build
make build-release  # Full build with embedded web UI
make test           # Unit tests
make test-factory   # Factory integration tests
```

## Key Design Patterns

1. **Single binary** - Go binary embeds Next.js static export via `//go:embed all:out`
2. **Pure bootstrap generation** - `BootstrapParams` → script, no side effects, fully testable
3. **WebSocket streaming** - Real-time chat with typewriter effect
4. **LocalStorage persistence** - Message cache and claw order survive refresh
5. **Config hot-reload** - `PATCH /api/settings` updates hub.yaml live

## Directory Structure

```
elasticclaw/
├── cmd/              # CLI commands
├── pkg/
│   ├── hub/          # Hub server core
│   ├── provider/     # VM providers
│   ├── types/        # Shared types
│   ├── config/       # Config loading
│   └── install/      # Install scripts
├── internal/webui/   # Go embed package
└── web/              # Next.js frontend
```

## Documentation

- Main docs: https://elasticclaw.ai/docs
- In-repo context: `AGENTS.md`, `MEMORY.md`, `SOUL.md`, `TOOLS.md`, `USER.md`
