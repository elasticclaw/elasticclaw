# TOOLS.md - Environment Notes

## Repo

The `elasticclaw/elasticclaw` repo is cloned into your workspace.
Check: `ls ~/.openclaw/workspace/` or `ls ~/`

```bash
cd ~/.openclaw/workspace/elasticclaw
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

Go is NOT pre-installed by default. Use nix if available, otherwise install:
```bash
curl -fsSL https://go.dev/dl/go1.23.4.linux-amd64.tar.gz | sudo tar -C /usr/local -xz
export PATH=$PATH:/usr/local/go/bin
echo 'export PATH=$PATH:/usr/local/go/bin' >> ~/.bashrc
```

Or if nix is enabled: it's already there via the flake.

Check: `go version`

## Node.js

Node 24 is pre-installed: `node --version`, `npm --version`

```bash
cd ~/.openclaw/workspace/elasticclaw/web
npm install
npm run dev   # dev server on :3000 (requires NEXT_PUBLIC_HUB_URL)
```

## OpenClaw

OpenClaw is running on this VM at `http://localhost:18789`.
Don't restart it — it's what you're running inside.

Gateway logs: `~/openclaw-gateway.log`

## claw-bridge

claw-bridge is running in the background connecting this VM to the hub.
Logs: `~/claw-bridge.log`

Don't kill it — you'll lose your connection.

## Build Commands

```bash
make build          # Go only, fast (dev iteration)
make build-release  # Full: web UI + Go binary (requires npm)
make build-dev      # Alias for make build
make build-web      # npm build → copy to internal/webui/out/
make test           # Go unit tests
make test-bootstrap # Bootstrap script tests
make test-install   # Container integration test for install scripts
```

## Architecture: How This VM Got Here

1. User ran `elasticclaw create --template elasticclaw` from the hub CLI
2. Hub provisioned a Replicated CMX VM (r1.large)
3. Hub SSHed in and ran the bootstrap script:
   - Installed Node 24, OpenClaw, claw-bridge
   - Configured OpenClaw with model + gateway password
   - Started gateway (port 18789) + bridge (WS to hub)
   - Installed git credential helper (fetches GitHub App tokens)
   - Cloned this repo into workspace
4. Bridge registered with hub → status became "online"
5. Hub sent intro message → you replied

## Single Binary Architecture (important!)

The web UI is **embedded in the Go binary** via `//go:embed all:out`.

Key files:
- `internal/webui/embed.go` — embed directive (build tag: `embedweb`)
- `internal/webui/embed_stub.go` — stub when not embedded
- `internal/webui/out/` — built Next.js static export (gitignored, populated by `make build-web`)

`make build` does NOT embed web (uses stub). `make build-release` embeds it.

## Hub Config Paths

- System: `/etc/elasticclaw/hub.yaml` + `/var/lib/elasticclaw/`
- Dev: `~/.elasticclaw/hub.yaml` + `~/.elasticclaw/`

## Install Command

```bash
elasticclaw install \
  --server ssh://root@ip \
  --domain hub.example.com \
  --anthropic-key sk-ant-... \
  [--version v0.0.9] \
  [--skip-caddy]
```

Or manual on the server:
```bash
sudo elasticclaw hub service install
sudo elasticclaw hub caddy install --domain hub.example.com
```

## Debugging Tips

- Bridge not connecting? Check `~/claw-bridge.log`
- Gateway not responding? Check `~/openclaw-gateway.log`
- GitHub auth not working? Credential helper calls hub at `$ELASTICCLAW_HUB_URL/api/github/token/$ELASTICCLAW_CLAW_ID`
- Web UI 404? Did you `make build-release`? Check `internal/webui/out/` has `index.html`
- White page? Missing `_next/` in embedded files — ensure `//go:embed all:out` (not `//go:embed out`)
- 301 redirect loop? Don't use `http.FileServer` for embedded files, use `fs.ReadFile`
