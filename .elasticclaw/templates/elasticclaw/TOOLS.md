# TOOLS.md - Environment Notes

## Repo

The `elasticclaw/elasticclaw` repo is cloned into your workspace.
Check: `ls ~/.openclaw/workspace/` or `ls ~/`

```bash
cd ~/.openclaw/workspace/elasticclaw
# or wherever it landed
```

## Git & GitHub

`git` and `gh` CLI are pre-configured via the elasticclaw credential helper.
Tokens are fetched automatically from the hub — you don't need to set anything up.

```bash
git clone https://github.com/elasticclaw/elasticclaw  # works without auth prompt
gh pr create                                           # works
gh issue list --repo elasticclaw/elasticclaw           # works
```

Token is scoped: **write** on `elasticclaw/elasticclaw`.

## Go

Go is NOT pre-installed. Install if needed:
```bash
curl -fsSL https://go.dev/dl/go1.23.4.linux-amd64.tar.gz | sudo tar -C /usr/local -xz
export PATH=$PATH:/usr/local/go/bin
echo 'export PATH=$PATH:/usr/local/go/bin' >> ~/.bashrc
```

Check: `go version`

## Node.js

Node 24 is pre-installed: `node --version`

```bash
cd ~/.openclaw/workspace/elasticclaw/web
npm install
npm run dev   # starts web UI on :3000
```

## OpenClaw

OpenClaw is running on this VM at `http://localhost:18789`.
Don't restart it — it's what you're running inside.

Gateway logs: `~/openclaw-gateway.log`

## claw-bridge

claw-bridge is running in the background connecting this VM to the hub.
Logs: `~/claw-bridge.log`

Don't kill it — you'll lose your connection.

## Architecture: How This VM Got Here

1. User ran `elasticclaw create --template elasticclaw` from the hub CLI
2. Hub provisioned a Replicated CMX VM (r1.large)
3. Hub SSHed in and ran the bootstrap script which:
   - Installed Node 24, OpenClaw, claw-bridge
   - Configured OpenClaw with the right model + gateway password
   - Started the OpenClaw gateway (port 18789)
   - Started claw-bridge (connects back to hub via WebSocket)
   - Installed git credential helper (fetches GitHub App tokens from hub)
   - Cloned this repo into workspace
4. claw-bridge registered with hub → status became "online"
5. You are now running inside that OpenClaw instance

## Hub Architecture

```
elasticclaw CLI / Web UI
        |
        v
    Hub (Go)  ← SQLite DB, REST + WS API
        |
        |--- REST: /api/claws, /api/messages, /api/github/token
        |--- WS:   /ws/user (UI), /ws/claw (bridge)
        |
        v
  claw-bridge (on this VM)
        |
        v
  OpenClaw gateway (localhost:18789)
```

## Relay (optional)

If `relay_url` is set in hub.yaml, claw-bridge connects to the relay instead
of directly to the hub. This handles NAT traversal.

Relay env vars (set automatically if relay is configured):
- `ELASTICCLAW_RELAY_URL`
- `ELASTICCLAW_HUB_ID`
- `ELASTICCLAW_RELAY_TOKEN`

## Environment Variables (set during bootstrap)

- `ELASTICCLAW_HUB_URL` — hub URL (may be via relay proxy on localhost:18790)
- `ELASTICCLAW_CLAW_ID` — this claw's UUID
- `ELASTICCLAW_CLAW_TOKEN` — auth token for hub API calls
- `ELASTICCLAW_CLAW_NAME` — human-readable name
- `ELASTICCLAW_GATEWAY_PASSWORD` — OpenClaw gateway auth password
- `ANTHROPIC_API_KEY` — injected from hub's llm_keys config
- `OPENCLAW_DEFAULT_MODEL` — e.g. `anthropic/claude-sonnet-4-6`

## Key Providers

### Replicated CMX
- Instance types: `r1.small`, `r1.medium`, `r1.large`
- SSH user: derived from hub's ed25519 key comment (`elasticclaw@hub`)
- Hub polls Replicated API for status; bootstrap triggers when VM hits "running"

### Daytona
- Uses `snapshot` + `default_snapshot` fields (not `instance_type`)
- exec via Daytona API (no SSH) — PATH must include nvm dir in every call

### Vercel Sandbox
- Not fully tested yet

## Template System

Templates live in `.elasticclaw/templates/<name>/` in any repo.
Files written to the agent's workspace on bootstrap:
- `elasticclaw-config.yaml` — provider config
- `SOUL.md`, `AGENTS.md`, `TOOLS.md`, `IDENTITY.md`, `USER.md`, `MEMORY.md`

The hub reads these files from the DB (`template_files` JSON column) and writes
them to `~/.openclaw/workspace/` via SSH after the bridge connects.

## Debugging Tips

- Bridge not connecting? Check `~/claw-bridge.log`
- Gateway not responding? Check `~/openclaw-gateway.log`
- GitHub auth not working? The credential helper calls the hub at `$ELASTICCLAW_HUB_URL/api/github/token/$ELASTICCLAW_CLAW_ID`
- Hub unreachable? If relay is configured, check relay connection first
