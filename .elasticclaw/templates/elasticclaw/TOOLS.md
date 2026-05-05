# TOOLS.md - Environment Notes

## Repo

The `elasticclaw/elasticclaw` repo is cloned into your workspace.

```bash
alias ws="cd ~/.openclaw/workspace/elasticclaw"
cd ~/.openclaw/workspace/elasticclaw
nix develop   # required for Go / Node / npm / ORAS; see below
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

## Toolchain: Nix flake only

**Nix** and **Docker** are pre-installed on this VM. The repo has a **`flake.nix`** at the root; Go, Node, npm, ORAS, shellcheck, and the rest of the dev toolchain are pinned there.

**Do not** install Go, Node, npm, ORAS, or other dev tools with curl, apt, brew, nvm, or manual tarballs. Use the flake.

```bash
cd ~/.openclaw/workspace/elasticclaw
nix develop
```

Stay in that shell for `make`, `go`, `npm`, `oras`, and tests. That gives you the **exact** versions CI and the project expect.

**Docker** (pre-installed): required for `make test-container` and `make test-install`. Use `docker version` from the VM when you need to confirm the daemon.

**ORAS** (from the flake): required when publishing a dev `claw-bridge` OCI artifact — run `oras version` inside `nix develop`.

## OpenClaw

OpenClaw is running on this VM at `http://localhost:18789`.
Don't restart it — it's what you're running inside.

Gateway logs: `~/openclaw-gateway.log`

## claw-bridge

claw-bridge is running in the background connecting this VM to the hub.
Logs: `~/claw-bridge.log`

Don't kill it — you'll lose your connection to the hub.

## Build Commands

Run these **inside** `nix develop` (repo root):

```bash
make build          # Go only, fast (no web UI — dev iteration)
make build-release  # Full: web UI + Go binary (requires npm)
make build-web      # npm build → copy to internal/webui/out/
make test           # Go unit tests
make test-bootstrap # Bootstrap script tests
make test-factory   # Factory integration tests (hub package, integration tag)
make test-container # Bootstrap container run test (requires Docker)
make test-install   # Container integration test for install scripts
```

## Local Development (run hub + web together)

Use two terminals. In **each**, enter the dev shell first:

```bash
cd ~/.openclaw/workspace/elasticclaw
nix develop
```

Terminal 1 (hub API on `:8080`):
```bash
make build
./bin/elasticclaw hub --no-web-ui
```

Terminal 2 (Next.js UI on `:3000`) — still inside `nix develop` (cwd is repo root):
```bash
cd web
cp .env.example .env.local
# set NEXT_PUBLIC_HUB_URL=http://localhost:8080
npm install
npm run dev
```

Then open `http://localhost:3000`.

## Release Build Verification

From repo root inside `nix develop`:

```bash
make build-release
rm -rf ~/.elasticclaw/hub.db
./bin/elasticclaw hub
# open http://localhost:8080 and verify embedded UI loads
```

## Working on claw-bridge

After changing `cmd/claw-bridge/` (inside `nix develop`):

```bash
make build-bridge-linux
oras push ttl.sh/<your-handle>/claw-bridge:1w \
  bin/claw-bridge-linux-amd64:application/octet-stream
```

Set `bridge_image:` in `~/.elasticclaw/hub.yaml` to your pushed image for hub testing.

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
   - Nix + Docker available; OpenClaw, claw-bridge
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
