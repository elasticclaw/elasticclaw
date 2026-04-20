# TOOLS.md - Environment Notes

## Repo

The `elasticclaw/elasticclaw` repo is cloned into your workspace.

```bash
alias ws="~/.openclaw/workspace/elasticclaw"
cd ~/.openclaw/workspace/elasticclaw
```

## Git & GitHub

`git` and `gh` CLI are pre-configured via the elasticclaw credential helper.
Tokens are fetched automatically from the hub — you don't need to set anything up.

```bash
git clone https://github.com/elasticclaw/elasticclaw  # works without auth prompt
gh pr create --base main                               # works
gh issue list --repo elasticclaw/elasticclaw           # works
```

Token is scoped: **write** on `elasticclaw/elasticclaw`.

## Go

Go is NOT pre-installed by default. Install if needed:
```bash
curl -fsSL https://go.dev/dl/go1.23.4.linux-amd64.tar.gz | sudo tar -C /usr/local -xz
export PATH=$PATH:/usr/local/go/bin
echo 'export PATH=$PATH:/usr/local/go/bin' >> ~/.bashrc
```

Or if nix is enabled in the template: it's already there via the flake.

Check: `go version`

## Node.js

Node 24 is pre-installed: `node --version`, `npm --version`

```bash
cd ~/.openclaw/workspace/elasticclaw/web
npm install
npm run dev   # dev server on :3000
```

## OpenClaw

OpenClaw is running on this VM at `http://localhost:18789`.
Don't restart it — it's what you're running inside.

Gateway logs: `~/openclaw-gateway.log`

## claw-bridge

claw-bridge is running in the background connecting this VM to the hub.
Logs: `~/claw-bridge.log`

Don't kill it — you'll lose your connection to the hub.

## Build Commands

```bash
make build          # Go only, fast (no web UI — dev iteration)
make build-release  # Full: web UI + Go binary (requires npm)
make build-web      # npm build → copy to internal/webui/out/
make test           # Go unit tests
make test-bootstrap # Bootstrap script tests
make test-install   # Container integration test for install scripts
```

## Hub Architecture

The hub binary:
- Serves REST API (`/api/*`) + WS (`/api/ws`)
- Serves embedded web UI at `/` (static Next.js export)
- Manages claw lifecycle (provision, bootstrap, poll, kill)
- Runs factory engine (Linear webhook → spawn/kill claws)
- Stores everything in SQLite

**Config:** `/etc/elasticclaw/hub.yaml` (system) or `~/.elasticclaw/hub.yaml` (dev)
**DB:** `/var/lib/elasticclaw/hub.db` (system) or `~/.elasticclaw/hub.db` (dev)

## Factory Engine (this is likely why you're here)

If you have a `CONTEXT.md`, a factory spawned you for a Linear issue.

The factory system is in `pkg/hub/linear.go`:
- `handleLinearWebhook()` — receives webhook, validates HMAC sig
- `processLinearEvent()` — matches factory config, dispatches create/terminate
- `createClawForIssue()` — provisions VM, injects CONTEXT.md
- `terminateClawForIssue()` — closes WS, marks deleted, destroys VM if mid-provision
- `handleClawDoneSignal()` — called when claw sends `[DONE]`

**When you're done:** Send `[DONE]` as a message. This moves the Linear issue and kills you.

## Bootstrap Sequence (how you got here)

1. Linear issue moved to trigger status → webhook hit hub
2. Hub called `createClawForIssue()`:
   - Resolved template, injected CONTEXT.md
   - Inserted claw record with `linear_issue_id`, tags, color
   - Provisioned Replicated CMX VM
3. VM booted → bootstrap script ran:
   - Installed Node 24, OpenClaw, claw-bridge
   - Configured OpenClaw (model, gateway password)
   - Started gateway (port 18789) + bridge (WS to hub)
   - Installed git credential helper
   - Cloned `elasticclaw/elasticclaw` repo to workspace
4. Bridge connected → hub sent intro message → you're here

## Debugging Tips

- Bridge not connecting? `cat ~/claw-bridge.log`
- Gateway not responding? `cat ~/openclaw-gateway.log`
- GitHub auth not working? Credential helper calls hub at `$ELASTICCLAW_HUB_URL/api/github/token/$ELASTICCLAW_CLAW_ID`
- Web UI 404? `make build-release` + check `internal/webui/out/` has `index.html`
- White page? Missing `_next/` — ensure `//go:embed all:out` (not `//go:embed out`)
- 301 redirect loop? Don't use `http.FileServer` — use `fs.ReadFile` directly
- Bootstrap syntax error? Script runs via `/bin/bash` stdin — avoid `PROFEOF`-style heredocs, use `printf`
- Claw resurrection after delete? Check `pollProviderStatus` excludes `deleted` and `bootstrapReplicated` has deleted guard
