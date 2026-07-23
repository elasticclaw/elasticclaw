# Contributing to ElasticClaw

## Development Setup

ElasticClaw has two main components: **ElasticClaw Server** (Go) and the **web UI** (Next.js). In development you run them separately.

### Prerequisites

- Go 1.23+ (`go version`)
- Node.js 22+ and npm (`node --version`, `npm --version`)
- [oras](https://oras.land/docs/installation) — for pushing the claw-bridge OCI artifact
- [nix](https://nixos.org/download) (optional) — the `flake.nix` provides Go, shellcheck, and Node in a reproducible shell

If using nix:
```bash
nix develop  # drops you into a shell with all deps
```

### Clone and build

```bash
git clone https://github.com/elasticclaw/elasticclaw
cd elasticclaw
```

### Dev workflow

**Terminal 1 — ElasticClaw Server (Go only, fast rebuild):**
```bash
# First time: build and push the claw-bridge binary
make build-bridge-linux
oras push ttl.sh/marc/claw-bridge:1w \
  bin/claw-bridge-linux-amd64:application/octet-stream

# Start ElasticClaw Server (no embedded web UI in dev)
make build && ./bin/elasticclaw hub --no-web-ui
```

ElasticClaw Server listens on `:8080`. The `--no-web-ui` flag tells it not to serve the embedded web UI so the Next.js dev server handles that instead.

**Terminal 2 — Web UI (Next.js dev server):**
```bash
cd web
cp .env.example .env.local
# Edit .env.local:
#   NEXT_PUBLIC_HUB_URL=http://localhost:8080
npm install
npm run dev
```

Open `http://localhost:3000`. Login password is `admin` by default (or whatever `ui_password:` is in your `hub.yaml`).

### Server config

On first run, ElasticClaw Server creates `~/.elasticclaw/hub.yaml`. You can also create it manually:

```yaml
# ~/.elasticclaw/hub.yaml
token: mytoken          # user API token
claw_token: myclawtoken # agent auth token
ui_password: mypassword # web UI login password
address: :8080

# Optional: configure providers in Settings UI after first login
```

Start ElasticClaw Server with token flags to auto-create the tenant:
```bash
./bin/elasticclaw hub --token mytoken --claw-token myclawtoken --no-web-ui
```

### Iterating on Go

```bash
make build        # fast, Go only
go test ./...     # run all tests
go build ./...    # verify it compiles
```

No need to rebuild the web UI for Go changes.

### Iterating on the web UI

Just save files. Next.js hot reloads automatically. No ElasticClaw Server restart needed.

### Building a release binary (with embedded web UI)

```bash
make build-release
# → builds web/out/ then embeds it in bin/elasticclaw via -tags embedweb
```

Test the full binary:
```bash
rm -rf ~/.elasticclaw/hub.db
./bin/elasticclaw hub
# visit http://localhost:8080
```

### Deploying a custom build to a test server

To test a PR branch or local changes on a remote Linux amd64 server that already has ElasticClaw installed:

1. Build a release binary with embedded web UI. The `Version` does not matter for the build, but a non-release version (e.g. `pr-541`) has no matching `claw-bridge` release on GitHub, so you must override `bridge_image` in the next step.

```bash
make build-web
CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -tags embedweb \
  -ldflags "-X github.com/elasticclaw/elasticclaw/cmd.Version=pr-XXX -X github.com/elasticclaw/elasticclaw/cmd.Commit=$(git rev-parse --short HEAD)" \
  -o /tmp/elasticclaw-linux-amd64 .
```

2. Back up the existing server binary and deploy the new one. Replace `ssh://user@host:port` with your server.

```bash
ssh -p 22 user@host 'sudo cp /usr/local/bin/elasticclaw /usr/local/bin/elasticclaw.backup && sudo systemctl stop elasticclaw'
scp -P 22 /tmp/elasticclaw-linux-amd64 user@host:/tmp/elasticclaw-new
ssh -p 22 user@host 'sudo mv /tmp/elasticclaw-new /usr/local/bin/elasticclaw && sudo chmod +x /usr/local/bin/elasticclaw'
```

3. Pin `bridge_image` in `/root/.elasticclaw/hub.yaml` to an existing release so the bootstrapped claws can download a real `claw-bridge` binary. Use the latest release tag from GitHub, for example:

```yaml
bridge_image: https://github.com/elasticclaw/elasticclaw/releases/download/2026.7.21/claw-bridge-linux-amd64
```

4. Restart the service and verify:

```bash
ssh -p 22 user@host 'sudo systemctl restart elasticclaw && /usr/local/bin/elasticclaw version && sudo systemctl is-active elasticclaw'
```

If the server is accessed via a domain, also confirm the web UI is reachable with `curl -I https://your-domain.com/login`.

To roll back:

```bash
ssh -p 22 user@host 'sudo mv /usr/local/bin/elasticclaw.backup /usr/local/bin/elasticclaw && sudo systemctl restart elasticclaw'
```

### Testing

```bash
make test                # Go unit tests
make test-bootstrap      # bootstrap script tests (needs shellcheck)
make test-install        # container integration test (needs Docker)
ELASTICCLAW_INSTALL_TESTS=1 make test-install  # actually spins Ubuntu container
```

### Working on the claw-bridge

The claw-bridge runs on each VM and connects back to ElasticClaw Server. After changes:

```bash
make build-bridge-linux
oras push ttl.sh/marc/claw-bridge:1w \
  bin/claw-bridge-linux-amd64:application/octet-stream
```

Set `bridge_image: ttl.sh/marc/claw-bridge:1w` in hub.yaml to use your dev bridge.

### Architecture quick reference

```
cmd/                    CLI entry points
cmd/claw-bridge/        claw-bridge binary (runs on each VM)
pkg/hub/server.go       ElasticClaw Server HTTP/WS server
pkg/hub/bootstrap.go    Bootstrap script generation (pure functions)
pkg/hub/settings.go     Settings API (GET/PATCH /api/settings)
pkg/install/scripts.go  Install script generation (pure functions)
pkg/types/              Shared types
pkg/config/             Config loading/saving
pkg/provider/           Provider implementations (Replicated, Daytona, Vercel)
web/                    Next.js web UI (static export in prod)
internal/webui/         Go embed package for the compiled web UI
.elasticclaw/workspaces/ Workspace definitions
```

### CI

CI runs on Depot (not GitHub Actions). Workflows in `.depot/workflows/`:
- `test.yaml` — runs on every PR (unit tests, shellcheck, container tests)
- `release.yaml` — runs on tag push (Go binaries → GitHub Releases)
- `release-web.yaml` — runs on tag push (Docker image, now deprecated)

### Commit style

Conventional commits: `feat:`, `fix:`, `chore:`, `docs:`, `test:`

PRs for anything non-trivial. Direct push to main for obvious fixes.

### Common mistakes

- `make build-release` fails? Make sure `npm` is in PATH. In nix shell, it's provided. Otherwise install Node.js.
- Web UI shows blank page after `make build-release`? Likely `//go:embed all:out` issue — check that `internal/webui/out/_next/` exists after the build.
- ElasticClaw Server says "web UI not built"? You need `make build-release`, not `make build`.
- Tests fail after renaming something? Update `pkg/install/scripts_test.go` and `container_test.go`.
