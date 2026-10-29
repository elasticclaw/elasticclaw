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

## Optional observability stack

For a hub running on the host, start the separate telemetry stack from the repository root:

```bash
docker compose -f docker/compose.observability.yml up -d
```

Grafana listens on host port 3001 by default so it does not clash with the Next.js dev server; set `GRAFANA_PORT` to change it. The collector exposes OTLP/HTTP on `127.0.0.1:4318` and OTLP/gRPC on `127.0.0.1:4317`; storage backends remain internal to the Compose network.

Add this block to the host hub's `hub.yaml` and restart the hub:

```yaml
observability:
  enabled: true
  otlp_endpoint: http://localhost:4318
  service_name: elasticclaw-hub
  environment: local
  traces_sample_ratio: 1.0
  metrics_interval: 15s
```

Observability is disabled when this block is absent or `enabled` is false. Standard `OTEL_EXPORTER_OTLP_*` environment variables override exporter settings. The hub continues serving if the collector is unavailable. For a hub inside Docker Desktop, use `http://host.docker.internal:4318` with an appropriate reachable collector binding instead; the default loopback binding is intended for a host process.

Open [Grafana](http://localhost:3001/grafana/), sign in with `admin` / `admin`, and set a password when prompted. In **Explore**, select:

- **VictoriaMetrics** for Go runtime, HTTP, streaming connection, and host metrics. Metrics use Prometheus naming (dots become underscores); use the metric browser to find `http_server_request_duration_seconds` and `system_filesystem_inodes_usage`.
- **VictoriaLogs** for hub messages; start with `*` and narrow the time range or filter `log.component`.
- **Tempo** for traces; search for the `elasticclaw-hub` service. A span's logs link queries VictoriaLogs by trace ID. Correlation requires a structured log emitted with the span context; legacy `log.Printf` messages have no trace context.

Metrics are retained for 15 days, logs for 7 days, and traces for 72 hours in named volumes. The collector mounts the host filesystem read-only to collect CPU, memory, filesystem (including inodes), disk, network, and load metrics. On Docker Desktop these represent its Linux VM, not native macOS/Windows host metrics; native host collection needs a collector running on that host.

```bash
docker compose -f docker/compose.observability.yml logs -f collector
docker compose -f docker/compose.observability.yml down
```

`down` preserves data; adding `--volumes` deletes it. The first Grafana startup requires internet access to install the VictoriaLogs datasource plugin. The stack is a single-node development setup; backend configuration lives under `pkg/observability/stack/` and is shared with the native production installer. Compose supplies the container-specific endpoints and `/hostfs`; the native installer supplies loopback endpoints and `/`.


## Production install

On the hub machine, run as root on Linux with systemd (`amd64` or `arm64`):

```bash
sudo elasticclaw hub observability install --environment production
sudo systemctl restart elasticclaw
sudo elasticclaw hub observability status
```

The installer downloads checksum-verified, pinned OSS releases: OpenTelemetry Collector Contrib 0.164.0,
VictoriaMetrics v1.155.0, VictoriaLogs v1.53.0, Tempo 3.1.0, and Grafana OSS 12.4.3.
It installs the VictoriaLogs datasource plugin 0.33.1 through Grafana's bundled CLI.
Installation needs internet access to the release hosts and Grafana plugin registry; Docker is not required.
Prerequisites include `systemctl`, `ss` (iproute2), `useradd`, and `chown`.

Binaries live in `/opt/elasticclaw-observability/<component>-<version>/` with stable
`<component>/current` symlinks; configs live in `/etc/elasticclaw-observability/` and data in
`/var/lib/elasticclaw-observability/`. Re-running the installer renders configs again,
upgrades to the pinned versions, and restarts the stack. Services run as the dedicated
`elasticclaw-obs` system user with systemd hardening. The hub is restarted only with
`--restart-hub`; otherwise the installer prints `sudo systemctl restart elasticclaw`.

The installer preserves the existing hub config and enables local OTLP export with a 0.25 trace
sample ratio if none was configured. `--environment` sets the telemetry environment.
`elasticclaw hub init --observability` also writes an enabled block, but does not install backends.

| Component | Loopback listeners | Retention / disk safety | MemoryMax |
|---|---|---|---|
| Collector | 4317 gRPC, 4318 HTTP, 13133 health | Batched, memory-limited export | 512 MiB |
| VictoriaMetrics | 8428 HTTP | 15 days; stop ingestion below 2 GB free | 512 MiB |
| VictoriaLogs | 9428 HTTP | 7 days; 4 GiB disk cap | 512 MiB |
| Tempo | 3200 HTTP, 4417 OTLP gRPC, 9095 internal gRPC, 7946 memberlist | 72 hours | 768 MiB |
| Grafana | 3001 HTTP | Persistent dashboards and settings | 384 MiB |

The total MemoryMax budget is about 2.7 GiB; typical use is about 1 GiB (workload dependent).
Preflight requires at least 1.5 GiB available RAM and 12 GiB free on the data filesystem.
`--force` overrides these resource checks only; platform, root, systemd, and occupied-port checks remain enforced.

Grafana uses `<hub.yaml public_url>/grafana/`; `--public-url https://hub.example.com` overrides it.
The URL must be a site root (no path), because Caddy routes `/grafana*` at the root.
Without either, the URL is `http://localhost:3001/grafana/`. An ElasticClaw-managed Caddyfile
is updated and reloaded automatically, including on subsequent `hub caddy install` runs.
For a manually maintained Caddyfile, add this before the hub proxy inside the site block:

```caddyfile
reverse_proxy /grafana* localhost:3001
```

All native listeners bind loopback. Expose Grafana through Caddy's HTTPS `/grafana` route,
which keeps its own login; hub authentication does not authenticate Grafana. Sign in as `admin`
with the random password printed once at first install and saved in
`/etc/elasticclaw-observability/grafana-admin-password` (0600). Reinstalling preserves it.
Anonymous access, sign-up, analytics, update checks, plugin preinstalls, and Tempo usage
reporting are disabled. Changing the password inside Grafana does not update the installer
password file; that file records the initial credential.

```bash
sudo elasticclaw hub observability uninstall
sudo systemctl restart elasticclaw
# To delete telemetry and Grafana data too:
sudo elasticclaw hub observability uninstall --purge-data
```

Uninstall disables the hub observability block and removes managed Caddy routing, units,
binaries, and configs. Data is kept unless `--purge-data` is supplied, including a root-only
credential recovery copy under `/var/lib/elasticclaw-observability/.credentials/` so reinstalling
against the existing Grafana database does not generate an unusable new login.

HTTP telemetry treats incoming trace contexts as links to new server traces. WebSockets and
requests accepting `text/event-stream` bypass HTTP latency histograms and spans; monitor
`elasticclaw.hub.streaming.connections` with `endpoint` (route pattern) and
`protocol` (`websocket` or `sse`) instead. Only client HTTP metrics retain `server.address`;
server metric labels stay bounded. Outbound client spans strip query strings and credentials
from `url.full` before export while preserving scheme, host, and path.
