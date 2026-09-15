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

### Retention

The hub accumulates checkpoint manifests, content-addressed blobs, captured
diagnostics logs, messages and task run events, and nothing removed any of it
until the retention sweeper. It is **opt-in**: with no `retention` section the
hub reclaims nothing, which is the state every existing hub is already in.

Every phase deletes irreversibly, and what a 90-day window actually removes
depends on the shape of one hub's history — not on the config. Arm it in this
order:

1. **Enable with `dry_run: true`.** A dry run selects exactly what a real cycle
   would remove, including the blob sweep, and logs it without removing a file
   or a row. `enabled: true` is still required: `dry_run` describes how a cycle
   behaves, not whether it runs.

   A dry run does write one thing: the `checkpoint_blob_refs` backfill, which
   records which blobs each checkpoint holds. It runs at the top of every cycle
   and only ever adds protection: it selects the `ready` and `skipped` rows that
   have no reference edge and gives them edges, and the blob sweep — dry or
   real — refuses to delete anything while any such row exists. Running it is
   what makes the dry run's blob numbers mean anything. Nothing else is
   written: in particular a dry run does not build the two retention indexes
   (a real cycle builds them at its end, from the space it just freed), so it
   is safe on a hub with no free space at all.

   The backfill, like every other phase, is bounded by the per-cycle budget.
   On a hub with years of checkpoints the first cycle may stop it partway and
   say so (`stopped at the cycle budget`); the sweep then reports
   `blobs=DECLINED(...)` for that cycle and the next cycle continues. That is
   the budget working, not a fault.

   **Rolling back.** A build without reference counting records no edges, so
   every checkpoint created while it runs is `ready` with a valid manifest and
   nothing in `checkpoint_blob_refs`. That is safe only because the backfill is
   continuous: on the roll-forward, the first cycle finds those rows, declines
   the sweep (`blobs=DECLINED(N checkpoint(s) without references)` in the cycle
   line), gives them edges, and the following cycle sweeps normally. Two things
   defeat it: deleting rows from `checkpoint_blob_refs` by hand, and a build in
   which the backfill is gated on a one-time marker. Do neither.

   ```yaml
   retention:
     enabled: true
     dry_run: true
     interval: 1h        # minimum 10m
     max_age: 2160h      # 90d; floor 168h
     compact_after: 240h # 10d; floor 24h, and always kept below max_age
   ```

   **Every knob here needs a hub restart to take effect, including
   `enabled` and `dry_run`.** The retention section is read once at boot and
   never reloaded — not from an edit to `hub.yaml`, and not from the settings
   or AI-config pages, which rewrite `hub.yaml` and hot-reload everything
   else. Changing a value and waiting for the next cycle changes nothing; the
   sweeper keeps running the policy it booted with and logs, once per change,
   `configuration changed since boot: configured [...], running [...]` so the
   drift is visible. This is deliberate: the first cycle always runs one
   `interval` after boot, and that window is the only grace an irreversible
   `dry_run: false` gets.

   Durations are Go durations: `h`, `m`, `s` — **there is no `d`**. `max_age: 30d`
   does not parse, and the hub falls back to the 90-day default. It says so in
   the startup line's `adjustments=[...]`, which is worth reading before walking
   away.

2. **Read the cycle log.** The sweeper logs its effective policy once at startup
   (including any value it clamped and when the first cycle runs), then a start
   and a done line per cycle:

   ```
   [retention] enabled: dry_run=true interval=1h0m0s max_age=2160h0m0s compact_after=240h0m0s adjustments=[none] first_cycle_at=...
   [retention] blob reference backfill in 18.2s: 1204 of 1204 checkpoint(s) given references, 3598 reference(s), 331 tree expansion(s) (1002114 rows), 0 schema-1 fallback(s); unparseable_manifests=3 missing_trees=0 unparseable_trees=0 ...
   [retention] dry_run: would sweep 41029 blob(s) (e.g. 0a1b2c3d, ..., and 41024 more) totalling 93112884213 bytes
   [retention] cycle done in 4.1s (dry_run=true): would remove compacted=812 diagnostics=39 checkpoints=1204 task_run_events=2911430 messages=88214 blobs=41029 bytes_freed=93112884213 (blobs=93110... manifests=1... diagnostics=...; row deletes free SQLite pages but do not shrink the database file without a VACUUM) phase_errors=0 item_errors=0
   ```

   `phase_errors` counts phases that failed outright; `item_errors` counts
   individual files or rows a phase could not process and stepped over. A cycle
   with `item_errors` in the thousands is not a cycle that found nothing. Each
   phase logs its first five item errors with the error text, then one line
   saying how many more there were and repeating the first — so a filesystem
   that refuses every unlink produces six lines, not forty thousand, and the
   `cycle done` line survives journald's rate limit.

   A disabled sweeper logs `cycle skipped: retention disabled` on every tick.
   Silence is what a wedged sweeper looks like; a disabled one says so.

   `bytes_freed` is filesystem bytes only — blobs, manifests and diagnostics
   logs. Deleting `messages` and `task_run_events` rows returns pages to
   SQLite's freelist for reuse; **the `hub.db` file does not get smaller without
   a `VACUUM`**, which is a separate, offline decision.

   A dry run aggregates: one line per phase with a total and a handful of
   examples, not one line per item. The earlier per-item logging produced tens of
   thousands of lines per cycle, which journald rate-limits — dropping the
   summary line that is the whole point.

   `unparseable_manifests` in the backfill line is expected on a hub that hit
   ENOSPC and is not an error: the row's own digests are still recorded.
   `missing_trees` is worth attention — a checkpoint whose tree blob is already
   gone cannot have its per-file blobs recorded, and those files will be swept.
   That checkpoint was already unrestorable. A `schema-1 fallback` is a
   checkpoint whose tree could not be expanded at all and whose old-style
   manifest still listed its files; those files are recorded under the
   checkpoint itself, one edge each.

   A cycle that says `blobs=DECLINED(...)` swept nothing because some `ready`
   or `skipped` checkpoint still has no reference edge — the line before it
   names a few — and the backfill error above it says why they could not be
   given one. Until the count reaches zero no blob is deleted; that is the
   design, not a fault.

3. **Archive anything you want to keep.** The counts from step 2 are the last
   warning you get.

4. **Arm it**: set `dry_run: false`. The first real cycle runs one `interval`
   after the restart — never during startup — so there is always a window to
   turn it back off.

The first sweep on a hub that has never run retention has a large backlog.
Every phase that writes in bulk — the backfill, compaction, checkpoint expiry,
the row deletes and the tree reference gc — is batched, releases the SQLite
write lock between batches, and stops at a per-cycle time budget with a
`stopping after N item(s), cycle budget ... reached` line, so the backlog is
spread over several cycles rather than holding the single write lock for
hours; this is expected and needs no intervention.

### Commit style

Conventional commits: `feat:`, `fix:`, `chore:`, `docs:`, `test:`

PRs for anything non-trivial. Direct push to main for obvious fixes.

### Common mistakes

- `make build-release` fails? Make sure `npm` is in PATH. In nix shell, it's provided. Otherwise install Node.js.
- Web UI shows blank page after `make build-release`? Likely `//go:embed all:out` issue — check that `internal/webui/out/_next/` exists after the build.
- ElasticClaw Server says "web UI not built"? You need `make build-release`, not `make build`.
- Tests fail after renaming something? Update `pkg/install/scripts_test.go` and `container_test.go`.
