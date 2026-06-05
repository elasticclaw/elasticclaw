# Local dev environment

Runs the full ElasticClaw stack locally — hub, web UI, Ollama, and real coding agents — without any cloud provider. Agents run as Docker containers on your machine.

## Prerequisites

- **Docker Desktop** (or Docker Engine) running
- **`make`** — already installed on macOS via Xcode Command Line Tools (`xcode-select --install`)
- No Go or Node.js required on the host; everything compiles inside containers

## One-time setup

```bash
cp docker/hub.dev.yaml.example docker/hub.dev.yaml
```

The example config defaults to the local Ollama service:

```yaml
default_model: ollama/qwen2.5-coder:1.5b
llm_keys:
  - name: local-ollama
    provider: ollama          # → OLLAMA_API_KEY env var
    api_key: "ollama-local"   # placeholder for local Ollama
    default: true
```

The file is gitignored and will never be committed. To use a stronger external model while testing, add an external key entry, mark it `default: true`, set `local-ollama` to `default: false`, and change `default_model` to the matching provider/model.

The default local Ollama agent profile is intentionally conservative for Docker dev: it uses OpenClaw lean mode and an explicit Ollama runtime context for the small default model. That keeps local message smoke tests stable on ordinary laptops. Use an external provider entry when you need stronger tool-heavy coding-agent behavior or prompt tuning against a production-like model.

## Start the environment

```bash
make dev
```

This builds the agent image (`elasticclaw/claw-agent:dev`) and starts:
- **hub** on `http://localhost:8080` — Go server with hot reload via `air`
- **web** on `http://localhost:3000` — Next.js dev server with fast refresh
- **ollama** on `http://localhost:11434` — local LLM service available to hub and agents as `http://ollama:11434`

Pull the default local model once:

```bash
make dev-ollama-pull
```

To use a different local model:

```bash
make dev-ollama-pull MODEL=llama3.2:3b
```

Open `http://localhost:3000` and log in with password `devpass`.

## Spawn a local agent

```bash
make dev-claw
```

This creates a claw using the `docker` provider. The hub starts a sibling container with `elasticclaw/claw-agent:dev`, which bootstraps OpenClaw and connects back to the hub. The agent appears in the dashboard in a few seconds.

You can also trigger agents via workflows configured with `provider: docker` in `hub.dev.yaml`.

## Useful commands

| Command | Description |
|---------|-------------|
| `make dev` | Build agent image + start hub + web + Ollama (foreground) |
| `make dev-up-d` | Start in background (detached) |
| `make dev-ollama-pull` | Pull the local Ollama model, override with `MODEL=model:tag` |
| `make dev-logs` | Follow logs from hub, web, and Ollama |
| `make dev-down` | Stop containers (DB preserved) |
| `make dev-reset` | Stop + delete all volumes (clean slate) |
| `make dev-restart` | Restart just the hub |
| `make dev-sh-hub` | Shell into the hub container |
| `make dev-agent-build` | Rebuild the agent image after code changes |
| `make dev-claw` | Spawn a one-off local agent |

## Architecture notes

**Docker-out-of-Docker**: the hub container has the Docker socket mounted (`/var/run/docker.sock`), so it can spawn sibling agent containers. Agent containers join the `elasticclaw-dev` network and reach the hub at `http://hub:8080` (the `public_url` in `hub.dev.yaml`) and Ollama at `http://ollama:11434`.

**Local vs external models**: Ollama is optional but available by default for cheap local testing. External API providers still work by editing `docker/hub.dev.yaml`; use them when you need stronger model behavior or want to tune prompts against a production-like model.

**Browser → hub**: the web UI at `:3000` calls the hub at `localhost:8080` directly (the hub has permissive CORS). The `NEXT_PUBLIC_HUB_URL` points to `localhost:8080`, not the internal Docker hostname.

**Persistence**: the SQLite DB lives in the `hub-data` volume. Ollama model data lives in the `ollama-data` volume. `make dev-down` preserves both; `make dev-reset` wipes everything.

**Hot reload Go**: edit any `.go` file and `air` recompiles the hub automatically (watch the logs). Hot reload for the web UI is handled by Next.js fast refresh.

## Limitations

This environment is for local development only. For end-to-end testing with real issue trackers and PRs you still need a public URL (see `make e2e` in the root Makefile). Do not use this compose file in production.
