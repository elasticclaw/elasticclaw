# Contributing to ElasticClaw

## Development Setup

### Prerequisites

- Go 1.21+
- [oras](https://oras.land/docs/installation) — for pushing the bridge binary to an OCI registry
- A [Replicated CMX](https://replicated.com) account and API token

### Running Locally

Pull, build, push the bridge binary, and start the hub in one shot:

```bash
rm -rf ~/.elasticclaw/hub.db && git pull origin main && \
  make build && \
  make build-bridge-linux && \
  oras push ttl.sh/marc/claw-bridge:1w \
    bin/claw-bridge-linux-amd64:application/octet-stream && \
  ./bin/elasticclaw hub --claw-token token --token token
```

This:
1. Wipes the local DB so you start fresh
2. Pulls latest code
3. Builds the `elasticclaw` CLI binary → `bin/elasticclaw`
4. Cross-compiles `claw-bridge` for Linux amd64 → `bin/claw-bridge-linux-amd64`
5. Pushes the bridge binary to `ttl.sh` (free, expires in 1 week — replace with your own registry for persistent builds)
6. Starts the hub on `localhost:8080` with both tokens set to `token`

### Hub Config

The hub reads `~/.elasticclaw/hub.yaml` on startup. Minimal config for local dev:

```yaml
url: http://localhost:8080
public_url: https://your-ngrok-or-tunnel.app  # URL claws use to reach the hub
token: token
claw_token: token

default_model: anthropic/claude-sonnet-4-6

llm_keys:
  anthropic: sk-ant-...

providers:
  replicated:
    token: your-replicated-api-token

# Optional: GitHub App for per-claw git credentials
github:
  - app_id: 123456
    private_key_pem: |
      -----BEGIN RSA PRIVATE KEY-----
      ...
      -----END RSA PRIVATE KEY-----
```

### Creating a Claw

```bash
elasticclaw login http://localhost:8080 --token token
elasticclaw create --name my-claw --template my-template
```

### Web UI

The web UI lives in [elasticclaw/elasticclaw-web](https://github.com/elasticclaw/elasticclaw-web).

```bash
cd elasticclaw-web
NEXT_PUBLIC_HUB_URL=http://localhost:8080 \
NEXT_PUBLIC_HUB_TOKEN=token \
pnpm dev
```

## Project Structure

```
cmd/                  CLI commands (hub, create, chat, kill, etc.)
cmd/claw-bridge/      claw-bridge binary — runs on VMs, connects to hub
pkg/hub/              Hub server (WebSocket, REST API, bootstrap)
pkg/config/           Config loading
pkg/provider/         VM providers (Replicated CMX, local)
pkg/types/            Shared types
```

## Making Changes

- Hub + CLI changes: `make build`
- Bridge changes: `make build-bridge-linux` + push to ttl.sh (or set `bridge_image` in hub.yaml to an OCI ref)
- To test end-to-end with a real VM, set `bridge_image` in hub.yaml to your OCI ref and create a claw

## Pull Requests

PRs welcome. Keep commits focused; use conventional commit messages (`feat:`, `fix:`, `chore:`, etc.).
